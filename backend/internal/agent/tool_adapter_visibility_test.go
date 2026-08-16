package agent

// tool_adapter_visibility_test.go 覆盖设计文档 §4.3 + TODO #52：
// 白名单 adapter 的 Schema() = 静态白名单 ∪（插件动态可见集 ∩ 本 Agent 已挂载集）。
// 权限天花板（roles 白名单）由 visibility 回调表达；Agent 只能在天花板内挂载/使用，
// 默认可见集收窄为角色基础工具（插件工具按需 tool_mount / 派发 tools_hint 挂载）。

import (
	"context"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

// visTool 是模拟插件工具（tool.Tool + SchemaSource）。
type visTool struct{ name string }

func (v *visTool) Name() string      { return v.name }
func (v *visTool) Aliases() []string { return nil }
func (v *visTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	return &tool.Result{Tool: v.name, Success: true}
}
func (v *visTool) Description() string             { return "插件工具 " + v.name }
func (v *visTool) InputSchema() *jsonschema.Schema { return nil }

func schemaNames(ts []tools.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[t.Name()] = true
	}
	return out
}

// visCeiling 是测试用权限天花板：web_search 仅 meta 可见，secret_tool 任何角色都不可见。
func visCeiling(roleID, name string) (owned, visible bool) {
	switch name {
	case "web_search":
		return true, roleID == "meta"
	case "secret_tool":
		return true, false
	}
	return false, false // 非插件工具：白名单语义不变
}

// TestAdapterVisibilityMountNarrows 验证 TODO #52 收窄语义：
// 插件工具默认不可见（即使天花板放行），挂载后可见，越界挂载被拒绝且不可见。
func TestAdapterVisibilityMountNarrows(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.SetPluginVisibility(visCeiling)
	reg.Register(&visTool{name: "web_search"})
	reg.Register(&visTool{name: "secret_tool"})

	const scope = "meta-1"
	adapter := NewToolRegistryAdapterForRole(reg, scope, []string{"ReadFile", "RunCommand"}, "meta", visCeiling)
	names := schemaNames(adapter.Schema())
	if names["web_search"] {
		t.Fatal("默认收窄：未挂载的插件工具不应暴露（即使天花板放行）")
	}
	if names["secret_tool"] {
		t.Fatal("动态可见集拒绝的插件工具不应暴露")
	}
	if !names["ReadFile"] || !names["RunCommand"] {
		t.Fatal("静态白名单工具应保留")
	}
	if names["WriteFile"] {
		t.Fatal("白名单外的普通工具不应暴露")
	}

	// 挂载天花板内的 web_search → 下一轮 Schema 可见。
	if acc, rej := reg.MountForScope(scope, "meta", []string{"web_search"}); len(acc) != 1 || len(rej) != 0 {
		t.Fatalf("天花板内挂载应全部接受: acc=%v rej=%v", acc, rej)
	}
	names = schemaNames(adapter.Schema())
	if !names["web_search"] {
		t.Fatal("挂载后插件工具应对本 scope 可见")
	}

	// 越界挂载（secret_tool 天花板拒绝）→ 拒绝且不可见。
	if acc, rej := reg.MountForScope(scope, "meta", []string{"secret_tool"}); len(acc) != 0 || len(rej) != 1 {
		t.Fatalf("越界挂载应全部拒绝: acc=%v rej=%v", acc, rej)
	}
	if schemaNames(adapter.Schema())["secret_tool"] {
		t.Fatal("越界挂载的工具不应暴露")
	}

	// 非插件工具挂载 → 拒绝（白名单管理，无需挂载）。
	if acc, rej := reg.MountForScope(scope, "meta", []string{"WriteFile"}); len(acc) != 0 || len(rej) != 1 {
		t.Fatalf("非插件工具挂载应拒绝: acc=%v rej=%v", acc, rej)
	}

	// 卸载 → 下一轮不可见。
	reg.UnmountTools(scope, []string{"web_search"})
	if schemaNames(adapter.Schema())["web_search"] {
		t.Fatal("卸载后插件工具不应再暴露")
	}
}

// TestAdapterMountScopeIsolation 验证挂载集按 scope（agentID）隔离：
// meta 挂载不影响 domain 子 Agent 的可见集；domain 越界挂载被拒绝。
func TestAdapterMountScopeIsolation(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.SetPluginVisibility(visCeiling)
	reg.Register(&visTool{name: "web_search"})

	reg.MountForScope("session-1", "meta", []string{"web_search"})

	meta := NewToolRegistryAdapterForRole(reg, "session-1", nil, "meta", visCeiling)
	if !schemaNames(meta.Schema())["web_search"] {
		t.Fatal("meta scope 挂载后应可见")
	}
	// 兄弟/子 Agent scope 独立：未挂载则不可见。
	domain := NewToolRegistryAdapterForRole(reg, "sub/domain-1", nil, "domain", visCeiling)
	if schemaNames(domain.Schema())["web_search"] {
		t.Fatal("挂载集按 scope 隔离，子 Agent 不应看到 meta 的挂载")
	}
	// domain 天花板不允许 web_search：挂载被拒绝。
	if acc, rej := reg.MountForScope("sub/domain-1", "domain", []string{"web_search"}); len(acc) != 0 || len(rej) != 1 {
		t.Fatalf("domain 越界挂载应拒绝: acc=%v rej=%v", acc, rej)
	}
}

// TestAdapterVisibilityDisabledPlugin 验证：插件工具被摘除（disable）后
// 不再出现在任何角色的 Schema 中（注册表层面已消失，挂载集残留无影响）。
func TestAdapterVisibilityDisabledPlugin(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	vis := func(roleID, name string) (bool, bool) { return name == "web_search", true }
	reg.SetPluginVisibility(vis)
	reg.Register(&visTool{name: "web_search"})
	adapter := NewToolRegistryAdapterForRole(reg, "m", nil, "meta", vis)
	reg.MountForScope("m", "meta", []string{"web_search"})
	if !schemaNames(adapter.Schema())["web_search"] {
		t.Fatal("挂载且启用中应可见")
	}
	reg.Unregister("web_search")
	if schemaNames(adapter.Schema())["web_search"] {
		t.Fatal("摘除后不应可见")
	}
}

// TestAdapterNilVisibility 验证：可见性回调为 nil 时行为与旧版一致（纯白名单）。
func TestAdapterNilVisibility(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.Register(&visTool{name: "web_search"})
	adapter := NewToolRegistryAdapterForRole(reg, "m", []string{"ReadFile"}, "meta", nil)
	names := schemaNames(adapter.Schema())
	if names["web_search"] {
		t.Fatal("无可见性回调时白名单外插件工具不应暴露（向后兼容）")
	}
	if !names["ReadFile"] {
		t.Fatal("白名单工具应保留")
	}
}
