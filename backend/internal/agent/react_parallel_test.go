// react_parallel_test.go 覆盖 TODO 第9项① 轮内并行工具执行：
//   - 并行派发墙钟 ≈ 最慢一个工具（非串行累加），峰值并发 = 调用数；
//   - 结果按模型给定顺序回填，ToolCallID 配对不乱序；
//   - 同路径写类工具互斥（峰值并发 = 1），两个调用都完成；
//   - ErrLoopExit 在回填段按原序命中即终止（无 tool 消息入史）。
package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/tools"
)

// concTracker 共享并发追踪器：记录同时在执行的工具 goroutine 峰值。
type concTracker struct {
	inFlight atomic.Int64
	maxSeen  atomic.Int64
}

func (tr *concTracker) enter() {
	cur := tr.inFlight.Add(1)
	for {
		m := tr.maxSeen.Load()
		if cur <= m || tr.maxSeen.CompareAndSwap(m, cur) {
			return
		}
	}
}

func (tr *concTracker) exit() { tr.inFlight.Add(-1) }

// slowTool 假工具：睡眠指定时长后返回成功，进出时上报共享并发追踪器。
type slowTool struct {
	name    string
	sleep   time.Duration
	tracker *concTracker
	calls   atomic.Int64
}

func (s *slowTool) Name() string      { return s.name }
func (s *slowTool) Aliases() []string { return nil }
func (s *slowTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	s.calls.Add(1)
	s.tracker.enter()
	time.Sleep(s.sleep)
	s.tracker.exit()
	return &tool.Result{Tool: s.name, Success: true, Output: s.name + " done"}
}

// parallelLoopConfig 返回开启并行的 LoopConfig（独立 bool 变量避免取址共享）。
func parallelLoopConfig() LoopConfig {
	enabled := true
	return LoopConfig{ToolParallelEnabled: &enabled, ToolParallelMaxConcurrency: 4}
}

// threeProbeCalls 构造一轮 3 个探查工具调用的 mock LLM 响应。
func threeProbeCalls() []*blades.Message {
	return []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{ID: "p1", Name: "probe_alpha", Request: "{}"},
				blades.ToolPart{ID: "p2", Name: "probe_beta", Request: "{}"},
				blades.ToolPart{ID: "p3", Name: "probe_gamma", Request: "{}"},
			},
		},
		blades.AssistantMessage("done"),
	}
}

func registerProbes(reg *tool.Registry, tr *concTracker) (*slowTool, *slowTool, *slowTool) {
	alpha := &slowTool{name: "probe_alpha", sleep: 400 * time.Millisecond, tracker: tr}
	beta := &slowTool{name: "probe_beta", sleep: 400 * time.Millisecond, tracker: tr}
	gamma := &slowTool{name: "probe_gamma", sleep: 400 * time.Millisecond, tracker: tr}
	reg.Register(alpha)
	reg.Register(beta)
	reg.Register(gamma)
	return alpha, beta, gamma
}

// TestReActAgent_ParallelToolCalls_WallClock 三个 400ms 工具并行执行：墙钟远小于
// 串行和（1.2s），峰值并发 = 3，结果按原序回填且 ToolCallID 配对正确。
func TestReActAgent_ParallelToolCalls_WallClock(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	tr := &concTracker{}
	alpha, beta, gamma := registerProbes(reg, tr)
	llm := &mockModelProvider{responses: threeProbeCalls()}
	ag := NewReActAgent("par", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithWorkDir(dir).
		WithLoopConfig(parallelLoopConfig())

	start := time.Now()
	res, err := ag.Run(context.Background(), "probe")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed >= 1100*time.Millisecond {
		t.Fatalf("parallel dispatch should take ~one tool duration (400ms), took %v", elapsed)
	}
	if got := tr.maxSeen.Load(); got != 3 {
		t.Fatalf("peak concurrency = %d, want 3 (true parallel)", got)
	}
	for _, s := range []*slowTool{alpha, beta, gamma} {
		if s.calls.Load() != 1 {
			t.Fatalf("tool %s called %d times, want 1", s.name, s.calls.Load())
		}
	}
	// 结果按原序回填：tool 消息顺序 = 调用顺序，ToolCallID 配对。
	var toolMsgs []ReactMessage
	for _, m := range res.History {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 3 {
		t.Fatalf("history has %d tool messages, want 3", len(toolMsgs))
	}
	wantIDs := []string{"p1", "p2", "p3"}
	wantOuts := []string{"probe_alpha done", "probe_beta done", "probe_gamma done"}
	for i := range toolMsgs {
		if toolMsgs[i].ToolCallID != wantIDs[i] {
			t.Fatalf("tool msg %d ToolCallID = %q, want %q", i, toolMsgs[i].ToolCallID, wantIDs[i])
		}
		if !strings.Contains(toolMsgs[i].Content, wantOuts[i]) {
			t.Fatalf("tool msg %d should contain %q, got: %s", i, wantOuts[i], toolMsgs[i].Content)
		}
	}
}

// TestReActAgent_SerialFallback 关闭并行开关时退回串行：墙钟 >= 三个工具之和。
func TestReActAgent_SerialFallback(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	tr := &concTracker{}
	registerProbes(reg, tr)
	llm := &mockModelProvider{responses: threeProbeCalls()}
	ag := NewReActAgent("ser", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithWorkDir(dir) // 不注入 ToolParallelEnabled：零值=关闭

	start := time.Now()
	if _, err := ag.Run(context.Background(), "probe"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 1150*time.Millisecond {
		t.Fatalf("serial dispatch should take >= 1.2s (3 x 400ms), took %v", elapsed)
	}
	if got := tr.maxSeen.Load(); got != 1 {
		t.Fatalf("serial dispatch peak concurrency = %d, want 1", got)
	}
}

// shadowWriteTool 顶替 WriteFile 的假写工具：记录调用数并上报并发追踪器。
type shadowWriteTool struct {
	sleep   time.Duration
	tracker *concTracker
	calls   atomic.Int64
}

func (s *shadowWriteTool) Name() string      { return "WriteFile" }
func (s *shadowWriteTool) Aliases() []string { return nil }
func (s *shadowWriteTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	s.calls.Add(1)
	s.tracker.enter()
	time.Sleep(s.sleep)
	s.tracker.exit()
	return &tool.Result{Tool: "WriteFile", Success: true, Output: "written"}
}

// TestReActAgent_ParallelSamePathWriteMutex 同轮两个 WriteFile 同路径：互斥下峰值
// 并发 = 1，两个调用都完成（无竞写交错）。
func TestReActAgent_ParallelSamePathWriteMutex(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	tr := &concTracker{}
	w := &shadowWriteTool{sleep: 300 * time.Millisecond, tracker: tr}
	reg.Register(w) // 顶替内置 WriteFile（同名覆盖）

	llm := &mockModelProvider{responses: []*blades.Message{
		{
			Role: blades.RoleAssistant,
			Parts: []blades.Part{
				blades.ToolPart{ID: "w1", Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "same.txt", "content": "A"}))},
				blades.ToolPart{ID: "w2", Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "same.txt", "content": "B"}))},
			},
		},
		blades.AssistantMessage("done"),
	}}
	ag := NewReActAgent("w", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithWorkDir(dir).
		WithLoopConfig(parallelLoopConfig())

	if _, err := ag.Run(context.Background(), "write same file twice"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls := w.calls.Load(); calls != 2 {
		t.Fatalf("both writes must complete, got %d calls", calls)
	}
	if got := tr.maxSeen.Load(); got != 1 {
		t.Fatalf("same-path writes must serialize (peak concurrency = %d, want 1)", got)
	}
}

// exitOnRegistry 包装注册表：指定工具返回 ErrLoopExit（模拟循环守卫命中）。
type exitOnRegistry struct {
	inner  ToolRegistry
	exitOn string
}

func (r *exitOnRegistry) Schema() []tools.Tool { return r.inner.Schema() }
func (r *exitOnRegistry) Dispatch(ctx context.Context, call ToolCall) (ToolResult, error) {
	if call.Name == r.exitOn {
		return ToolResult{}, tool.ErrLoopExit
	}
	return r.inner.Dispatch(ctx, call)
}

// TestReActAgent_ParallelErrLoopExit 回填段首个 ErrLoopExit 按原序命中即终止：
// 返回哨兵错误，且无任何 tool 消息入史（首个结果即守卫命中）。
func TestReActAgent_ParallelErrLoopExit(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	tr := &concTracker{}
	registerProbes(reg, tr)
	llm := &mockModelProvider{responses: threeProbeCalls()}
	inner := NewToolRegistryAdapter(reg)
	ag := NewReActAgent("exit", types.RoleDefinition{SystemPrompt: "t"}, llm, &exitOnRegistry{inner: inner, exitOn: "probe_alpha"}).
		WithWorkDir(dir).
		WithLoopConfig(parallelLoopConfig())

	res, err := ag.Run(context.Background(), "probe with guard hit")
	if !errors.Is(err, tool.ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit, got: %v", err)
	}
	for _, m := range res.History {
		if m.Role == "tool" {
			t.Fatalf("no tool message should enter history after ErrLoopExit, got: %s", m.Content)
		}
	}
}
