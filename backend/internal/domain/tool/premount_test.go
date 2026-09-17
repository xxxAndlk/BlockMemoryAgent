package tool

// premount_test.go 覆盖配置预挂载（插件 settings.top_level → 顶层必备工具）：
//   - MountPreApprovedForScope 不经角色天花板、只挂已注册工具、幂等、按 scope 隔离；
//   - PreMountedTools 快照；
//   - 执行硬门对预挂名豁免（角色白名单外也放行，且只对预挂的那个 scope 放行）。

import (
	"context"
	"strings"
	"testing"
)

// TestMountPreApproved_BypassesCeiling 预挂载不看角色可见性天花板：
// 角色不可见的插件工具（mountVis 对 plug_b 恒 false）也可挂上，落进 preMounted 快照。
func TestMountPreApproved_BypassesCeiling(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis) // plug_b 任何角色都不可见
	r.Register(&fakePluginTool{name: "plug_b"})

	acc := r.MountPreApprovedForScope("sess-1", "doc_assistant", []string{"plug_b"})
	if len(acc) != 1 || acc[0] != "plug_b" {
		t.Fatalf("预挂载应绕过天花板接受 plug_b: acc=%v", acc)
	}
	if pre := r.PreMountedTools("sess-1"); !pre["plug_b"] {
		t.Fatalf("preMounted 快照应含 plug_b: %v", pre)
	}
	// 幂等：重复预挂不重复计数。
	if acc := r.MountPreApprovedForScope("sess-1", "doc_assistant", []string{"plug_b"}); len(acc) != 1 {
		t.Fatalf("重复预挂应幂等: acc=%v", acc)
	}
}

// TestMountPreApproved_RegisteredOnlyAndScopeIsolated 未注册工具静默跳过；
// 预挂只落在自己的 scope，其他 scope 的 preMounted 快照为空。
func TestMountPreApproved_RegisteredOnlyAndScopeIsolated(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.Register(&fakePluginTool{name: "plug_a"})

	acc := r.MountPreApprovedForScope("sess-1", "domain", []string{"plug_a", "ghost"})
	if len(acc) != 1 || acc[0] != "plug_a" {
		t.Fatalf("未注册工具应静默跳过: acc=%v", acc)
	}
	if pre := r.PreMountedTools("sess-2"); len(pre) != 0 {
		t.Fatalf("其他 scope 不应有预挂记录: %v", pre)
	}
	// 空 scope / 空名单零行为。
	if acc := r.MountPreApprovedForScope("", "domain", []string{"plug_a"}); acc != nil {
		t.Fatalf("空 scope 应跳过: %v", acc)
	}
	if acc := r.MountPreApprovedForScope("sess-1", "domain", nil); acc != nil {
		t.Fatalf("空名单应跳过: %v", acc)
	}
}

// TestRoleToolGate_PreMountedExempt 预挂名对执行硬门豁免：角色白名单不含它仍可执行；
// 同一工具在未预挂的 scope 上照旧被硬门拒绝（豁免按 scope 生效）。
func TestRoleToolGate_PreMountedExempt(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetRoleToolGateResolver(func(roleID string) []string { return []string{"ReadFile"} })
	r.Register(&fakePluginTool{name: "plug_a"})

	ctx := WithAgentID(WithRoleID(context.Background(), "doc_assistant"), "sess-1")
	r.MountPreApprovedForScope("sess-1", "doc_assistant", []string{"plug_a"})

	res, err := r.Dispatch(ctx, "plug_a", nil)
	if err != nil || !res.Success {
		t.Fatalf("预挂名应过硬门: success=%v err=%v res=%+v", res.Success, err, res)
	}

	// 另一 scope 未预挂：同一角色同一工具被硬门拒绝。
	ctx2 := WithAgentID(WithRoleID(context.Background(), "doc_assistant"), "sess-2")
	res2, err := r.Dispatch(ctx2, "plug_a", nil)
	if err != nil {
		t.Fatalf("硬门拒绝应返回工具级错误: err=%v", err)
	}
	if res2.Success || !strings.Contains(res2.Error, "白名单") {
		t.Fatalf("未预挂 scope 应被硬门拒绝: %+v", res2)
	}
}

// TestToolCatalog_ListsPreMounted 预挂工具在 tool_catalog 中列出并标 ✓，
// 且不因角色天花板不可见而缺席（否则模型误判"没有这个能力"）。
func TestToolCatalog_ListsPreMounted(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis) // plug_b 恒不可见
	r.Register(&fakePluginTool{name: "plug_b"})
	r.SetPluginManager(&fakePluginManager{infos: []PluginInfo{
		{ID: "plug", Name: "plug", Kind: "mcp", State: "running", Enabled: true, Tools: []string{"plug_b"}},
	}})
	r.MountPreApprovedForScope("sess-1", "doc_assistant", []string{"plug_b"})

	ctx := WithAgentID(WithRoleID(context.Background(), "doc_assistant"), "sess-1")
	res, err := r.Dispatch(ctx, "tool_catalog", nil)
	if err != nil || !res.Success {
		t.Fatalf("tool_catalog 应成功: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Output, "plug_b") || !strings.Contains(res.Output, "✓") {
		t.Fatalf("预挂工具应列出并标 ✓: %s", res.Output)
	}
}
