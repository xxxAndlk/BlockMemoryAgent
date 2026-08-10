package agent

// engine_test.go 验证 TODO #29 派发执行模式三引擎：
//   - ReflectEngine：产出后对照验收标准自检，不达标带反馈重试；LLM 缺失/报错/非法 JSON fail-open；
//   - PlanExecuteEngine：先出步骤计划（planSink 落板）再逐步执行（历史累积）、超步数截断；
//     规划失败降级纯 ReAct；
//   - ReAct 默认引擎（省略 mode）即裸 Run，行为零变化（由既有测试覆盖）。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// scriptedProvider 按调用顺序返回预设文本的模型提供者；超出脚本时返回固定尾串。
type scriptedProvider struct {
	mu      sync.Mutex
	replies []string
	calls   int
}

func (p *scriptedProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := "out of script"
	if p.calls < len(p.replies) {
		text = p.replies[p.calls]
	}
	p.calls++
	return &blades.ModelResponse{Message: blades.AssistantMessage(text)}, nil
}

func (p *scriptedProvider) callsCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// scriptedLLM 按调用顺序返回预设文本的 LLMComplete。
type scriptedLLM struct {
	mu      sync.Mutex
	replies []string
	errs    []error
	calls   int
}

func (l *scriptedLLM) Call(ctx context.Context, prompt string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.calls
	l.calls++
	if idx < len(l.errs) && l.errs[idx] != nil {
		return "", l.errs[idx]
	}
	if idx < len(l.replies) {
		return l.replies[idx], nil
	}
	return "", nil
}

func (l *scriptedLLM) callsCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// newEngineTestAgent 构造测试用 ReActAgent（无工具、无 mailbox）。
func newEngineTestAgent(t *testing.T, provider ModelProvider) *ReActAgent {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	return NewReActAgent("engine-test", types.RoleDefinition{SystemPrompt: "t"}, provider, NewToolRegistryAdapter(reg))
}

func reflectPass() string { return `{"pass": true, "feedback": ""}` }
func reflectFail(fb string) string {
	return `{"pass": false, "feedback": "` + fb + `"}`
}

// TestReflectEngine_RetriesWithFeedback 自检不达标带反馈重试，二次产出通过。
func TestReflectEngine_RetriesWithFeedback(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"v1 answer", "v2 answer"}}
	reflectLLM := &scriptedLLM{replies: []string{reflectFail("缺少验证证据"), reflectPass()}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{
		LLM:                 reflectLLM.Call,
		MaxReflectionRounds: 2,
	})
	result, err := eng.Run(context.Background(), "写一个排序函数，验收：时间复杂度 O(n log n)")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "v2 answer" {
		t.Fatalf("expected v2 answer after retry, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 2 {
		t.Fatalf("expected 2 agent runs (1 + 1 retry), got %d", got)
	}
	if got := reflectLLM.callsCount(); got != 2 {
		t.Fatalf("expected 2 reflect calls, got %d", got)
	}
}

// TestReflectEngine_PassFirstRound 首轮自检通过，仅一次 ReAct 运行。
func TestReflectEngine_PassFirstRound(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{replies: []string{reflectPass()}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected 1 agent run, got %d", got)
	}
}

// TestReflectEngine_MaxRoundsExhausted 自检持续不达标，到轮数上限后返回最后一次产出。
func TestReflectEngine_MaxRoundsExhausted(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"v1", "v2", "v3"}}
	reflectLLM := &scriptedLLM{replies: []string{reflectFail("a"), reflectFail("b")}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{
		LLM:                 reflectLLM.Call,
		MaxReflectionRounds: 2,
	})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "v3" {
		t.Fatalf("expected last attempt v3, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 3 {
		t.Fatalf("expected 3 agent runs (1 + 2 retries), got %d", got)
	}
}

// TestReflectEngine_NoLLMFallback LLM 缺失时零变化（裸 ReAct）。
func TestReflectEngine_NoLLMFallback(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected single agent run, got %d", got)
	}
}

// TestReflectEngine_LLMErrorFailOpen 自检 LLM 报错时按通过处理，不阻塞交付。
func TestReflectEngine_LLMErrorFailOpen(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{errs: []error{errors.New("llm down")}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected single agent run, got %d", got)
	}
}

// TestReflectEngine_InvalidJSONFailOpen 自检返回非法 JSON 时按通过处理。
func TestReflectEngine_InvalidJSONFailOpen(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{replies: []string{"乱七八糟的输出"}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
}

// captureEngineProvider 记录每次请求的全部消息文本。
type captureEngineProvider struct {
	mu      sync.Mutex
	replies []string
	texts   []string
}

func (p *captureEngineProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := "done"
	if len(p.replies) > 0 {
		text = p.replies[0]
	}
	var b strings.Builder
	for _, m := range req.Messages {
		if m != nil {
			b.WriteString(bladesText(m))
			b.WriteString("\n")
		}
	}
	p.texts = append(p.texts, b.String())
	p.replies = p.replies[1:]
	return &blades.ModelResponse{Message: blades.AssistantMessage(text)}, nil
}

// TestReflectEngine_RetryMessageInHistory 重试轮的用户指令含自检反馈。
func TestReflectEngine_RetryMessageInHistory(t *testing.T) {
	provider := &captureEngineProvider{replies: []string{"v1", "v2"}}
	reflectLLM := &scriptedLLM{replies: []string{reflectFail("缺验证证据"), reflectPass()}}
	eng := NewReflectEngine(newEngineTestAgent(t, provider), EngineOptions{LLM: reflectLLM.Call})
	if _, err := eng.Run(context.Background(), "task"); err != nil {
		t.Fatalf("run: %v", err)
	}
	provider.mu.Lock()
	all := strings.Join(provider.texts, "\n")
	provider.mu.Unlock()
	if !strings.Contains(all, "自检未通过") || !strings.Contains(all, "缺验证证据") {
		t.Fatalf("retry message missing reflection feedback: %q", all)
	}
}

func planStepsJSON() string {
	return `[{"title": "步骤一", "instruction": "写 config.js"}, {"title": "步骤二", "instruction": "写 game.js"}]`
}

// TestPlanExecuteEngine_StepsThenFinalize 规划 → 逐步执行 → 汇总终答。
func TestPlanExecuteEngine_StepsThenFinalize(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"s1 done", "s2 done", "final answer"}}
	planLLM := &scriptedLLM{replies: []string{planStepsJSON()}}
	var sinkGoal string
	var sinkSteps []PlanStep
	eng := NewPlanExecuteEngine(newEngineTestAgent(t, agentProvider), EngineOptions{
		LLM:          planLLM.Call,
		PlanMaxSteps: 8,
		PlanSink: func(goal string, steps []PlanStep) error {
			sinkGoal = goal
			sinkSteps = steps
			return nil
		},
	})
	result, err := eng.Run(context.Background(), "做一个塔防游戏")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "final answer" {
		t.Fatalf("expected final answer, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 3 {
		t.Fatalf("expected 3 agent runs (2 steps + finalize), got %d", got)
	}
	if sinkGoal != "做一个塔防游戏" || len(sinkSteps) != 2 {
		t.Fatalf("plan sink not called with goal+steps: goal=%q steps=%d", sinkGoal, len(sinkSteps))
	}
	if sinkSteps[0].Title != "步骤一" || sinkSteps[1].Instruction != "写 game.js" {
		t.Fatalf("plan steps mismatch: %+v", sinkSteps)
	}
}

// TestPlanExecuteEngine_StepHistoryAccumulates 后续步骤请求携带前序产出。
func TestPlanExecuteEngine_StepHistoryAccumulates(t *testing.T) {
	provider := &captureEngineProvider{replies: []string{"s1 done", "s2 done", "final"}}
	planLLM := &scriptedLLM{replies: []string{planStepsJSON()}}
	eng := NewPlanExecuteEngine(newEngineTestAgent(t, provider), EngineOptions{LLM: planLLM.Call})
	if _, err := eng.Run(context.Background(), "task"); err != nil {
		t.Fatalf("run: %v", err)
	}
	provider.mu.Lock()
	all := strings.Join(provider.texts, "\n")
	provider.mu.Unlock()
	// 全部请求的消息中应同时含步骤一与步骤二的产出（步骤二调用时能看到 s1 done）。
	if !strings.Contains(all, "s1 done") || !strings.Contains(all, "s2 done") {
		t.Fatalf("step history not accumulated: %q", all)
	}
}

// TestPlanExecuteEngine_PlanErrorDegrades 规划 LLM 报错降级纯 ReAct。
func TestPlanExecuteEngine_PlanErrorDegrades(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	planLLM := &scriptedLLM{errs: []error{errors.New("plan llm down")}}
	eng := NewPlanExecuteEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: planLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected single agent run, got %d", got)
	}
}

// TestPlanExecuteEngine_InvalidPlanDegrades 规划返回非法 JSON/空数组降级纯 ReAct。
func TestPlanExecuteEngine_InvalidPlanDegrades(t *testing.T) {
	for _, reply := range []string{"不是 JSON", `[]`, `[{"title": "缺指令"}]`} {
		agentProvider := &scriptedProvider{replies: []string{"answer"}}
		planLLM := &scriptedLLM{replies: []string{reply}}
		eng := NewPlanExecuteEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: planLLM.Call})
		result, err := eng.Run(context.Background(), "task")
		if err != nil {
			t.Fatalf("run (reply=%q): %v", reply, err)
		}
		if result.Text != "answer" {
			t.Fatalf("expected answer (reply=%q), got %q", reply, result.Text)
		}
		if got := agentProvider.callsCount(); got != 1 {
			t.Fatalf("expected single agent run (reply=%q), got %d", reply, got)
		}
	}
}

// TestPlanExecuteEngine_MaxStepsCapped 步骤数超上限时截断，仅执行前 maxSteps 步。
func TestPlanExecuteEngine_MaxStepsCapped(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"s1", "s2", "final"}}
	planLLM := &scriptedLLM{replies: []string{
		`[{"title":"1","instruction":"i1"},{"title":"2","instruction":"i2"},{"title":"3","instruction":"i3"},{"title":"4","instruction":"i4"},{"title":"5","instruction":"i5"}]`,
	}}
	eng := NewPlanExecuteEngine(newEngineTestAgent(t, agentProvider), EngineOptions{
		LLM:          planLLM.Call,
		PlanMaxSteps: 2,
	})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "final" {
		t.Fatalf("expected final, got %q", result.Text)
	}
	if got := agentProvider.callsCount(); got != 3 {
		t.Fatalf("expected 3 agent runs (2 steps + finalize), got %d", got)
	}
}

// TestEngineModeConstants 模式常量三值稳定（schema/validateDispatchArgs 依赖）。
func TestEngineModeConstants(t *testing.T) {
	if ModeReact != "react" || ModeReflection != "reflection" || ModePlanExecute != "plan_execute" {
		t.Fatalf("mode constants drift: %q %q %q", ModeReact, ModeReflection, ModePlanExecute)
	}
}
