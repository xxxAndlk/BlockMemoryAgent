// Package agent 包含 ReActAgent 的单元测试。
package agent

import (
	// context 用于传递测试请求上下文。
	"context"
	// encoding/json 用于在测试中构造工具调用的 JSON 参数。
	"encoding/json"
	// testing 提供 Go 标准测试框架。
	"testing"

	// tool 提供内建工具注册表，用于构造可调用的工具环境。
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	// types 提供角色定义等 DTO。
	"github.com/blockmemory/agent/backend/pkg/types"
	// blades 提供模型消息与 provider 接口。
	"github.com/go-kratos/blades"
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

// TestReActAgent_Run_MaxIterations 验证当模型持续请求工具调用且达到最大迭代次数时返回错误。
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

	_, err := agent.Run(context.Background(), "loop")
	if err == nil {
		t.Fatal("expected error for max iterations")
	}
}

// mustJSON 将任意值序列化为 JSON 字节切片，忽略序列化错误。
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
