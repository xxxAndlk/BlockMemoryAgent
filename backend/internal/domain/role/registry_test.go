package role

import (
	"testing"

	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestRegistry_Get_MetaAndFixed(t *testing.T) {
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{
			SystemPrompt: "meta prompt",
			ModelConfig:  types.AgentModelConfig{Model: "meta-model"},
		},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
			{ID: "ui_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}},
		},
	}
	r := NewRegistry(cfg)

	meta := r.Get("meta")
	if meta == nil || meta.SystemPrompt != "meta prompt" {
		t.Fatalf("unexpected meta role: %+v", meta)
	}

	code := r.Get("code_assistant")
	if code == nil || code.ID != "code_assistant" {
		t.Fatalf("unexpected code role: %+v", code)
	}

	if r.Get("missing") != nil {
		t.Fatal("expected nil for missing role")
	}
}

func TestRegistry_CanCall(t *testing.T) {
	cfg := &config.RoleConfigFile{
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true},
			{ID: "ui_assistant", Type: enums.RoleTypeFixed, CanBeCalled: true, Parents: []string{"code_assistant"}},
			{ID: "private", Type: enums.RoleTypeFixed, CanBeCalled: false},
		},
	}
	r := NewRegistry(cfg)

	if !r.CanCall("meta", "code_assistant") {
		t.Error("meta should be able to call callable fixed role")
	}
	if r.CanCall("meta", "private") {
		t.Error("meta should not be able to call non-callable fixed role")
	}
	if !r.CanCall("ui_assistant", "code_assistant") {
		t.Error("ui_assistant parent permission should allow calling code_assistant")
	}
	if r.CanCall("code_assistant", "ui_assistant") {
		t.Error("code_assistant should not be able to call ui_assistant without parent")
	}
}
