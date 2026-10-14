package agent

// top_level_essentials_test.go 顶层必备工具预挂（2026-10-14 内建 widget 工具）用例：
// 三档顶层（fast/daily/cluster）的会话 scope 都应看到 widget_save/widget_list
//（即使插件 provider 未接线）；子 Agent 的独立 scope 不受影响——角色列表未动，
// 权限靠 MountPreApprovedForScope 的 preMounted 直通，仅挂顶层 scope。

import (
	"testing"
)

// TestMountTopLevelEssentials_BuiltinWidgets 三档顶层预挂内建 widget 工具：
// Schema 直通可见；未预挂的其他 scope（子 Agent 视角）不可见。
func TestMountTopLevelEssentials_BuiltinWidgets(t *testing.T) {
	svc := newAskUserTestService(t, &mockReactModelProvider{})
	if svc.toolRegistry == nil {
		t.Skip("toolRegistry 未接线")
	}
	svc.topLevelTools = nil // 模拟插件 provider 未接线：内建工具仍应预挂（不依赖插件）

	schemaNames := func(scope string) map[string]bool {
		adapter := NewToolRegistryAdapterForRole(svc.toolRegistry, scope, []string{"ReadFile"}, "domain", nil)
		names := make(map[string]bool)
		for _, def := range adapter.Schema() {
			names[def.Name()] = true
		}
		return names
	}

	for _, gear := range []string{"fast", "daily", "cluster"} {
		scope := "sess-" + gear
		svc.mountTopLevelEssentials(scope, gear)
		names := schemaNames(scope)
		if !names["widget_save"] || !names["widget_list"] {
			t.Fatalf("gear=%s 顶层 scope 应预挂 widget 工具, got %v", gear, names)
		}
	}

	// 子 Agent 独立 scope（如 daily 顶层派发的 domain 子实例）：未预挂，白名单外不可见。
	sub := schemaNames("sess-daily/domain-1")
	if sub["widget_save"] || sub["widget_list"] {
		t.Fatalf("子 Agent scope 不应看到 widget 工具: %v", sub)
	}
}
