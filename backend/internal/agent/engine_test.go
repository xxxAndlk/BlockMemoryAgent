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

// scriptedLLM 按调用顺序返回预设文本的 LLMComplete，并记录每次 prompt 供断言。
type scriptedLLM struct {
	mu      sync.Mutex
	replies []string
	errs    []error
	calls   int
	prompts []string
}

func (l *scriptedLLM) Call(ctx context.Context, prompt string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.calls
	l.calls++
	l.prompts = append(l.prompts, prompt)
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

func (l *scriptedLLM) lastPrompt() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.prompts) == 0 {
		return ""
	}
	return l.prompts[len(l.prompts)-1]
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
// TestReflectEngine_NoLLMFallback 无 judge LLM 时 fail-closed（TODO #43）：
// 标记 Unverified 而非静默通过（旧 fail-open 使自检形同虚设）。
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
	if !result.Unverified || result.VerifyNote == "" {
		t.Fatalf("expected Unverified with note, got Unverified=%v note=%q", result.Unverified, result.VerifyNote)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected single agent run, got %d", got)
	}
}

// TestReflectEngine_LLMErrorFailClosed 自检 LLM 报错时重试 1 次仍败 fail-closed
//（TODO #43/#54）：标记 Unverified 附原因，绝不静默放行。
func TestReflectEngine_LLMErrorFailClosed(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{errs: []error{errors.New("llm down"), errors.New("llm down")}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if !result.Unverified || !strings.Contains(result.VerifyNote, "llm down") {
		t.Fatalf("expected Unverified with judge error reason, got Unverified=%v note=%q", result.Unverified, result.VerifyNote)
	}
	if got := agentProvider.callsCount(); got != 1 {
		t.Fatalf("expected single agent run, got %d", got)
	}
	if got := reflectLLM.callsCount(); got != 2 {
		t.Fatalf("expected 2 judge calls (1 + 1 retry), got %d", got)
	}
}

// TestReflectEngine_InvalidJSONFailClosed 自检返回非法 JSON 时 fail-closed（TODO #43）。
func TestReflectEngine_InvalidJSONFailClosed(t *testing.T) {
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
	if !result.Unverified || !strings.Contains(result.VerifyNote, "invalid JSON") {
		t.Fatalf("expected Unverified with invalid JSON reason, got Unverified=%v note=%q", result.Unverified, result.VerifyNote)
	}
}

// TestReflectEngine_RubricChecks judge rubric 分项判定解析（TODO #43）：
// judge 返 checks 数组时逐条解析；pass=true 时完成摘要置 VerifyNote="L2 rubric"。
func TestReflectEngine_RubricChecks(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	rubricJSON := `{"pass": true, "feedback": "", "checks": [{"item": "验收一", "pass": true, "evidence": "测试通过"}, {"item": "验收二", "pass": true, "evidence": "文件已写"}]}`
	reflectLLM := &scriptedLLM{replies: []string{rubricJSON}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Unverified {
		t.Fatalf("rubric pass should not be unverified, note=%q", result.VerifyNote)
	}
	if result.VerifyNote != "L2 rubric" {
		t.Fatalf("expected VerifyNote 'L2 rubric', got %q", result.VerifyNote)
	}
}

// TestReflectEngine_JudgePromptSections judge prompt 含客观证据段（TODO #43）：
// 【修改文件】+【验证证据】+【验收标准】三段必在，堵幻觉 pass。
func TestReflectEngine_JudgePromptSections(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{replies: []string{reflectPass()}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	if _, err := eng.Run(context.Background(), "task with 验收标准"); err != nil {
		t.Fatalf("run: %v", err)
	}
	p := reflectLLM.lastPrompt()
	for _, want := range []string{"【修改文件】", "【验证证据】", "【验收标准】", "checks"} {
		if !strings.Contains(p, want) {
			t.Fatalf("judge prompt missing %q, got: %s", want, p)
		}
	}
}

// TestExtractJSON 验证 judge 响应提取变体（TODO #54）：
// 裸 JSON / ```json 围栏 / 前后夹带文字 / 截断坏 JSON。
func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"裸 JSON", `{"pass": true}`, `{"pass": true}`},
		{"json 围栏", "```json\n{\"pass\": true, \"feedback\": \"\"}\n```", `{"pass": true, "feedback": ""}`},
		{"无语言围栏", "```\n{\"pass\": false}\n```", `{"pass": false}`},
		{"前置文字", "判定如下：\n{\"pass\": true}", `{"pass": true}`},
		{"前后夹带", "好的，结果：{\"pass\": true, \"checks\": []} 以上。", `{"pass": true, "checks": []}`},
		{"字符串内花括号与嵌套", `{"a": "{不是括号}", "b": {"c": 1}}`, `{"a": "{不是括号}", "b": {"c": 1}}`},
		{"截断坏 JSON", `{"pass": true, "feedback": "未闭合`, ""},
		{"非 JSON", "完全不是 JSON", ""},
	}
	for _, c := range cases {
		if got := extractJSON(c.in); got != c.want {
			t.Errorf("%s: extractJSON(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// TestReflectEngine_FencedJSONAccepted judge 返围栏 JSON 不再误判（TODO #54 根因场景）：
// 首答即通过，无需重试。
func TestReflectEngine_FencedJSONAccepted(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{replies: []string{"```json\n" + reflectPass() + "\n```"}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Unverified {
		t.Fatalf("fenced JSON should parse, got Unverified note=%q", result.VerifyNote)
	}
	if result.VerifyNote != "L2 rubric" {
		t.Fatalf("expected 'L2 rubric', got %q", result.VerifyNote)
	}
	if got := reflectLLM.callsCount(); got != 1 {
		t.Fatalf("expected 1 judge call, got %d", got)
	}
}

// TestReflectEngine_JudgeRetryRecovers 首答坏 JSON，重试（更严措辞）后通过。
func TestReflectEngine_JudgeRetryRecovers(t *testing.T) {
	agentProvider := &scriptedProvider{replies: []string{"answer"}}
	reflectLLM := &scriptedLLM{replies: []string{"前置解释文字导致解析失败", reflectPass()}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Unverified || result.VerifyNote != "L2 rubric" {
		t.Fatalf("retry should recover, got Unverified=%v note=%q", result.Unverified, result.VerifyNote)
	}
	if got := reflectLLM.callsCount(); got != 2 {
		t.Fatalf("expected 2 judge calls, got %d", got)
	}
	if p := reflectLLM.lastPrompt(); !strings.Contains(p, "【重试】") {
		t.Fatalf("retry prompt should carry strict suffix, got: %s", p)
	}
}

// verifyThenAnswerProvider 第一次调用返回验证类 RunCommand 工具调用（echo verify，
// 命中 IsVerificationCommand 且退出 0 = L0 可执行证据），第二次返回最终答复。
type verifyThenAnswerProvider struct {
	scriptedProvider
}

func (p *verifyThenAnswerProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.mu.Lock()
	p.calls++
	calls := p.calls
	p.mu.Unlock()
	if calls == 1 {
		return &blades.ModelResponse{Message: blades.AssistantMessage(
			blades.NewToolPart("call-verify-1", "RunCommand", `{"command":"echo verify"}`),
		)}, nil
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("answer")}, nil
}

// TestReflectEngine_L0EvidenceDegradesPassWithWarning judge 重试仍败 + L0 可执行证据存在
//（TODO #54 降级）：pass-with-warning 标注放行，不 Unverified 不触发重派。
func TestReflectEngine_L0EvidenceDegradesPassWithWarning(t *testing.T) {
	agentProvider := &verifyThenAnswerProvider{}
	reflectLLM := &scriptedLLM{replies: []string{"坏输出", "还是坏输出"}}
	eng := NewReflectEngine(newEngineTestAgent(t, agentProvider), EngineOptions{LLM: reflectLLM.Call})
	result, err := eng.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("expected answer, got %q", result.Text)
	}
	if result.Unverified {
		t.Fatalf("L0 evidence should degrade to pass-with-warning, got Unverified note=%q", result.VerifyNote)
	}
	if !strings.Contains(result.VerifyNote, "L0 通过") || !strings.Contains(result.VerifyNote, "judge 不可用") {
		t.Fatalf("expected degrade note, got %q", result.VerifyNote)
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

// TestPlanStepMessage_SlimReportFormat 验证步骤消息携带三段式瘦身报告模板（TODO #50）：
// 结论/关键改动位置/验收证据三节齐全，且保留"验收标准逐条判 PASS/FAIL"纪律。
func TestPlanStepMessage_SlimReportFormat(t *testing.T) {
	msg := planStepMessage(PlanStep{Title: "步骤一", Instruction: "写 config.js"}, 1, 2)
	for _, want := range []string{
		"【执行计划 步骤 1/2】步骤一",
		"写 config.js",
		"① 结论",
		"② 关键改动位置",
		"③ 验收证据",
		"PASS/FAIL",
		"禁止整段复述 diff 与代码原文",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("step message missing %q:\n%s", want, msg)
		}
	}
}

// TestEngineModeConstants 模式常量三值稳定（schema/validateDispatchArgs 依赖）。
func TestEngineModeConstants(t *testing.T) {
	if ModeReact != "react" || ModeReflection != "reflection" || ModePlanExecute != "plan_execute" {
		t.Fatalf("mode constants drift: %q %q %q", ModeReact, ModeReflection, ModePlanExecute)
	}
}

// streamEngineProvider 同时实现 Generate 与 NewStreaming：
// Generate 返回 "from generate" 作为判别标记，NewStreaming 可编程（前 streamFails 次产出错误流）。
// 用于验证 NewEngineLLM 流式优先（2026-08-13 ark 拒绝非流式致 reflection 自检全天 fail-open 的根因场景）。
type streamEngineProvider struct {
	scriptedProvider
	streamFails int
}

// NewStreaming 满足 streamingModelProvider：错误流或单条成功产出。
func (p *streamEngineProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		if p.streamFails > 0 {
			p.streamFails--
			yield(nil, errors.New("streaming is required for operations that may take longer than 10 minutes"))
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage(`{"pass": true, "feedback": ""}`)}, nil)
	}
}

// TestNewEngineLLM_PrefersStreaming 验证 provider 支持流式时走 NewStreaming 而非 Generate。
func TestNewEngineLLM_PrefersStreaming(t *testing.T) {
	p := &streamEngineProvider{scriptedProvider: scriptedProvider{replies: []string{"from generate"}}}
	llm := NewEngineLLM(p)
	got, err := llm(context.Background(), "自检")
	if err != nil {
		t.Fatalf("流式路径应成功，got err: %v", err)
	}
	if got != `{"pass": true, "feedback": ""}` {
		t.Fatalf("应返回流式响应，got %q", got)
	}
	if p.calls != 0 {
		t.Fatalf("不应调用 Generate，got %d calls", p.calls)
	}
}

// TestNewEngineLLM_GenerateFallback 验证无流式接口的 provider 回退 Generate。
func TestNewEngineLLM_GenerateFallback(t *testing.T) {
	p := &scriptedProvider{replies: []string{"from generate"}}
	llm := NewEngineLLM(p)
	got, err := llm(context.Background(), "自检")
	if err != nil {
		t.Fatalf("回退路径应成功，got err: %v", err)
	}
	if got != "from generate" {
		t.Fatalf("应返回 Generate 响应，got %q", got)
	}
}

// TestNewEngineLLM_StreamingError 验证流式错误透传（引擎 fail-open 兜底）。
func TestNewEngineLLM_StreamingError(t *testing.T) {
	p := &streamEngineProvider{scriptedProvider: scriptedProvider{replies: []string{"x"}}, streamFails: 1}
	llm := NewEngineLLM(p)
	if _, err := llm(context.Background(), "自检"); err == nil {
		t.Fatal("流式错误应透传")
	}
}
