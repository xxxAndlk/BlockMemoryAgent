package flow

// engine_test.go 定义校验 + 图执行（branch/approval/tool/变量解析）的引擎级测试。
// 内存 store + 假模型 + 假工具，不依赖 PG/真实 LLM；走真实异步调度路径（CreateRun →
// Kick → walk goroutine），测试用轮询等终态，不绕过调度。

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/model"
)

// ---------------------------------------------------------------------------
// 内存 Store

type memStore struct {
	mu       sync.Mutex
	runs     map[string]*Run
	nodeRuns map[string]*NodeRun
}

func newMemStore() *memStore {
	return &memStore{runs: map[string]*Run{}, nodeRuns: map[string]*NodeRun{}}
}

func (m *memStore) CreateRun(_ context.Context, run *Run, nrs []*NodeRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *run
	m.runs[run.ID] = &c
	for _, nr := range nrs {
		nc := *nr
		m.nodeRuns[nr.ID] = &nc
	}
	return nil
}

func (m *memStore) GetRun(_ context.Context, id string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrRunNotFound
	}
	c := *r
	return &c, nil
}

func (m *memStore) ListRuns(_ context.Context, f RunFilter) ([]*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Run
	for _, r := range m.runs {
		if f.FlowID != "" && r.FlowID != f.FlowID {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		c := *r
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *memStore) UpdateRunStatus(_ context.Context, id, from, to string, outputs map[string]any, errMsg string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != from {
		return false, nil
	}
	r.Status = to
	if outputs != nil {
		r.Outputs = outputs
	}
	if errMsg != "" {
		r.Error = errMsg
	}
	r.UpdatedAt = time.Now()
	if IsRunTerminal(to) {
		t := time.Now()
		r.FinishedAt = &t
	}
	return true, nil
}

func (m *memStore) ListNodeRuns(_ context.Context, runID string) ([]*NodeRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*NodeRun{}
	for _, nr := range m.nodeRuns {
		if nr.RunID == runID {
			c := *nr
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (m *memStore) GetNodeRun(_ context.Context, id string) (*NodeRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nr, ok := m.nodeRuns[id]
	if !ok {
		return nil, ErrNodeRunNotFound
	}
	c := *nr
	return &c, nil
}

func (m *memStore) StartNodeRun(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nr, ok := m.nodeRuns[id]
	if !ok {
		return ErrNodeRunNotFound
	}
	if nr.Status == NodeStatusPending {
		nr.Status = NodeStatusRunning
		t := time.Now()
		nr.StartedAt = &t
	}
	return nil
}

func (m *memStore) FinishNodeRun(_ context.Context, id, status string, inputs, outputs map[string]any, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	nr, ok := m.nodeRuns[id]
	if !ok {
		return ErrNodeRunNotFound
	}
	nr.Status = status
	if inputs != nil {
		nr.Inputs = inputs
	}
	if outputs != nil {
		nr.Outputs = outputs
	}
	if errMsg != "" {
		nr.Error = errMsg
	}
	t := time.Now()
	nr.FinishedAt = &t
	return nil
}

func (m *memStore) ResetRunningNodes(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, nr := range m.nodeRuns {
		if nr.Status == NodeStatusRunning {
			nr.Status = NodeStatusPending
			nr.StartedAt = nil
			n++
		}
	}
	return n, nil
}

func (m *memStore) RunningRunIDs(_ context.Context) ([]string, error) {
	return m.runIDsBy(RunStatusRunning), nil
}

func (m *memStore) PendingRunIDs(_ context.Context, _ int) ([]string, error) {
	return m.runIDsBy(RunStatusPending), nil
}

func (m *memStore) runIDsBy(status string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{}
	for _, r := range m.runs {
		if r.Status == status {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 假模型 / 假工具

type fakeClient struct{ gen func(ctx context.Context, prompt string) (string, error) }

func (c fakeClient) Generate(ctx context.Context, prompt string) (string, error) {
	return c.gen(ctx, prompt)
}

// fakeModels 按提示词关键字返回不同 JSON（模拟模型按指令输出）。
type fakeModels struct{}

func (fakeModels) GetModel(_ context.Context, _ string) (model.LLMClient, error) {
	return fakeClient{gen: func(_ context.Context, _ string) (string, error) {
		return `{"category":"normal","confidence":0.9}`, nil
	}}, nil
}

func (fakeModels) GetLightweightModel(_ context.Context) (model.LLMClient, error) {
	return fakeClient{gen: func(_ context.Context, prompt string) (string, error) {
		if strings.Contains(prompt, "差评") {
			return `{"category":"sensitive"}`, nil
		}
		return `{"category":"normal"}`, nil
	}}, nil
}

// fakeTools 记录调用并返回固定结果。
type fakeTools struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (f *fakeTools) Dispatch(_ context.Context, _ string, args map[string]any) (*tool.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]any{}
	for k, v := range args {
		cp[k] = v
	}
	f.calls = append(f.calls, cp)
	return &tool.Result{Tool: "fake", Success: true, Output: "ok-out"}, nil
}

// ---------------------------------------------------------------------------
// 测试用 flow JSON

const flowClassify = `{
  "id": "review",
  "name": "审核",
  "nodes": [
    {"id": "start", "type": "start", "config": {"inputs": [{"key": "text", "type": "string", "required": true}]}},
    {"id": "classify", "type": "llm", "depends_on": ["start"],
     "config": {"prompt": "分类：{{start.text}}", "outputs": [{"key": "category", "type": "string"}]}},
    {"id": "gate", "type": "branch", "depends_on": ["classify"],
     "config": {"cases": [{"label": "敏感", "when": [{"left": "{{classify.category}}", "op": "==", "right": "sensitive"}], "next": "reject"}],
                "default": "pass"}},
    {"id": "pass", "type": "answer", "depends_on": ["gate"],
     "config": {"outputs": {"result": "通过：{{start.text}}"}}},
    {"id": "reject", "type": "answer", "depends_on": ["gate"],
     "config": {"outputs": {"result": "驳回（{{classify.category}}）"}}}
  ]
}`

const flowApproval = `{
  "id": "pub",
  "name": "发布审批",
  "nodes": [
    {"id": "start", "type": "start", "config": {"inputs": [{"key": "title", "type": "string", "required": true}]}},
    {"id": "ask", "type": "approval", "depends_on": ["start"],
     "config": {"question": "确认发布 {{start.title}}？"}},
    {"id": "out", "type": "answer", "depends_on": ["ask"],
     "config": {"outputs": {"result": "已发布：{{start.title}}（审批意见：{{ask.comment}}）"}}}
  ]
}`

const flowTool = `{
  "id": "touch",
  "name": "建文件",
  "nodes": [
    {"id": "start", "type": "start", "config": {"inputs": [{"key": "name", "type": "string", "required": true}]}},
    {"id": "mk", "type": "tool", "depends_on": ["start"],
     "config": {"tool": "WriteFile", "args": {"path": "{{start.name}}", "content": "hi"}}},
    {"id": "out", "type": "answer", "depends_on": ["mk"],
     "config": {"outputs": {"result": "{{mk.result}}", "p": "{{mk.path}}"}}}
  ]
}`

func writeFlow(t *testing.T, workdir, id, jsonText string) {
	t.Helper()
	if _, err := SaveFlowJSON(workdir, id, jsonText); err != nil {
		t.Fatalf("SaveFlowJSON: %v", err)
	}
}

// waitRun 轮询 run 到达任一目标状态（真实异步调度路径，3s 超时）。
func waitRun(t *testing.T, ms *memStore, id string, want ...string) *Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, err := ms.GetRun(context.Background(), id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		for _, w := range want {
			if r.Status == w {
				return r
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := ms.GetRun(context.Background(), id)
	t.Fatalf("run %s 等待超时：状态 %s（err=%s）", id, r.Status, r.Error)
	return nil
}

func nodeStatus(t *testing.T, ms *memStore, runID, nodeID string) string {
	t.Helper()
	nr, err := ms.GetNodeRun(context.Background(), nodeRunID(runID, nodeID))
	if err != nil {
		t.Fatalf("GetNodeRun %s: %v", nodeID, err)
	}
	return nr.Status
}

// ---------------------------------------------------------------------------
// 定义校验

func TestValidateRejectsBadDefs(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"无 start", `{"id":"f","nodes":[{"id":"a","type":"answer","config":{"outputs":{"r":"1"}}}]}`, "start"},
		{"无 answer", `{"id":"f","nodes":[{"id":"s","type":"start"}]}`, "answer"},
		{"依赖不存在", `{"id":"f","nodes":[{"id":"s","type":"start"},{"id":"a","type":"answer","depends_on":["ghost"],"config":{"outputs":{"r":"1"}}}]}`, "依赖不存在"},
		{"有环", `{"id":"f","nodes":[{"id":"s","type":"start"},{"id":"l1","type":"llm","depends_on":["l2"],"config":{"prompt":"{{l2.k}}","outputs":[{"key":"k"}]}},{"id":"l2","type":"llm","depends_on":["l1"],"config":{"prompt":"{{l1.k}}","outputs":[{"key":"k"}]}},{"id":"e","type":"answer","depends_on":["l1"],"config":{"outputs":{"r":"1"}}}]}`, "环"},
		{"引用未声明输出", `{"id":"f","nodes":[{"id":"s","type":"start"},{"id":"a","type":"llm","depends_on":["s"],"config":{"prompt":"{{s.ghost}}","outputs":[{"key":"k"}]}},{"id":"e","type":"answer","depends_on":["a"],"config":{"outputs":{"r":"1"}}}]}`, "未声明输出"},
		{"branch case 目标悬空", `{"id":"f","nodes":[{"id":"s","type":"start"},{"id":"b","type":"branch","depends_on":["s"],"config":{"cases":[{"label":"x","when":[{"left":"1","op":"==","right":"1"}],"next":"ghost"}],"default":"s"}},{"id":"e","type":"answer","depends_on":["s"],"config":{"outputs":{"r":"1"}}}]}`, "目标节点"},
		{"llm 无 prompt", `{"id":"f","nodes":[{"id":"s","type":"start"},{"id":"a","type":"llm","depends_on":["s"],"config":{"outputs":[{"key":"k"}]}},{"id":"e","type":"answer","depends_on":["a"],"config":{"outputs":{"r":"1"}}}]}`, "prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlow([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 引擎执行

func TestBranchNormalPath(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "review", flowClassify)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	run, err := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "review", Inputs: map[string]any{"text": "好评"}})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusDone)
	if got := stringify(r.Outputs["result"]); got != "通过：好评" {
		t.Fatalf("outputs.result = %q", got)
	}
	if s := nodeStatus(t, ms, run.ID, "reject"); s != NodeStatusSkipped {
		t.Fatalf("reject node = %s, want skipped", s)
	}
}

func TestBranchSensitivePath(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "review", flowClassify)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	run, err := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "review", Inputs: map[string]any{"text": "差评"}})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusDone)
	if got := stringify(r.Outputs["result"]); !strings.Contains(got, "驳回") {
		t.Fatalf("outputs.result = %q", got)
	}
	if s := nodeStatus(t, ms, run.ID, "pass"); s != NodeStatusSkipped {
		t.Fatalf("pass node = %s, want skipped", s)
	}
}

func TestApprovalFlow(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "pub", flowApproval)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	run, err := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "pub", Inputs: map[string]any{"title": "v1.2"}})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	waitRun(t, ms, run.ID, RunStatusWaitingApprove)
	if err := eng.DecideNode(context.Background(), run.ID, "ask", true, "可以发"); err != nil {
		t.Fatalf("DecideNode: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusDone)
	if got := stringify(r.Outputs["result"]); !strings.Contains(got, "已发布：v1.2") || !strings.Contains(got, "可以发") {
		t.Fatalf("outputs.result = %q", got)
	}
}

func TestApprovalRejectFailsRun(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "pub", flowApproval)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	run, _ := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "pub", Inputs: map[string]any{"title": "v1.3"}})
	waitRun(t, ms, run.ID, RunStatusWaitingApprove)
	if err := eng.DecideNode(context.Background(), run.ID, "ask", false, "先别发"); err != nil {
		t.Fatalf("DecideNode: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusFailed)
	if !strings.Contains(r.Error, "先别发") {
		t.Fatalf("run error = %q", r.Error)
	}
}

func TestToolNode(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "touch", flowTool)
	ms := newMemStore()
	ft := &fakeTools{}
	eng := NewEngine(Config{}, ms, ft, fakeModels{})
	run, err := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "touch", Inputs: map[string]any{"name": "a.txt"}})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusDone)
	if got := stringify(r.Outputs["result"]); got != "ok-out" {
		t.Fatalf("outputs.result = %q", got)
	}
	// 工具入参应已插值（{{start.name}} → a.txt）。
	if len(ft.calls) != 1 || ft.calls[0]["path"] != "a.txt" {
		t.Fatalf("tool calls = %v", ft.calls)
	}
	// 调试 trace：node_run 带解析后的实际入参。
	nr, _ := ms.GetNodeRun(context.Background(), nodeRunID(run.ID, "mk"))
	if stringify(nr.Inputs["args"].(map[string]any)["path"]) != "a.txt" {
		t.Fatalf("node inputs = %v", nr.Inputs)
	}
}

func TestMissingRequiredInput(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "review", flowClassify)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	_, err := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "review", Inputs: map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("want required-input error, got %v", err)
	}
}

func TestAbortRun(t *testing.T) {
	workdir := t.TempDir()
	writeFlow(t, workdir, "pub", flowApproval)
	ms := newMemStore()
	eng := NewEngine(Config{}, ms, &fakeTools{}, fakeModels{})
	run, _ := eng.CreateRun(context.Background(), CreateSpec{Workdir: workdir, FlowID: "pub", Inputs: map[string]any{"title": "x"}})
	waitRun(t, ms, run.ID, RunStatusWaitingApprove)
	if err := eng.AbortRun(context.Background(), run.ID); err != nil {
		t.Fatalf("AbortRun: %v", err)
	}
	r := waitRun(t, ms, run.ID, RunStatusAborted)
	_ = r
}

// ---------------------------------------------------------------------------
// 纯函数

func TestResolveVars(t *testing.T) {
	sc := scope{
		"a": {"items": []any{map[string]any{"name": "甲"}, map[string]any{"name": "乙"}}},
	}
	got, err := resolveString("名单：{{a.items.1.name}}", sc)
	if err != nil || got != "名单：乙" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := resolveString("{{ghost.x}}", sc); err == nil {
		t.Fatal("want error for undefined node ref")
	}
	if _, err := resolveString("{{a.nope}}", sc); err == nil {
		t.Fatal("want error for undefined key")
	}
}

func TestEvalCondition(t *testing.T) {
	sc := scope{"n": {"count": "5"}, "s": {"tag": "abc"}}
	cases := []struct {
		cond Condition
		want bool
	}{
		{Condition{Left: "{{n.count}}", Op: ">", Right: "3"}, true},
		{Condition{Left: "{{n.count}}", Op: "<=", Right: "3"}, false},
		{Condition{Left: "{{s.tag}}", Op: "contains", Right: "b"}, true},
		{Condition{Left: "{{s.tag}}", Op: "==", Right: "abc"}, true},
		{Condition{Left: "{{s.missing}}", Op: "exists"}, false},
		{Condition{Left: "{{s.missing}}", Op: "not_exists"}, true},
	}
	for i, tc := range cases {
		got, err := evalCondition(tc.cond, sc)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if got != tc.want {
			t.Fatalf("case %d: got %v want %v", i, got, tc.want)
		}
	}
}

func TestParseLLMJSON(t *testing.T) {
	if got := parseLLMJSON("前言 {\"a\":1} 后语"); got["a"] != float64(1) {
		t.Fatalf("got %v", got)
	}
	if got := parseLLMJSON("no json"); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
