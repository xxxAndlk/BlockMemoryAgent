package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// mockModelProvider is a programmable blades.ModelProvider for tests.
type mockModelProvider struct {
	responses []*blades.Message
	calls     int
}

func (m *mockModelProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if m.calls >= len(m.responses) {
		return &blades.ModelResponse{Message: blades.AssistantMessage("done")}, nil
	}
	resp := m.responses[m.calls]
	m.calls++
	return &blades.ModelResponse{Message: resp}, nil
}

func (m *mockModelProvider) Name() string { return "mock" }

func TestReActAgent_Run_NoTools(t *testing.T) {
	llm := &mockModelProvider{
		responses: []*blades.Message{
			blades.AssistantMessage("hello world"),
		},
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	agent := NewReActAgent("test", types.RoleDefinition{SystemPrompt: "You are a tester."}, llm, NewToolRegistryAdapter(reg))

	res, err := agent.Run(context.Background(), "say hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "hello world" {
		t.Fatalf("expected 'hello world', got %q", res.Text)
	}
	if len(res.History) != 2 {
		t.Fatalf("expected 2 history messages, got %d", len(res.History))
	}
}

func TestReActAgent_Run_WithToolCall(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(dir, nil, nil)

	// First model response requests a file write.
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
	if len(res.History) != 4 { // user, assistant(tool), tool, assistant
		t.Fatalf("expected 4 history messages, got %d", len(res.History))
	}
}

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

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
