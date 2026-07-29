// Package agent 包含 ReActAgent 的单元测试。
package agent

import (
	// context 用于传递测试请求上下文。
	"context"
	// encoding/json 用于在测试中构造工具调用的 JSON 参数。
	"encoding/json"
	// errors 用于构造模拟的瞬时 LLM 错误。
	"errors"
	// strings 用于构造长字符串与断言内容。
	"strings"
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
		_ = f.mb.Send(&mailbox.Message{From: "session-1/code_assistant-1", To: parentID, Type: mailbox.MsgInfo, Body: "子 Agent 完成: ok"})
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
			blades.AssistantMessage("整合完毕：ok"),    // 第二轮：吸收 mailbox 摘要后给最终答复
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
