package subagent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// mockProvider returns a fixed assistant message on every Generate call.
type mockProvider struct {
	text string
}

func (m *mockProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: blades.AssistantMessage(m.text)}, nil
}

func (m *mockProvider) Name() string { return "mock" }

// mockModelFactory always returns the mock provider.
type mockModelFactory struct {
	provider *mockProvider
}

func (f *mockModelFactory) GetBladesProvider(ctx context.Context, roleID string) (agent.ModelProvider, error) {
	return f.provider, nil
}

func TestDispatcher_RegisterAndCall(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta",
			ModelConfig:  types.AgentModelConfig{Provider: "mock"},
		},
		DomainAgent: config.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "mock"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, SystemPrompt: "code"},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "write tests",
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	if res.Output == "" {
		t.Fatal("expected non-empty sub-agent id")
	}

	// Wait for the asynchronous sub-agent to complete and deliver its summary.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs := mb.Drain("meta")
		if len(msgs) > 0 {
			if msgs[0].Body != "done" {
				t.Fatalf("expected summary 'done', got %q", msgs[0].Body)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for sub-agent mailbox message")
}

func TestDispatcher_CannotCallUncallable(t *testing.T) {
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: false},
		},
	}
	reg := role.NewRegistry(cfg)
	toolsReg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, nil, nil)
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "write tests",
	})
	if err == nil && res.Success {
		t.Fatal("expected failure for non-callable role")
	}
}
