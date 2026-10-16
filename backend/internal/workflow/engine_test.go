package workflow

// engine_test.go 调度/执行/重摇/预算/熔断/人审的引擎级测试（M1/M2 验收面）。
// 用内存 store + 假生成工具 + 假评委/向量器，不依赖 PG/Redis/真实 LLM。

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/subagent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// encodePNG 往文件写一张测试 PNG（alpha=false 全不透明，编码为无透明通道的真彩）。
func encodePNG(f *os.File, w, h int, alpha bool) error {
	var img image.Image
	if alpha {
		m := image.NewNRGBA(image.Rect(0, 0, w, h))
		for i := 0; i+3 < len(m.Pix); i += 4 {
			m.Pix[i+3] = 128
		}
		img = m
	} else {
		m := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i+3 < len(m.Pix); i += 4 {
			m.Pix[i+3] = 255
		}
		img = m
	}
	return png.Encode(f, img)
}

// ---------------------------------------------------------------------------
// 内存 Store

type memStore struct {
	mu    sync.Mutex
	runs  map[string]*Run
	units map[string]*Unit
}

func newMemStore() *memStore {
	return &memStore{runs: map[string]*Run{}, units: map[string]*Unit{}}
}

func (m *memStore) copyRun(r *Run) *Run {
	c := *r
	c.Roster = append([]RosterItem(nil), r.Roster...)
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		c.FinishedAt = &t
	}
	return &c
}

func (m *memStore) CreateRun(_ context.Context, run *Run, units []*Unit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = m.copyRun(run)
	for _, u := range units {
		c := *u
		m.units[u.ID] = &c
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
	return m.copyRun(r), nil
}

func (m *memStore) ListRuns(_ context.Context, f RunFilter) ([]*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Run
	for _, r := range m.runs {
		if f.SessionID != "" && r.SessionID != f.SessionID {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, m.copyRun(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *memStore) UpdateRunStatus(_ context.Context, id, from, to, pauseReason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok || r.Status != from {
		return false, nil
	}
	r.Status = to
	r.PauseReason = pauseReason
	r.UpdatedAt = time.Now()
	switch {
	case IsRunTerminal(to):
		t := time.Now()
		r.FinishedAt = &t
	case to == RunStatusRunning:
		r.FinishedAt = nil
	}
	return true, nil
}

func (m *memStore) AddRunSpent(_ context.Context, id string, delta int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return 0, ErrRunNotFound
	}
	r.SpentEstimate += delta
	return r.SpentEstimate, nil
}

func (m *memStore) RaiseRunBudget(_ context.Context, id string, extra int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return 0, ErrRunNotFound
	}
	r.BudgetCap += extra
	return r.BudgetCap, nil
}

func (m *memStore) ListUnits(_ context.Context, runID string) ([]*Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Unit
	for _, u := range m.units {
		if u.RunID == runID {
			c := *u
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memStore) GetUnit(_ context.Context, id string) (*Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.units[id]
	if !ok {
		return nil, ErrUnitNotFound
	}
	c := *u
	return &c, nil
}

func (m *memStore) LatestUnit(_ context.Context, runID, itemID, stage string) (*Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *Unit
	for _, u := range m.units {
		if u.RunID != runID || u.ItemID != itemID || u.Stage != stage {
			continue
		}
		if best == nil || u.AttemptNo > best.AttemptNo {
			best = u
		}
	}
	if best == nil {
		return nil, nil
	}
	c := *best
	return &c, nil
}

func (m *memStore) CreateUnit(_ context.Context, u *Unit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *u
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	m.units[u.ID] = &c
	return nil
}

func (m *memStore) TransitionUnit(_ context.Context, id string, from []string, to string, patch UnitPatch) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.units[id]
	if !ok {
		return false, nil
	}
	matched := false
	for _, f := range from {
		if u.Status == f {
			matched = true
			break
		}
	}
	if !matched {
		return false, nil
	}
	u.Status = to
	u.UpdatedAt = time.Now()
	if patch.ArtifactPath != nil {
		u.ArtifactPath = *patch.ArtifactPath
	}
	if patch.CheckResult != nil {
		u.CheckResult = *patch.CheckResult
	}
	if patch.RejectReason != nil {
		u.RejectReason = *patch.RejectReason
	}
	if patch.ItemName != "" {
		u.ItemName = patch.ItemName
	}
	return true, nil
}

func (m *memStore) UnitStatusCounts(_ context.Context, runID string) (map[string]int, error) {
	units, _ := m.ListUnits(context.Background(), runID)
	out := map[string]int{}
	for _, u := range units {
		out[u.Status]++
	}
	return out, nil
}

func (m *memStore) ResetRunningUnits(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, u := range m.units {
		if u.Status == UnitStatusRunning {
			u.Status = UnitStatusPending
			n++
		}
	}
	return n, nil
}

func (m *memStore) RunningRunIDs(_ context.Context) ([]string, error) {
	return m.runIDsByStatus(RunStatusRunning), nil
}

func (m *memStore) PendingRunIDs(_ context.Context, limit int) ([]string, error) {
	ids := m.runIDsByStatus(RunStatusPending)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (m *memStore) runIDsByStatus(status string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Run
	for _, r := range m.runs {
		if r.Status == status {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	ids := make([]string, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// 假依赖

type fakeCall struct {
	tool string
	args map[string]any
}

type fakeTools struct {
	mu    sync.Mutex
	calls []fakeCall
	fn    func(call int, args map[string]any) *tool.Result
}

func (f *fakeTools) Dispatch(_ context.Context, name string, args map[string]any) (*tool.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{tool: name, args: args})
	callNo := len(f.calls)
	f.mu.Unlock()
	return f.fn(callNo, args), nil
}

func (f *fakeTools) MountPreApprovedForScope(_, _ string, names []string) []string { return names }

func (f *fakeTools) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeModels struct {
	fn func(prompt string) (string, error)
}

func (f *fakeModels) CallLightweightWithRetry(_ context.Context, prompt string) (string, error) {
	if f.fn == nil {
		return `{"items":[]}`, nil
	}
	return f.fn(prompt)
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, 8)
	for i, r := range text {
		v[i%8] += float32(r%17) / 17
	}
	return v, nil
}

func (fakeEmbedder) Dim() int { return 8 }

// writeOK 假生成工具：把合法产物写到 save_as（128x64 = 2 格 × 64x64，无 alpha）。
func writeOK(workdir string) func(call int, args map[string]any) *tool.Result {
	return func(_ int, args map[string]any) *tool.Result {
		p, _ := args["save_as"].(string)
		abs := filepath.Join(workdir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		f, err := os.Create(abs)
		if err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		defer f.Close()
		if err := encodePNG(f, 128, 64, false); err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		return &tool.Result{Tool: "fake_gen", Success: true, Output: fmt.Sprintf(`{"path":%q}`, p)}
	}
}

// ---------------------------------------------------------------------------
// 夹具

const engineRecipe = `
id: test_recipe
version: 1
title: 引擎测试配方
tool: fake__image_generate
deliverable:
  type: sprite_sheet
  frames: 2
  cell: 64x64
  format: png
  path: "units/{{id}}/{{stage}}/"
stages:
  - { id: concept, anchor_out: true }
  - { id: walk, anchor_in: [concept] }
invariants: 测试不变量
slots: [id, name, element]
params: { reroll: 1 }
check:
  program: ["exists", "path_template", "format", "dimensions"]
`

type engineHarness struct {
	eng     *Engine
	store   *memStore
	tools   *fakeTools
	workdir string
	events  *eventCapture
}

type eventCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (e *eventCapture) emit(_, text, _ string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msgs = append(e.msgs, text)
}

func newHarness(t *testing.T, recipeYAML string, toolFn func(call int, args map[string]any) *tool.Result, models LightModel) *engineHarness {
	t.Helper()
	workdir := t.TempDir()
	if _, err := SaveRecipeYAML(workdir, recipeYAML); err != nil {
		t.Fatalf("写配方失败: %v", err)
	}
	store := newMemStore()
	tools := &fakeTools{fn: toolFn}
	if models == nil {
		models = &fakeModels{}
	}
	eng := NewEngine(Config{MaxActiveRuns: 4, DefaultConcurrency: 2, BudgetCapDefault: 100, JudgeTimeout: 3 * time.Second}, store, tools, models)
	eng.WithEmbedder(fakeEmbedder{})
	events := &eventCapture{}
	eng.WithEmitter(events.emit)
	t.Cleanup(eng.Stop)
	return &engineHarness{eng: eng, store: store, tools: tools, workdir: workdir, events: events}
}

func (h *engineHarness) createRun(t *testing.T, roster string, mutate func(*CreateSpec)) *Run {
	t.Helper()
	spec := CreateSpec{
		SessionID: "session-1", Workdir: h.workdir, RecipeID: "test_recipe", RosterRaw: roster,
	}
	if mutate != nil {
		mutate(&spec)
	}
	run, _, err := h.eng.CreateRun(context.Background(), spec)
	if err != nil {
		t.Fatalf("建 run 失败: %v", err)
	}
	return run
}

func (h *engineHarness) waitRun(t *testing.T, runID string, want func(*Run) bool, d time.Duration) *Run {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		r, err := h.store.GetRun(context.Background(), runID)
		if err == nil && want(r) {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := h.store.GetRun(context.Background(), runID)
	t.Fatalf("等待 run 状态超时: status=%s spent=%d", r.Status, r.SpentEstimate)
	return nil
}

func latestUnitOf(t *testing.T, h *engineHarness, runID, item, stage string) *Unit {
	t.Helper()
	u, err := h.store.LatestUnit(context.Background(), runID, item, stage)
	if err != nil || u == nil {
		t.Fatalf("取 unit 失败: %v", err)
	}
	return u
}

// ---------------------------------------------------------------------------
// 用例

// 采纳全链路：件内阶段串行 + 锚点传播（walk 的 reference = 该件 concept 采纳件）+ run done。
func TestEngineRunAdoptsAndPropagatesAnchors(t *testing.T) {
	h := newHarness(t, engineRecipe, nil, nil)
	h.tools.fn = writeOK(h.workdir)
	run := h.createRun(t, `[{"id":"ice_dragon","name":"冰霜巨龙","element":"ice"}]`, nil)
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	if got.Status != RunStatusDone {
		t.Fatalf("run 应为 done: %s", got.Status)
	}
	concept := latestUnitOf(t, h, run.ID, "ice_dragon", "concept")
	walk := latestUnitOf(t, h, run.ID, "ice_dragon", "walk")
	if concept.Status != UnitStatusDone || walk.Status != UnitStatusDone {
		t.Fatalf("两阶段都应 done: %s / %s", concept.Status, walk.Status)
	}
	if concept.ArtifactPath != "units/ice_dragon/concept/ice_dragon_concept.png" {
		t.Fatalf("产物路径不符: %s", concept.ArtifactPath)
	}
	// 锚点传播：walk 的生成参数 reference 应指向 concept 采纳件。
	h.tools.mu.Lock()
	defer h.tools.mu.Unlock()
	if len(h.tools.calls) != 2 {
		t.Fatalf("应恰好 2 次生成: %d", len(h.tools.calls))
	}
	if ref, _ := h.tools.calls[1].args["reference"].(string); ref != concept.ArtifactPath {
		t.Fatalf("walk 应带 concept 锚点, got %q want %q", ref, concept.ArtifactPath)
	}
	if walk.CheckResult == "" || !strings.Contains(walk.CheckResult, `"program"`) {
		t.Fatalf("check_result 应落库: %s", walk.CheckResult)
	}
	// 事件账本：应包含创建 + 各阶段迁移 + 完成。
	h.events.mu.Lock()
	joined := strings.Join(h.events.msgs, "\n")
	h.events.mu.Unlock()
	for _, want := range []string{"已创建", "concept", "walk", "done"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("事件流缺 %q:\n%s", want, joined)
		}
	}
}

// 重摇直到耗尽：产物存在但校验不过 → needs_review（attempt 行只标不删，重摇产物带 _a2）。
func TestEngineRerollThenNeedsReview(t *testing.T) {
	h := newHarness(t, engineRecipe, nil, nil)
	// 每次产出错误尺寸（128x128 ≠ 2 格×64）。
	h.tools.fn = func(_ int, args map[string]any) *tool.Result {
		p, _ := args["save_as"].(string)
		abs := filepath.Join(h.workdir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(abs), 0o755)
		f, err := os.Create(abs)
		if err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		defer f.Close()
		if err := encodePNG(f, 128, 128, false); err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		return &tool.Result{Success: true, Output: fmt.Sprintf(`{"path":%q}`, p)}
	}
	run := h.createRun(t, `[{"id":"a","stages":["concept"]}]`, nil)
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	if got.Status != RunStatusDone { // 全部 unit 终态（needs_review 也是终态）→ run done
		t.Fatalf("run 应为 done（含待审单元）: %s", got.Status)
	}
	u := latestUnitOf(t, h, run.ID, "a", "concept")
	if u.Status != UnitStatusNeedsReview || u.AttemptNo != 2 {
		t.Fatalf("应 needs_review@attempt2: %s@%d", u.Status, u.AttemptNo)
	}
	// reroll limit 1 → 共 2 次生成，第二次产物带 _a2 后缀（不覆盖第一次）。
	if h.tools.callCount() != 2 {
		t.Fatalf("应重摇一次共 2 调: %d", h.tools.callCount())
	}
	h.tools.mu.Lock()
	second := h.tools.calls[1].args["save_as"].(string)
	h.tools.mu.Unlock()
	if !strings.HasSuffix(second, "_a2.png") {
		t.Fatalf("重摇产物应带 _a2: %s", second)
	}
	// 失败轨迹只标不删：attempt1 行仍可查（failed）。
	units, _ := h.store.ListUnits(context.Background(), run.ID)
	if len(units) != 2 || units[0].Status != UnitStatusFailed {
		t.Fatalf("attempt1 应保留为 failed: %+v", units)
	}
}

// 预算闸：cap=1 单并发 → 第一个 cell 结算后 paused(budget_exceeded)；续跑需提额。
func TestEngineBudgetPauseAndResumeWithTopUp(t *testing.T) {
	recipe := strings.Replace(engineRecipe, "params: { reroll: 1 }", "params: { reroll: 1, concurrency: 1 }", 1)
	h := newHarness(t, recipe, nil, nil)
	h.tools.fn = writeOK(h.workdir)
	run := h.createRun(t, `[{"id":"a"},{"id":"b"}]`, func(spec *CreateSpec) { spec.BudgetCap = 1 })
	paused := h.waitRun(t, run.ID, func(r *Run) bool { return r.Status == RunStatusPaused }, 10*time.Second)
	if paused.PauseReason != PauseBudgetExceeded {
		t.Fatalf("暂停原因应为 budget_exceeded: %s", paused.PauseReason)
	}
	// 未提额续跑 → 拒绝。
	if err := h.eng.ResumeRun(context.Background(), run.ID, 0); err == nil {
		t.Fatalf("预算暂停未提额应拒绝续跑")
	}
	// 提额续跑 → 完成。
	if err := h.eng.ResumeRun(context.Background(), run.ID, 10); err != nil {
		t.Fatalf("提额续跑失败: %v", err)
	}
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	if got.Status != RunStatusDone {
		t.Fatalf("续跑后应 done: %s", got.Status)
	}
	for _, item := range []string{"a", "b"} {
		if u := latestUnitOf(t, h, run.ID, item, "concept"); u.Status != UnitStatusDone {
			t.Fatalf("%s 应 done: %s", item, u.Status)
		}
	}
}

// 早期熔断：前 5 个终态单元失败 ≥3 → run failed(circuit_breaker)。
func TestEngineCircuitBreaker(t *testing.T) {
	h := newHarness(t, engineRecipe, nil, nil)
	h.tools.fn = func(_ int, _ map[string]any) *tool.Result {
		return &tool.Result{Success: false, Error: "生成服务不可用"}
	}
	roster := `[{"id":"a","stages":["concept"]},{"id":"b","stages":["concept"]},{"id":"c","stages":["concept"]},{"id":"d","stages":["concept"]},{"id":"e","stages":["concept"]}]`
	run := h.createRun(t, roster, nil)
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 15*time.Second)
	if got.Status != RunStatusFailed || got.PauseReason != PauseCircuitBreaker {
		t.Fatalf("应熔断为 failed(circuit_breaker): %s/%s", got.Status, got.PauseReason)
	}
}

// 人审：reroll 复活（run done → running → 重跑新 attempt）；reject 需理由。
func TestEngineUnitActionsRerollAndReject(t *testing.T) {
	h := newHarness(t, engineRecipe, nil, nil)
	failDims := func(_ int, args map[string]any) *tool.Result {
		p, _ := args["save_as"].(string)
		abs := filepath.Join(h.workdir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(abs), 0o755)
		f, err := os.Create(abs)
		if err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		defer f.Close()
		_ = encodePNG(f, 128, 128, false)
		return &tool.Result{Success: true, Output: fmt.Sprintf(`{"path":%q}`, p)}
	}
	h.tools.fn = failDims
	run := h.createRun(t, `[{"id":"a","stages":["concept"]}]`, nil)
	h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	u := latestUnitOf(t, h, run.ID, "a", "concept")
	if u.Status != UnitStatusNeedsReview {
		t.Fatalf("前置应为 needs_review: %s", u.Status)
	}
	// reject 必带理由。
	if err := h.eng.UnitAction(context.Background(), u.ID, UnitActionReject, ""); err == nil {
		t.Fatalf("reject 无理由应拒绝")
	}
	// reroll：换成正确尺寸后复活续跑 → done。
	h.tools.fn = writeOK(h.workdir)
	if err := h.eng.UnitAction(context.Background(), u.ID, UnitActionReroll, ""); err != nil {
		t.Fatalf("reroll 失败: %v", err)
	}
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	if got.Status != RunStatusDone {
		t.Fatalf("reroll 续跑后应 done: %s", got.Status)
	}
	concept := latestUnitOf(t, h, run.ID, "a", "concept")
	if concept.Status != UnitStatusDone {
		t.Fatalf("reroll 后应 done: %s", concept.Status)
	}
}

// 人审 accept：needs_review → done。
func TestEngineUnitActionAccept(t *testing.T) {
	h := newHarness(t, engineRecipe, nil, nil)
	h.tools.fn = func(_ int, args map[string]any) *tool.Result {
		p, _ := args["save_as"].(string)
		abs := filepath.Join(h.workdir, filepath.FromSlash(p))
		_ = os.MkdirAll(filepath.Dir(abs), 0o755)
		f, err := os.Create(abs)
		if err != nil {
			return &tool.Result{Success: false, Error: err.Error()}
		}
		defer f.Close()
		_ = encodePNG(f, 128, 128, false)
		return &tool.Result{Success: true, Output: fmt.Sprintf(`{"path":%q}`, p)}
	}
	run := h.createRun(t, `[{"id":"a","stages":["concept"]}]`, nil)
	h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	u := latestUnitOf(t, h, run.ID, "a", "concept")
	if err := h.eng.UnitAction(context.Background(), u.ID, UnitActionAccept, ""); err != nil {
		t.Fatalf("accept 失败: %v", err)
	}
	after := latestUnitOf(t, h, run.ID, "a", "concept")
	if after.Status != UnitStatusDone {
		t.Fatalf("accept 后应 done: %s", after.Status)
	}
}

// 运行中操作：pause 在边界生效、abort 终态、pause 后不可重复 pause。
func TestEnginePauseAbortControls(t *testing.T) {
	recipe := strings.Replace(engineRecipe, "params: { reroll: 1 }", "params: { reroll: 1, concurrency: 1 }", 1)
	h := newHarness(t, recipe, nil, nil)
	block := make(chan struct{})
	h.tools.fn = func(_ int, args map[string]any) *tool.Result {
		<-block // 阻塞第一个生成调用
		return writeOK(h.workdir)(0, args)
	}
	run := h.createRun(t, `[{"id":"a"},{"id":"b"}]`, nil)
	// 等第一个 unit 进入 running。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		units, _ := h.store.ListUnits(context.Background(), run.ID)
		running := false
		for _, u := range units {
			if u.Status == UnitStatusRunning {
				running = true
			}
		}
		if running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.eng.PauseRun(context.Background(), run.ID); got != nil {
		t.Fatalf("pause 失败: %v", got)
	}
	close(block) // 放行在途调用
	// 在途 unit 完成后在边界停下：run 停在 paused，第二个 unit 未开始。
	paused := h.waitRun(t, run.ID, func(r *Run) bool {
		units, _ := h.store.ListUnits(context.Background(), r.ID)
		for _, u := range units {
			if u.Status == UnitStatusRunning {
				return false
			}
		}
		return r.Status == RunStatusPaused
	}, 10*time.Second)
	if paused.PauseReason != PauseUser {
		t.Fatalf("暂停原因应为 user: %s", paused.PauseReason)
	}
	// 件间并发下 Goroutine 顺序不定（先跑哪件都合法）：不变量 = 恰有一个 cell 完成、
	// 其余停在 pending（边界生效，没有第二个 cell 悄悄开跑）。
	units, _ := h.store.ListUnits(context.Background(), run.ID)
	doneCells, pendingCells := 0, 0
	for _, u := range units {
		switch u.Status {
		case UnitStatusDone:
			doneCells++
		case UnitStatusPending:
			pendingCells++
		}
	}
	if doneCells != 1 || pendingCells != len(units)-1 {
		for _, u := range units {
			t.Logf("unit %s/%s#%d = %s (%s)", u.ItemID, u.Stage, u.AttemptNo, u.Status, u.RejectReason)
		}
		t.Fatalf("暂停边界应恰停在一个完成 cell：done=%d pending=%d", doneCells, pendingCells)
	}
	if err := h.eng.PauseRun(context.Background(), run.ID); err == nil {
		t.Fatalf("paused 状态重复 pause 应拒绝")
	}
	if err := h.eng.AbortRun(context.Background(), run.ID); err != nil {
		t.Fatalf("abort 失败: %v", err)
	}
	got := h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 5*time.Second)
	if got.Status != RunStatusAborted {
		t.Fatalf("应 aborted: %s", got.Status)
	}
}

// 升级通道：重摇耗尽 + 配方声明 escalate → 派发真子 Agent（假 Escalator 记录）。
func TestEngineEscalation(t *testing.T) {
	recipe := engineRecipe + "\nescalate: { role: ui_assistant, domain: 界面设计 }\n"
	h := newHarness(t, recipe, nil, nil)
	h.tools.fn = func(_ int, _ map[string]any) *tool.Result {
		return &tool.Result{Success: false, Error: "生成失败"}
	}
	esc := &fakeEscalator{}
	h.eng.WithEscalator(esc)
	run := h.createRun(t, `[{"id":"a","stages":["concept"]}]`, nil)
	h.waitRun(t, run.ID, func(r *Run) bool { return IsRunTerminal(r.Status) }, 10*time.Second)
	esc.mu.Lock()
	defer esc.mu.Unlock()
	if len(esc.specs) != 1 || esc.specs[0] != "ui_assistant" {
		t.Fatalf("应派发 1 次升级: %+v", esc.specs)
	}
	u := latestUnitOf(t, h, run.ID, "a", "concept")
	if !strings.Contains(u.CheckResult, "upgrade") {
		t.Fatalf("check_result 应记录升级: %s", u.CheckResult)
	}
}

type fakeEscalator struct {
	mu    sync.Mutex
	specs []string
}

func (f *fakeEscalator) DispatchProgrammatic(_ context.Context, spec subagent.ProgrammaticSpec) (string, *tool.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec.RoleID)
	return "session-1/ui_assistant-9", nil
}
