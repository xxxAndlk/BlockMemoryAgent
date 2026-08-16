package agent

// tool_adapter_visibility_test.go 覆盖设计文档 §4.3：
// 白名单 adapter 的 Schema() = 静态白名单 ∪ 插件动态可见集（pluginVisibility 回调）。

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

// TestAdapterVisibilityUnion 验证：白名单未命中的插件工具经动态可见集暴露；
// 可见集拒绝的插件工具不暴露；非插件工具不受影响。
func TestAdapterVisibilityUnion(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.Register(&visTool{name: "web_search"})
	reg.Register(&visTool{name: "secret_tool"})

	// meta 角色：白名单仅内置工具；插件可见集放行 web_search、拒绝 secret_tool。
	visibility := func(roleID, name string) (owned, visible bool) {
		switch name {
		case "web_search":
			return true, roleID == "meta"
		case "secret_tool":
			return true, false
		}
		return false, false // 非插件工具：白名单语义不变
	}
	adapter := NewToolRegistryAdapterForRole(reg, []string{"ReadFile", "RunCommand"}, "meta", visibility)
	names := schemaNames(adapter.Schema())
	if !names["web_search"] {
		t.Fatal("白名单 ∪ 动态可见集：web_search 应对 meta 可见")
	}
	if names["secret_tool"] {
		t.Fatal("动态可见集拒绝的插件工具不应暴露")
	}
	if !names["ReadFile"] || !names["RunCommand"] {
		t.Fatal("静态白名单工具应保留")
	}
	// 白名单外的普通内置工具不暴露（WriteFile 不在 meta 白名单）。
	if names["WriteFile"] {
		t.Fatal("白名单外的普通工具不应暴露")
	}

	// 另一角色（domain）无白名单：仅可见集生效。
	domain := NewToolRegistryAdapterForRole(reg, nil, "domain", visibility)
	dNames := schemaNames(domain.Schema())
	if dNames["web_search"] {
		t.Fatal("web_search 仅对 meta 可见，domain 不应看到")
	}
	if !dNames["ReadFile"] {
		t.Fatal("domain 无白名单应看到全部普通工具")
	}
}

// TestAdapterVisibilityDisabledPlugin 验证：插件工具被摘除（disable）后
// 不再出现在任何角色的 Schema 中（注册表层面已消失）。
func TestAdapterVisibilityDisabledPlugin(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	reg.Register(&visTool{name: "web_search"})
	adapter := NewToolRegistryAdapterForRole(reg, nil, "meta", func(roleID, name string) (bool, bool) {
		return name == "web_search", true
	})
	if !schemaNames(adapter.Schema())["web_search"] {
		t.Fatal("启用中应可见")
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
	adapter := NewToolRegistryAdapterForRole(reg, []string{"ReadFile"}, "meta", nil)
	names := schemaNames(adapter.Schema())
	if names["web_search"] {
		t.Fatal("无可见性回调时白名单外插件工具不应暴露（向后兼容）")
	}
	if !names["ReadFile"] {
		t.Fatal("白名单工具应保留")
	}
}
