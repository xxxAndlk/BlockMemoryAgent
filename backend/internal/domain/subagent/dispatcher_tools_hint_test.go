package subagent

// dispatcher_tools_hint_test.go 覆盖 TODO #52 执行项 4（tools_hint）：
// 派发时 tools_hint ∩ 子 Agent 角色权限天花板后预挂载进子 scope——子 Agent 当轮即可见
// 对应插件工具；天花板外越界项被忽略、随派发结果回告父 Agent（日志可查），不放大权限。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/google/jsonschema-go/jsonschema"
)

// hintPluginTool 是模拟插件工具（tool.Tool + SchemaSource）。
type hintPluginTool struct{ name string }

func (h *hintPluginTool) Name() string      { return h.name }
func (h *hintPluginTool) Aliases() []string { return nil }
func (h *hintPluginTool) Execute(context.Context, map[string]any) *tool.Result {
	return &tool.Result{Tool: h.name, Success: true}
}
func (h *hintPluginTool) Description() string             { return "插件工具 " + h.name }
func (h *hintPluginTool) InputSchema() *jsonschema.Schema { return nil }

// hintCeiling 是测试天花板：canvas_draw 全角色可见；computer_use_click 仅 meta。
func hintCeiling(roleID, name string) (owned, visible bool) {
	switch name {
	case "canvas_draw":
		return true, true
	case "computer_use_click":
		return true, roleID == "meta"
	}
	return false, false
}

// TestDispatch_ToolsHint 派发带 tools_hint：天花板内工具预挂载进子 scope；
// 越界项与未注册项被忽略并在结果中回告。
func TestDispatch_ToolsHint(t *testing.T) {
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
	toolsReg.SetPluginVisibility(hintCeiling)
	toolsReg.Register(&hintPluginTool{name: "canvas_draw"})
	toolsReg.Register(&hintPluginTool{name: "computer_use_click"})
	mb := mailbox.New()
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mb, agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	ctx := agent.WithAgentID(context.Background(), "meta")
	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "画 UI 页面",
		"verify_kind": "none",
		"tools_hint":  []any{"canvas_draw", "computer_use_click", "ghost_tool"},
	})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got: %s", res.Error)
	}
	// 越界项（computer_use_click 仅 meta）与未注册项（ghost_tool）回告父 Agent。
	if !strings.Contains(res.Output, "tools_hint 越界忽略") ||
		!strings.Contains(res.Output, "computer_use_click") ||
		!strings.Contains(res.Output, "ghost_tool") {
		t.Fatalf("越界/未注册 hint 应回告: %s", res.Output)
	}
	// 子 scope 预挂载：仅天花板内 canvas_draw。
	subID := strings.TrimSpace(res.Output)
	if i := strings.Index(subID, "。"); i >= 0 {
		subID = subID[:i]
	}
	mounted := toolsReg.MountedTools(subID)
	if !mounted["canvas_draw"] {
		t.Fatalf("天花板内 hint 应预挂载到子 scope %s: %v", subID, mounted)
	}
	if mounted["computer_use_click"] || mounted["ghost_tool"] {
		t.Fatalf("越界/未注册 hint 不应挂载: %v", mounted)
	}

	// 子 Agent 正常完成（邮箱收到通知，异步路径不因挂载受影响）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mb.Drain("meta")) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for sub-agent mailbox message")
}

// TestDispatch_NoToolsHint 无 tools_hint：子 scope 挂载集为空（默认收窄）。
func TestDispatch_NoToolsHint(t *testing.T) {
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
	toolsReg.SetPluginVisibility(hintCeiling)
	toolsReg.Register(&hintPluginTool{name: "canvas_draw"})
	d := NewDispatcher(reg, &mockModelFactory{provider: &mockProvider{text: "done"}}, toolsReg, mailbox.New(), agent.NopMemoryPipeline{})
	d.RegisterCallTool(toolsReg)

	res, err := toolsReg.Dispatch(agent.WithAgentID(context.Background(), "meta"), "call_sub_agent", map[string]any{
		"role_id":     "code_assistant",
		"task":        "写测试",
		"verify_kind": "none",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch: %v %s", err, res.Error)
	}
	subID := strings.TrimSpace(res.Output)
	if m := toolsReg.MountedTools(subID); len(m) != 0 {
		t.Fatalf("无 hint 时子 scope 挂载集应为空: %v", m)
	}
}
