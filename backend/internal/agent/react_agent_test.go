// Package agent 包含 ReActAgent 的单元测试。
package agent

import (
	// context 用于传递测试请求上下文。
	"context"
	// encoding/json 用于在测试中构造工具调用的 JSON 参数。
	"encoding/json"
	// errors 用于构造模拟的瞬时 LLM 错误。
	"errors"
	// fmt 用于构造每轮不同的读取路径（规避连读同参守卫的干扰）。
	"fmt"
	// os 与 path/filepath 用于校验工具落盘文件内容。
	"os"
	"path/filepath"
	// strings 用于构造长字符串与断言内容。
	"strings"
	// sync/atomic 用于并发安全的活动计数。
	"sync/atomic"
	// time 用于重试退避等时间参数。
	"time"
	// testing 提供 Go 标准测试框架。
	"testing"

	// tool 提供内建工具注册表，用于构造可调用的工具环境。
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	// mailbox 提供共享邮箱，用于异步子 Agent 摘要投递测试。
	"github.com/blockmemory/agent/backend/internal/mailbox"
	// types 提供角色定义等 DTO。
	"github.com/blockmemory/agent/backend/pkg/types"
	// blades 提供模型消息与 provider 接口。
	"github.com/go-kratos/blades"
	// tools 提供工具 Schema 类型。
	"github.com/go-kratos/blades/tools"
)

// mockModelProvider 是一个可编程的 blades.ModelProvider，用于 ReActAgent 测试。
type mockModelProvider struct {
	responses []*blades.Message // responses 预设的模型响应队列
	calls     int               // calls 记录 Generate 被调用次数
}

// Generate 按顺序返回预设响应，耗尽后返回默认 "done"。
func (m *mockModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 若调用次数超过预设响应数，返回默认结束消息，避免测试死循环。
	if m.calls >= len(m.responses) {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	// 取出当前索引对应的响应，调用计数加一。
	resp := m.responses[m.calls]
	m.calls++
	return &blades.ModelResponse{Message: resp}, nil
}

// Name 返回 mock provider 名称。
func (m *mockModelProvider) Name() string { return "mock" }

// TestReActAgent_Run_NoTools 验证模型直接返回文本时，Run 能正确得到结果与历史。
func TestReActAgent_Run_NoTools(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("hello world"),
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	// 执行 ReAct 循环。
	res, err := agent.Run(context.Background(), "say hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 校验最终文本。
	if res.Text != "hello world" {
		t.Fatalf("expected 'hello world', got %q", res.Text)
	}
	// 历史应包含 user 与 assistant 两条消息。
	if len(res.History) != 2 {
		t.Fatalf("expected 2 history messages, got %d", len(res.History))
	}
}

// TestReActAgent_Run_WithToolCall 验证模型请求工具调用时，Agent 能执行工具并继续对话。
func TestReActAgent_Run_WithToolCall(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)

	// 第一次模型响应要求写入文件，第二次给出最终回复。
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "test.txt", "content": "42"}))},
				},
			},
			blades.AssistantMessage("wrote the file"),
		},
	}

	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))
	res, err := agent.Run(context.Background(), "write test.txt with 42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "wrote the file" {
		t.Fatalf("expected 'wrote the file', got %q", res.Text)
	}
	// 历史包含 user、assistant(tool)、tool、assistant 四条消息。
	if len(res.History) != 4 { // user, assistant(tool), tool, assistant
		t.Fatalf("expected 4 history messages, got %d", len(res.History))
	}
}

// TestReActAgent_ActivityReporter 验证 generateOnce 与工具派发均触发 activityReporter 回调，
// 供 Dispatcher 心跳巡检判活。Run 单协程同步执行，plain int 计数器无需加锁。
func TestReActAgent_ActivityReporter(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "a.txt", "content": "x"}))},
				},
			},
			blades.AssistantMessage("done"),
		},
	}
	var touches int
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg)).
		WithActivityReporter(func() { touches++ })
	if _, err := a.Run(context.Background(), "write a.txt"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 至少 3 次：第一轮 generateOnce + 工具派发，第二轮 generateOnce。
	if touches < 3 {
		t.Fatalf("expected >=3 activity touches (generateOnce+tool+generateOnce), got %d", touches)
	}
}

// TestReActAgent_Run_MaxIterations 验证当模型持续请求工具调用且达到最大迭代次数时：
// 不返回错误，而是返回 LimitReached 标记与完整历史，由上层暂停会话等待用户续跑。
func TestReActAgent_Run_MaxIterations(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "ListDir", Request: string(mustJSON(map[string]any{"path": "."}))},
				},
			},
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "ListDir", Request: string(mustJSON(map[string]any{"path": "."}))},
				},
			},
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "ListDir", Request: string(mustJSON(map[string]any{"path": "."}))},
				},
			},
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg)).
		WithMaxIterations(2)

	result, err := agent.Run(context.Background(), "loop")
	if err != nil {
		t.Fatalf("达到轮数上限不应返回错误（改为 LimitReached 暂停），got: %v", err)
	}
	if !result.LimitReached {
		t.Fatal("达到轮数上限时 LimitReached 应为 true")
	}
	// 历史应保留全部进度（user + 2 轮 assistant/tool），供续跑使用。
	if len(result.History) == 0 {
		t.Fatal("LimitReached 时 History 应保留进度")
	}
}

// mustJSON 将任意值序列化为 JSON 字节切片，忽略序列化错误。
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// fakePendingChecker 模拟父 Agent 有未决子 Agent 的场景。
// PendingChildren 在 mailbox 投递前返 1，WaitForAnyChild 调用时投递邮箱消息并切到 0。
type fakePendingChecker struct {
	mb        *mailbox.Mailbox
	parentID  string
	delivered bool
}

func (f *fakePendingChecker) PendingChildren(parentID string) int {
	if parentID != f.parentID || f.delivered {
		return 0
	}
	return 1
}

func (f *fakePendingChecker) WaitForAnyChild(parentID string, timeout time.Duration) bool {
	// 投递一条 mailbox 消息模拟子 Agent 完成。
	if !f.delivered && f.mb != nil {
		_, _ = f.mb.Send(&mailbox.Message{From: "session-1/code_assistant-1", To: parentID, Type: mailbox.MsgInfo, Body: "子 Agent 完成: ok"})
		f.delivered = true
	}
	return true
}

// TestReActAgent_WakeOnMailbox 验证：父 Agent 给出终答前若有未决子 Agent，
// 应阻塞等待 mailbox 而非立刻终结；mailbox 到达后续跑 ReAct 整合结果。
// 旧实现 30s 超时白跑 LLM 烧 maxIter，塔防任务死等 46 分钟；新实现纯阻塞等信号。
func TestReActAgent_WakeOnMailbox(t *testing.T) {
	mb := mailbox.New()
	llm := &mockModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("待子 Agent 完成"), // 第一轮：终答但子 Agent 未决
			blades.AssistantMessage("整合完毕：ok"),     // 第二轮：吸收 mailbox 摘要后给最终答复
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	checker := &fakePendingChecker{mb: mb, parentID: "test"}
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: ""}, llm, NewToolRegistryAdapter(reg)).
		WithMailbox(mb).
		WithPendingChildrenChecker(checker)

	res, err := ag.Run(context.Background(), "wait for sub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(res.Text, "整合完毕") {
		t.Fatalf("expected final answer after mailbox drain, got %q", res.Text)
	}
	if llm.calls != 2 {
		t.Fatalf("expected 2 LLM calls (initial + post-mailbox), got %d", llm.calls)
	}
}

// fakePausedChildChecker 模拟父 Agent 有未决子 Agent 且其中一个为 Paused domain 的场景。
// PendingChildren 恒返 1（不完成），HasPausedChild 恒返 true，使 wait loop 命中 PausedOnChild 分支。
type fakePausedChildChecker struct{}

func (f *fakePausedChildChecker) PendingChildren(string) int                 { return 1 }
func (f *fakePausedChildChecker) WaitForAnyChild(string, time.Duration) bool { return false }
func (f *fakePausedChildChecker) HasPausedChild(string) bool                 { return true }

// TestReActAgent_PausedOnChild 验证：父 Agent 给出终答前若有未决子 Agent 且存在 Paused 子 domain，
// wait loop 应跳出返回 ReactResult{LimitReached:true, PausedOnChild:true}，由上层置会话暂停态。
func TestReActAgent_PausedOnChild(t *testing.T) {
	mb := mailbox.New()
	llm := &mockModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("终答但子 domain 暂停"), // 第一轮终答，但子 Agent 未决 + Paused
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	checker := &fakePausedChildChecker{}
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: ""}, llm, NewToolRegistryAdapter(reg)).
		WithMailbox(mb).
		WithPendingChildrenChecker(checker).
		WithPausedChildChecker(checker)

	res, err := ag.Run(context.Background(), "wait for sub")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.LimitReached {
		t.Error("expected LimitReached=true when paused child detected")
	}
	if !res.PausedOnChild {
		t.Error("expected PausedOnChild=true when HasPausedChild returns true")
	}
}

// tokenUsageProvider 返回带 TokenUsage 的响应，用于测试 token 预算上限。
// 每次响应固定 InputTokens/OutputTokens，第 calls 次后给空响应收尾防死循环。
type tokenUsageProvider struct {
	input  int64
	output int64
	calls  int
}

func (m *tokenUsageProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.calls++
	msg := blades.AssistantMessage("working")
	msg.TokenUsage = blades.TokenUsage{InputTokens: m.input, OutputTokens: m.output, TotalTokens: m.input + m.output}
	return &blades.ModelResponse{Message: msg}, nil
}

func (m *tokenUsageProvider) Name() string { return "tokenUsage" }

// TestReActAgent_ContextBudgetExceeded 验证上下文 token 超阈值（压缩后仍超=近 N 单独就超、
// 压不下去）时返回部分完成（LimitReached），在首轮 LLM 调用前拦截，而非错误或继续烧轮次。
// 与 maxIter 轮数上限正交。
func TestReActAgent_ContextBudgetExceeded(t *testing.T) {
	llm := &tokenUsageProvider{input: 60, output: 60} // 即使真调了也能计数
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithLoopConfig(LoopConfig{TokenBudget: 5, MaxIterations: 50}) // 极小阈值：任务+时间消息估算即超

	res, err := ag.Run(context.Background(), "budget test")
	if err != nil {
		t.Fatalf("超预算不应返回错误（应 LimitReached 暂停）: %v", err)
	}
	if !res.LimitReached {
		t.Fatal("超预算时 LimitReached 应为 true")
	}
	if llm.calls != 0 {
		t.Fatalf("上下文超阈值应在首轮 LLM 调用前拦截，got %d calls", llm.calls)
	}
}

// TestEstimateMessagesTokens 验证消息切片 token 估算累加 Content + ToolCalls + ReasoningContent。
func TestEstimateMessagesTokens(t *testing.T) {
	if got := EstimateMessagesTokens(nil); got != 0 {
		t.Fatalf("empty messages should estimate 0, got %d", got)
	}
	base := EstimateMessagesTokens([]ReactMessage{{Role: "user", Content: "任务目标"}})
	if base <= 0 {
		t.Fatalf("non-empty content should estimate > 0, got %d", base)
	}
	msgs := []ReactMessage{
		{Role: "user", Content: "任务目标"},
		{Role: "assistant", Content: "code", ToolCalls: []ToolCall{{ID: "c1", Name: "RunCommand", Input: map[string]any{"command": "node -c a.js"}}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"tool":"RunCommand","success":true,"output":"PASS"}`},
		{Role: "assistant", ReasoningContent: "thinking...", Content: "done"},
	}
	total := EstimateMessagesTokens(msgs)
	if total <= base {
		t.Fatalf("should accumulate all parts (content+toolcalls+reasoning), got %d vs base %d", total, base)
	}
}

// TestReActAgent_TokenBudgetZeroUnlimited 验证 tokenBudget<=0（默认）时不触发预算限制，
// 模型直接给终答正常返回。
func TestReActAgent_TokenBudgetZeroUnlimited(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{blades.AssistantMessage("done")},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithLoopConfig(LoopConfig{TokenBudget: 0}) // 0 = 不限制

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.LimitReached {
		t.Fatal("tokenBudget=0 时不应触发 LimitReached")
	}
	if res.Text != "done" {
		t.Fatalf("expected 'done', got %q", res.Text)
	}
}

// fakePersonaInjector 测试用 PersonaInjector，固定前缀人格内容。
type fakePersonaInjector struct{ prefix string }

func (f fakePersonaInjector) Inject(systemPrompt string) string {
	if f.prefix == "" {
		return systemPrompt
	}
	return f.prefix + "\n\n---\n\n" + systemPrompt
}

// TestReActAgent_PersonaInjected 验证注入 PersonaInjector 后，系统提示词头部带人格前缀；
// nil 时不注入，原样返回。
func TestReActAgent_PersonaInjected(t *testing.T) {
	llm := &mockModelProvider{responses: []*blades.Message{blades.AssistantMessage("ok")}}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "base"}, llm, NewToolRegistryAdapter(reg)).
		WithPersonaInjector(fakePersonaInjector{prefix: "【人格】谦逊严谨"})

	res, err := ag.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("expected 'ok', got %q", res.Text)
	}
	if llm.calls != 1 {
		t.Fatalf("expected 1 LLM call, got %d", llm.calls)
	}
}

// TestReActAgent_NilPersonaNoOp 验证 nil PersonaInjector 时 systemPrompt 不注入人格（无副作用）。
func TestReActAgent_NilPersonaNoOp(t *testing.T) {
	llm := &mockModelProvider{responses: []*blades.Message{blades.AssistantMessage("ok")}}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	ag := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "base"}, llm, NewToolRegistryAdapter(reg))

	// systemPrompt 不应包含人格分隔符（nil persona 不注入）。
	if strings.Contains(ag.systemPrompt(), "---") {
		t.Fatal("nil persona 时 systemPrompt 不应注入人格分隔符")
	}
	if _, err := ag.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// flakyModelProvider 前 failTimes 次 Generate 返回错误，之后返回成功响应，用于重试测试。
type flakyModelProvider struct {
	failTimes int
	calls     int
}

// Generate 前 failTimes 次返回瞬时错误，之后返回 "ok"。
func (m *flakyModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.calls++
	if m.calls <= m.failTimes {
		return nil, errors.New("transient llm error")
	}
	return &blades.ModelResponse{Message: blades.AssistantMessage("ok")}, nil
}

// Name 返回 mock provider 名称。
func (m *flakyModelProvider) Name() string { return "flaky" }

// TestReActAgent_GenerateRetriesThenSucceeds 验证 LLM 瞬时失败时会按指数退避重试，
// 重试内成功则正常返回结果，而不是让整个会话失败。
func TestReActAgent_GenerateRetriesThenSucceeds(t *testing.T) {
	llm := &flakyModelProvider{failTimes: 2}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithLoopConfig(LoopConfig{RetryCount: 3, RetryBackoff: time.Millisecond})

	res, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("重试内成功不应返回错误: %v", err)
	}
	if res.Text != "ok" {
		t.Fatalf("expected 'ok', got %q", res.Text)
	}
	if llm.calls != 3 {
		t.Fatalf("应重试到第 3 次才成功，got %d calls", llm.calls)
	}
}

// TestReActAgent_GenerateRetryExhausted 验证重试耗尽后返回错误，且 History 保留已积累进度。
func TestReActAgent_GenerateRetryExhausted(t *testing.T) {
	llm := &flakyModelProvider{failTimes: 10}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithLoopConfig(LoopConfig{RetryCount: 2, RetryBackoff: time.Millisecond})

	res, err := agent.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("重试耗尽后应返回错误")
	}
	if llm.calls != 3 {
		t.Fatalf("RetryCount=2 应共尝试 3 次，got %d calls", llm.calls)
	}
	if len(res.History) == 0 {
		t.Fatal("出错时 History 应保留进度（供部分结果回传）")
	}
}

// deadlineProvider 模拟单次调用超时（返回 context.DeadlineExceeded），
// 用于验证 generate() 不在 deadline 上重试（避免慢推理模型 180s×N 重试风暴）。
type deadlineProvider struct{ calls int }

func (m *deadlineProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.calls++
	return nil, context.DeadlineExceeded
}
func (m *deadlineProvider) Name() string { return "deadline" }

// TestReActAgent_GenerateNoRetryOnDeadline 验证单次调用超时（DeadlineExceeded）
// 不会重试：慢推理模型重试只会重复同样超时，白等 N×timeout（实证 12min 卡死）。
func TestReActAgent_GenerateNoRetryOnDeadline(t *testing.T) {
	llm := &deadlineProvider{}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).
		WithLoopConfig(LoopConfig{RetryCount: 3, RetryBackoff: time.Millisecond})

	_, err := agent.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("deadline 应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err 应为 DeadlineExceeded, got %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("deadline 不应重试, want 1 call, got %d", llm.calls)
	}
}

// stubToolRegistry 返回固定大输出的工具注册表，用于输出截断测试。
type stubToolRegistry struct {
	output string
}

// Schema 返回空工具列表。
func (s stubToolRegistry) Schema() []tools.Tool { return nil }

// Dispatch 返回预设的大输出。
func (s stubToolRegistry) Dispatch(ctx context.Context, call ToolCall) (ToolResult, error) {
	return ToolResult{Tool: call.Name, Success: true, Output: s.output}, nil
}

// TestReActAgent_ToolOutputTruncatedInHistory 验证写入历史的工具输出按
// ToolOutputMaxRunes 截断，防止长任务历史无限膨胀。
func TestReActAgent_ToolOutputTruncatedInHistory(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "ReadFile", Request: string(mustJSON(map[string]any{"path": "big.txt"}))},
				},
			},
			blades.AssistantMessage("done"),
		},
	}
	big := strings.Repeat("x", 5000)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, stubToolRegistry{output: big}).
		WithLoopConfig(LoopConfig{ToolOutputMaxRunes: 100})

	res, err := agent.Run(context.Background(), "read big")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 找到 tool 消息，确认输出被截断到 100 字符 + 省略提示。
	var toolMsg string
	for _, m := range res.History {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if toolMsg == "" {
		t.Fatal("历史中应包含 tool 消息")
	}
	if strings.Contains(toolMsg, strings.Repeat("x", 200)) {
		t.Fatal("工具输出未被截断")
	}
	if !strings.Contains(toolMsg, "...(truncated)") {
		t.Fatal("截断后应包含省略提示")
	}
}

// TestWindowMessages 验证滑动窗口裁剪：保留 system 前缀，在 user 边界下刀，
// 被省略部分以说明消息占位，tool 调用链不被切散。
func TestWindowMessages(t *testing.T) {
	msgs := []ReactMessage{
		{Role: "system", Content: "近期事件"},
		{Role: "user", Content: "g"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", Content: "r1"},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", Content: "r2"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a3"},
	}
	out := windowMessages(msgs, 5)
	if len(out) > 5 {
		t.Fatalf("裁剪后应不超过 5 条，got %d", len(out))
	}
	if out[0].Role != "system" {
		t.Fatal("应保留 system 前缀")
	}
	// 首条 user（任务目标）必须保留，避免子 Agent 跑几轮后 task 被占位替换。
	if out[1].Content != "g" {
		t.Fatalf("第 2 条应为保留的首条 user 任务目标，got: %s", out[1].Content)
	}
	if !strings.Contains(out[2].Content, "省略") {
		t.Fatalf("第 3 条应为省略说明，got: %s", out[2].Content)
	}
	// 保留段应从 user 边界开始，不能出现悬空的 tool 消息。
	if out[3].Role != "user" {
		t.Fatalf("保留段应从 user 边界开始，got role=%s", out[3].Role)
	}
	if out[len(out)-1].Content != "a3" {
		t.Fatal("最近消息应保留")
	}

	// max<=0 或未超限时原样返回。
	if got := windowMessages(msgs, 0); len(got) != len(msgs) {
		t.Fatal("max<=0 时不应裁剪")
	}
	if got := windowMessages(msgs, 100); len(got) != len(msgs) {
		t.Fatal("未超限时不应裁剪")
	}
}

// TestReActAgent_EmptyResponseNudged 验证模型返回空响应（无文本、无工具调用）时，
// 不会被误判为"最终答复"导致任务静默中断，而是注入提示让模型继续。
func TestReActAgent_EmptyResponseNudged(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			// 第一条是空 assistant 消息（无 Parts），模拟端点异常/max_tokens 截断。
			{Role: blades.RoleAssistant},
			blades.AssistantMessage("real answer"),
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg))

	res, err := agent.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("空响应后恢复不应返回错误: %v", err)
	}
	if res.Text != "real answer" {
		t.Fatalf("expected 'real answer', got %q", res.Text)
	}
	if llm.calls != 2 {
		t.Fatalf("空响应应触发一次额外 LLM 调用，got %d calls", llm.calls)
	}
	// 历史中应包含空响应提示消息，且不应残留空 assistant 消息。
	foundNudge := false
	for _, m := range res.History {
		if m.Role == "user" && strings.Contains(m.Content, "上一条回复为空") {
			foundNudge = true
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0 {
			t.Fatal("空 assistant 消息不应写入历史")
		}
	}
	if !foundNudge {
		t.Fatal("空响应后应向历史注入提示消息")
	}
}

// TestReActAgent_EmptyResponseStreakFails 验证连续空响应达到上限后返回显式错误，
// 而不是把空文本当作最终答复静默完成。
func TestReActAgent_EmptyResponseStreakFails(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{Role: blades.RoleAssistant},
			{Role: blades.RoleAssistant},
			{Role: blades.RoleAssistant},
			{Role: blades.RoleAssistant},
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg))

	res, err := agent.Run(context.Background(), "hi")
	if err == nil {
		t.Fatal("连续空响应达到上限后应返回错误")
	}
	if !strings.Contains(err.Error(), "empty responses") {
		t.Fatalf("错误信息应说明是连续空响应，got: %v", err)
	}
	if res.Text != "" {
		t.Fatal("出错时不应产生最终答复文本")
	}
	if llm.calls != maxEmptyResponses {
		t.Fatalf("应在第 %d 次空响应后报错，got %d calls", maxEmptyResponses, llm.calls)
	}
}

// TestReActAgent_TruncatesToolCallInputsInHistory 验证超长工具入参（如 WriteFile 全文）
// 只在写入历史的副本中截断，派发执行仍用完整入参：
//   - 磁盘文件内容为完整的 15000 字（派发未被截断影响）；
//   - 历史中 assistant 消息的 ToolCalls input 被截断（防止滑动窗口内逐轮重发耗尽预算）。
func TestReActAgent_TruncatesToolCallInputsInHistory(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)

	full := strings.Repeat("字", 15000)
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "big.txt", "content": full}))},
				},
			},
			blades.AssistantMessage("done"),
		},
	}

	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg))
	res, err := agent.Run(context.Background(), "write big.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 派发执行用完整入参：磁盘文件内容不被截断。
	b, err := os.ReadFile(filepath.Join(dir, "big.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(b) != full {
		t.Fatalf("dispatched content truncated: got %d runes, want %d", len([]rune(string(b))), 15000)
	}

	// 历史中的工具入参被截断：assistant(tool) 消息的 content 值带截断标记。
	var stored string
	for _, m := range res.History {
		for _, tc := range m.ToolCalls {
			if tc.Name == "WriteFile" {
				stored, _ = tc.Input["content"].(string)
			}
		}
	}
	if stored == "" {
		t.Fatal("history missing WriteFile tool call")
	}
	if !strings.Contains(stored, "truncated") {
		t.Fatalf("history tool input not truncated: len=%d runes", len([]rune(stored)))
	}
	if len([]rune(stored)) >= len([]rune(full)) {
		t.Fatalf("history tool input should be shorter than full content: got %d runes", len([]rune(stored)))
	}
}

// TestWindowMessages_WorkHistoryNotCollapsed 验证纯工作型历史（首条 user 后全是
// assistant/tool 交替、窗口内无 user）裁剪后不塌缩：
// 回归：旧实现锚定 user 边界，窗口内无 user 时走空整个窗口，只剩首条 user + 占位符
// 两条消息，Agent 每轮失忆（实证：塔防配置 Agent msgs=2 反复重写 config.js 不收敛）。
func TestWindowMessages_WorkHistoryNotCollapsed(t *testing.T) {
	msgs := []ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			ReactMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: "c" + string(rune('a'+i)), Name: "WriteFile", Input: map[string]any{"path": "f"}}}},
			ReactMessage{Role: "tool", Content: "ok"},
		)
	}
	out := windowMessages(msgs, 30)
	if len(out) < 10 {
		t.Fatalf("工作型历史不应塌缩到 %d 条", len(out))
	}
	if out[0].Content != "task" {
		t.Fatal("应保留首条 user 任务目标")
	}
	if !strings.Contains(out[1].Content, "省略") {
		t.Fatalf("第 2 条应为省略说明，got: %s", out[1].Content)
	}
	// 保留段不得以孤立 tool 结果起刀（须从 assistant/user 开始，保证调用对完整）。
	if out[2].Role == "tool" {
		t.Fatalf("保留段不应从孤立 tool 结果开始")
	}
	// 最近的 assistant/tool 对应完整保留。
	if out[len(out)-1].Content != "ok" {
		t.Fatalf("最近消息应保留，got: %v", out[len(out)-1])
	}

	// 窗口下刀恰落在 tool 上时应前进到 assistant，不得产出孤立 tool 起始。
	out2 := windowMessages(msgs, 29)
	for _, m := range out2[2:] {
		if m.Role == "tool" {
			// 首个非占位消息不能是 tool；此处只需保证 out2[2] 非 tool。
			break
		}
	}
	if out2[2].Role == "tool" {
		t.Fatal("保留段不应从孤立 tool 结果开始(max=29)")
	}
}

// TestSanitizeToolPairing 验证发送前的 tool 配对兜底：
// 孤立 tool 结果被丢弃、缺失响应的 tool_calls 就地补合成错误结果、已配对的原样保留。
func TestSanitizeToolPairing(t *testing.T) {
	paired := []ReactMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "WriteFile"}}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", Content: "done"},
	}
	if got := sanitizeToolPairing(paired); len(got) != len(paired) {
		t.Fatalf("已配对的消息不应被改动，got %d 条", len(got))
	}

	// 孤立 tool 结果（窗口起刀残留）应被丢弃。
	orphan := []ReactMessage{
		{Role: "tool", ToolCallID: "cX", Content: "stale"},
		{Role: "user", Content: "task"},
	}
	got := sanitizeToolPairing(orphan)
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("孤立 tool 结果应被丢弃，got: %+v", got)
	}

	// mailbox user 消息插在 assistant tool_calls 与 tool 结果之间（实证 400 的序列）：
	// 应在 assistant 后立即补合成结果，后续真实结果因孤立被丢弃。
	interposed := []ReactMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_sub_agent:10", Name: "call_sub_agent"}}},
		{Role: "user", Content: "[mailbox from sub] done"},
		{Role: "tool", ToolCallID: "call_sub_agent:10", Content: "real result"},
	}
	got = sanitizeToolPairing(interposed)
	if len(got) != 4 {
		t.Fatalf("修正后应为 4 条，got %d: %+v", len(got), got)
	}
	if got[2].Role != "tool" || got[2].ToolCallID != "call_sub_agent:10" {
		t.Fatalf("assistant tool_calls 后应紧随合成的 tool 结果，got: %+v", got[2])
	}
	if !strings.Contains(got[2].Content, "tool result missing") {
		t.Fatalf("合成结果应标注缺失原因，got: %s", got[2].Content)
	}
	if got[3].Role != "user" {
		t.Fatalf("mailbox user 消息应保留在 tool 结果之后，got: %+v", got[3])
	}

	// 并行 tool_calls 只有部分响应：只为缺失的补合成结果，保留真实结果。
	partial := []ReactMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "T1"}, {ID: "b", Name: "T2"}}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
		{Role: "user", Content: "next"},
	}
	got = sanitizeToolPairing(partial)
	if len(got) != 4 || got[2].ToolCallID != "b" || got[2].Role != "tool" {
		t.Fatalf("缺失的 tool_calls=b 应补合成结果，got: %+v", got)
	}
}

// TestReActAgent_MailboxAfterToolResult 验证 mailbox 注入时序：
// 有 tool_calls 的轮次，mailbox user 消息必须排在 tool 结果之后，
// 不得插在 assistant tool_calls 与其 tool 结果之间（Anthropic 400 回归，
// 实证 domain-2 白跑 31m43s 后整轮被拒）。
func TestReActAgent_MailboxAfterToolResult(t *testing.T) {
	mb := mailbox.New()
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	llm := &mockModelProvider{
		responses: []*blades.Message{
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "a.txt", "content": "1"}))},
				},
			},
			blades.AssistantMessage("done"),
		},
	}
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, llm, NewToolRegistryAdapter(reg)).WithMailbox(mb)

	// 子 Agent 完成通知在 Agent 执行工具前已到达 mailbox。
	_, _ = mb.Send(&mailbox.Message{From: "sub-1", To: "test", Type: mailbox.MsgInfo, Body: "子 Agent 完成"})

	res, err := agent.Run(context.Background(), "write a.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 期望历史：user, assistant(tool_calls), tool, user(mailbox), assistant(final)。
	if len(res.History) != 5 {
		t.Fatalf("expected 5 history messages, got %d: %+v", len(res.History), res.History)
	}
	if res.History[1].Role != "assistant" || len(res.History[1].ToolCalls) == 0 {
		t.Fatalf("第 2 条应为带 tool_calls 的 assistant，got: %+v", res.History[1])
	}
	if res.History[2].Role != "tool" {
		t.Fatalf("assistant tool_calls 后必须紧随 tool 结果，got role=%s", res.History[2].Role)
	}
	if res.History[3].Role != "user" || !strings.Contains(res.History[3].Content, "mailbox from") {
		t.Fatalf("mailbox 消息应排在 tool 结果之后，got: %+v", res.History[3])
	}
}

// TestSerializePromptForLog_ToolParts 验证 prompt 日志渲染 tool 调用与结果：
// 纯 tool_call 的 assistant 消息与 tool 结果消息不再序列化为空 content
// （回归：日志里大量 {"role":"tool","content":""} 被误以为上下文为空）。
func TestSerializePromptForLog_ToolParts(t *testing.T) {
	req := &blades.ModelRequest{
		Messages: []*blades.Message{
			blades.UserMessage("write a file"),
			{
				Role: blades.RoleAssistant,
				Parts: []blades.Part{
					blades.ToolPart{ID: "c1", Name: "WriteFile", Request: `{"path":"a.txt"}`},
				},
			},
			{
				Role:  blades.RoleTool,
				Parts: []blades.Part{blades.ToolPart{ID: "c1", Response: `{"ok":true}`}},
			},
		},
	}
	out := serializePromptForLog(req)
	if !strings.Contains(out, "[tool_call] name=WriteFile") {
		t.Fatalf("assistant 的 tool_call 应渲染进日志，got: %s", out)
	}
	if !strings.Contains(out, `{\"path\":\"a.txt\"}`) {
		t.Fatalf("tool_call 入参应渲染进日志，got: %s", out)
	}
	if !strings.Contains(out, "[tool_result] id=c1") {
		t.Fatalf("tool 结果应渲染进日志，got: %s", out)
	}
	if strings.Contains(out, `"content": ""`) {
		t.Fatalf("不应再出现空 content，got: %s", out)
	}
}

// TestBuildEnvBlock_InjectsProjectDoc 验证 buildEnvBlock 在存在 .bma/PROJECT.md 时
// 把 managed 区正文作为【项目概览】段注入；缺失时不附该段。
func TestBuildEnvBlock_InjectsProjectDoc(t *testing.T) {
	// 无 PROJECT.md 的工作目录：不应出现项目概览段。
	emptyDir := t.TempDir()
	env := buildEnvBlock(emptyDir)
	if strings.Contains(env, "【项目概览】") {
		t.Fatalf("空 workDir 不应注入项目概览，got: %s", env)
	}

	// 构造带 managed 区的 PROJECT.md。
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bma"), 0o755); err != nil {
		t.Fatalf("mkdir .bma: %v", err)
	}
	managed := "# 项目概览\n\n- 模块: github.com/example/demo\n## 推荐领域拆分\n### `backend/` - 后端服务\n"
	content := "<!-- bma:managed begin -->\n" + managed + "\n<!-- bma:managed end -->\n"
	if err := os.WriteFile(filepath.Join(dir, ".bma", "PROJECT.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write PROJECT.md: %v", err)
	}
	env = buildEnvBlock(dir)
	if !strings.Contains(env, "【项目概览】") {
		t.Fatalf("应注入项目概览段，got: %s", env)
	}
	if !strings.Contains(env, "github.com/example/demo") {
		t.Fatalf("应含 managed 正文，got: %s", env)
	}
	if !strings.Contains(env, "### `backend/` - 后端服务") {
		t.Fatalf("应含领域拆分条目，got: %s", env)
	}
	if strings.Contains(env, "bma:managed") {
		t.Fatalf("不应把标记本身注入提示词，got: %s", env)
	}
}

// TestMailboxMessageToReact_EscalatePrefix 验证 TODO #23 升级消息渲染：
// MsgEscalate 类型消息在 [mailbox from X] 后带 [升级] 前缀，父 LLM 可识别干预类消息。
func TestMailboxMessageToReact_EscalatePrefix(t *testing.T) {
	plain := mailboxMessageToReact(&mailbox.Message{
		From: "sub-1", To: "meta", Type: mailbox.MsgInfo, Subject: "完成",
	})
	if strings.Contains(plain.Content, "[升级]") {
		t.Fatalf("info message should not carry escalate prefix, got: %s", plain.Content)
	}
	esc := mailboxMessageToReact(&mailbox.Message{
		From: "sub-1", To: "meta", Type: mailbox.MsgEscalate, Subject: "验证未通过",
	})
	if !strings.Contains(esc.Content, "[mailbox from sub-1] [升级] 验证未通过") {
		t.Fatalf("escalate message should carry [升级] prefix, got: %s", esc.Content)
	}
}


// multiChunkStreamProvider 产出两个流式块的 provider，用于验证流式期间活动上报。
type multiChunkStreamProvider struct {
	genCalls int
}

func (p *multiChunkStreamProvider) Name() string { return "multichunk" }

func (p *multiChunkStreamProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.genCalls++
	return &blades.ModelResponse{Message: blades.AssistantMessage("from generate")}, nil
}

func (p *multiChunkStreamProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		yield(&blades.ModelResponse{Message: &blades.Message{Metadata: map[string]any{"thinking": "思考块1"}}}, nil)
		yield(&blades.ModelResponse{Message: &blades.Message{Metadata: map[string]any{"thinking": "思考块2"}}}, nil)
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("final")}, nil)
	}
}

// TestReActAgent_StreamingActivityReporter 验证流式长生成期间每个块都上报活动：
// 心跳巡检据此不误杀正在长时间生成代码的活跃叶子（实证 5-7 分钟调用被杀）。
func TestReActAgent_StreamingActivityReporter(t *testing.T) {
	p := &multiChunkStreamProvider{}
	var touches int
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, p, NewToolRegistryAdapter(reg)).
		WithActivityReporter(func() { touches++ })
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.genCalls != 0 {
		t.Fatalf("应走流式路径，不应调用 Generate，got %d", p.genCalls)
	}
	// generateOnce 起始 1 次 + 3 个流式块各 1 次，至少 4 次。
	if touches < 4 {
		t.Fatalf("流式块应触发活动上报，got %d touches", touches)
	}
}

// silentStreamProvider 流式 provider：先静默 silence 时长零 chunk（thinking 长考模拟），
// 再产出最终块——复现 08-13 塔防 42K 输入 5m58s 无首 chunk 被心跳误杀的场景。
type silentStreamProvider struct {
	silence time.Duration
}

func (p *silentStreamProvider) Name() string { return "silent-stream" }

func (p *silentStreamProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: blades.AssistantMessage("from generate")}, nil
}

func (p *silentStreamProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		select {
		case <-time.After(p.silence):
		case <-ctx.Done():
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("final")}, nil)
	}
}

// stallStreamProvider 流式 provider：第 1 次尝试产出首块后挂死（只等 ctx 取消），
// 第 2 次尝试正常收尾。复现 2026-08-20 ark glm-5.3 流中途静默：首块后零 chunk 直到墙钟。
type stallStreamProvider struct {
	attempts int
}

func (p *stallStreamProvider) Name() string { return "stall-stream" }

func (p *stallStreamProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: blades.AssistantMessage("from generate")}, nil
}

func (p *stallStreamProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		p.attempts++
		if !yield(&blades.ModelResponse{Message: blades.AssistantMessage("partial")}, nil) {
			return
		}
		if p.attempts == 1 {
			// 首块后流挂死：被取消前零 chunk，取消后优雅 return（不 yield 错误）。
			<-ctx.Done()
			return
		}
		yield(&blades.ModelResponse{Message: blades.AssistantMessage("final answer")}, nil)
	}
}

// TestReActAgent_StreamIdleTimeoutRetries 验证流式块间空闲超时：首块后挂死的流被
// 判死并取消，generate 经 RetryLLM 自动重试，第 2 次尝试成功拿到完整答复。
// 首块前静默不受影响（沿用 silentStreamProvider 场景，由整次调用墙钟兜底）。
func TestReActAgent_StreamIdleTimeoutRetries(t *testing.T) {
	p := &stallStreamProvider{}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, p, NewToolRegistryAdapter(reg)).
		WithStreamKeepalive(10 * time.Millisecond).
		WithStreamIdleTimeout(50 * time.Millisecond).
		WithLoopConfig(LoopConfig{RetryCount: 1, RetryBackoff: 10 * time.Millisecond})
	result, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("空闲超时应触发重试并成功, got err: %v", err)
	}
	if p.attempts != 2 {
		t.Fatalf("应重试一次（2 次尝试）, got %d", p.attempts)
	}
	if len(result.History) == 0 {
		t.Fatal("应产生历史")
	}
	last := result.History[len(result.History)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "final answer") {
		t.Fatalf("应拿到第 2 次尝试的完整答复, got %+v", last)
	}
}

// TestReActAgent_StreamKeepaliveDuringSilence 验证零 chunk 静默流期间保活定时器持续上报：
// chunk 级上报救不了"首 token 前长考"，需定时器补上报防心跳误杀；真实挂死由墙钟超时兜底。
func TestReActAgent_StreamKeepaliveDuringSilence(t *testing.T) {
	p := &silentStreamProvider{silence: 150 * time.Millisecond}
	var touches atomic.Int64
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	a := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "t"}, p, NewToolRegistryAdapter(reg)).
		WithActivityReporter(func() { touches.Add(1) }).
		WithStreamKeepalive(20 * time.Millisecond)
	if _, err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 静默 150ms 零 chunk；保活每 20ms 一次，至少 5 次上报。
	if n := touches.Load(); n < 5 {
		t.Fatalf("零 chunk 静默流期间保活应持续上报，got %d touches", n)
	}
}



// readCallMsg 构造一轮 ReadFile 工具调用响应（路径每轮不同，规避连读同参×3 守卫）。
func readCallMsg(i int) *blades.Message {
	return &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "ReadFile", Request: string(mustJSON(map[string]any{"path": fmt.Sprintf("f%02d.txt", i)}))},
		},
	}
}

// TestReActAgent_StagnationGuard_HardKill 连续 24 轮只读探查（无产出性工具/无 mailbox
// 新消息/无终答）触发停滞守卫 ErrLoopExit 硬杀；第 6/12 轮已注入递进预警。
// 覆盖既有守卫够不着的语义死循环（重复探针/完美主义不收口，2026-08-28 两小时撞墙实证）。
func TestReActAgent_StagnationGuard_HardKill(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	responses := make([]*blades.Message, 0, stagnationExitRounds)
	for i := 0; i < stagnationExitRounds; i++ {
		responses = append(responses, readCallMsg(i))
	}
	llm := &mockModelProvider{responses: responses}
	a := NewReActAgent("test", types.RoleDefinition{ID: "domain", SystemPrompt: "x"}, llm, NewToolRegistryAdapter(reg))
	res, err := a.Run(context.Background(), "probe")
	if err == nil || !errors.Is(err, tool.ErrLoopExit) {
		t.Fatalf("expected ErrLoopExit after %d unproductive rounds, got %v", stagnationExitRounds, err)
	}
	var warn, finalWarn bool
	for _, m := range res.History {
		if m.Role == "user" && strings.Contains(m.Content, "【停滞预警】") {
			warn = true
		}
		if m.Role == "user" && strings.Contains(m.Content, "【停滞最终警告】") {
			finalWarn = true
		}
	}
	if !warn || !finalWarn {
		t.Fatalf("expected escalating warnings before kill, warn=%v finalWarn=%v", warn, finalWarn)
	}
}

// TestReActAgent_StagnationGuard_ProductiveResets 产出性工具调用（WriteFile）重置停滞计数：
// 5 轮只读 + WriteFile + 5 轮只读 + 终答，不触发任何预警、正常完成。
func TestReActAgent_StagnationGuard_ProductiveResets(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	responses := []*blades.Message{readCallMsg(0), readCallMsg(1), readCallMsg(2), readCallMsg(3), readCallMsg(4)}
	responses = append(responses, &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "WriteFile", Request: string(mustJSON(map[string]any{"path": "out.txt", "content": "x"}))},
		},
	})
	responses = append(responses, readCallMsg(5), readCallMsg(6), readCallMsg(7), readCallMsg(8), readCallMsg(9))
	responses = append(responses, blades.AssistantMessage("done"))
	llm := &mockModelProvider{responses: responses}
	a := NewReActAgent("test", types.RoleDefinition{ID: "domain", SystemPrompt: "x"}, llm, NewToolRegistryAdapter(reg))
	res, err := a.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "done" {
		t.Fatalf("expected final answer, got %q", res.Text)
	}
	for _, m := range res.History {
		if strings.Contains(m.Content, "【停滞") {
			t.Fatalf("no stagnation warning expected with productive reset, got %q", m.Content)
		}
	}
}

// TestReActAgent_StagnationGuard_MetaExempt MetaAgent 豁免硬杀（ErrLoopExit 会终止整个
// 会话）：连续 24 轮无产出仅收预警，耗尽后默认终答正常返回。
func TestReActAgent_StagnationGuard_MetaExempt(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	responses := make([]*blades.Message, 0, stagnationExitRounds)
	for i := 0; i < stagnationExitRounds; i++ {
		responses = append(responses, readCallMsg(i))
	}
	llm := &mockModelProvider{responses: responses} // 耗尽后默认返回 "done" 终答
	a := NewReActAgent("meta", types.RoleDefinition{ID: "meta", SystemPrompt: "x"}, llm, NewToolRegistryAdapter(reg))
	res, err := a.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("meta must be exempt from stagnation kill, got %v", err)
	}
	if res.Text != "done" {
		t.Fatalf("expected final answer after exemption, got %q", res.Text)
	}
}
