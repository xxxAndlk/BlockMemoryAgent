package tool

// mount_tools_test.go 覆盖 TODO #52 按需挂载机制：
//   - MountForScope 天花板校验（接受/越界拒绝/非插件拒绝/未注册拒绝/空 scope 拒绝）；
//   - 挂载集 scope 隔离与幂等；
//   - tool_catalog 分组枚举 + 描述 + 已挂载标注（无 schema）；
//   - tool_mount / tool_unmount 工具闭环；
//   - plugin_install 成功后自动挂载（天花板内），越界项回告。

import (
	"context"
	"strings"
	"testing"
)

// mountVis 是测试天花板：plug_a/seq_tool/p_tool 仅 meta 可见；plug_b 任何角色不可见。
func mountVis(roleID, name string) (owned, visible bool) {
	switch name {
	case "plug_a", "seq_tool", "p_tool":
		return true, roleID == "meta"
	case "plug_b":
		return true, false
	}
	return false, false
}

// TestMountForScope_CeilingValidation 天花板校验全分支。
func TestMountForScope_CeilingValidation(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "plug_a"})
	r.Register(&fakePluginTool{name: "plug_b"})

	// 天花板内：接受。
	if acc, rej := r.MountForScope("scope-1", "meta", []string{"plug_a"}); len(acc) != 1 || len(rej) != 0 {
		t.Fatalf("天花板内挂载应接受: acc=%v rej=%v", acc, rej)
	}
	// 幂等：重复挂载 no-op 但计入 accepted。
	if acc, rej := r.MountForScope("scope-1", "meta", []string{"plug_a"}); len(acc) != 1 || len(rej) != 0 {
		t.Fatalf("重复挂载应幂等接受: acc=%v rej=%v", acc, rej)
	}
	// 越界（角色不可见）：拒绝且原因含天花板字样。
	if acc, rej := r.MountForScope("scope-1", "meta", []string{"plug_b"}); len(acc) != 0 || len(rej) != 1 || !strings.Contains(rej[0], "天花板") {
		t.Fatalf("越界挂载应拒绝并说明: acc=%v rej=%v", acc, rej)
	}
	// 非插件工具：拒绝（白名单管理，无需挂载）。
	if acc, rej := r.MountForScope("scope-1", "meta", []string{"ReadFile"}); len(acc) != 0 || len(rej) != 1 || !strings.Contains(rej[0], "非插件工具") {
		t.Fatalf("非插件工具挂载应拒绝: acc=%v rej=%v", acc, rej)
	}
	// 未注册工具：拒绝。
	if acc, rej := r.MountForScope("scope-1", "meta", []string{"ghost"}); len(acc) != 0 || len(rej) != 1 || !strings.Contains(rej[0], "未注册") {
		t.Fatalf("未注册工具挂载应拒绝: acc=%v rej=%v", acc, rej)
	}
	// 空 scope：全部拒绝（挂载无处落地）。
	if acc, rej := r.MountForScope("", "meta", []string{"plug_a"}); len(acc) != 0 || len(rej) != 1 {
		t.Fatalf("空 scope 应拒绝: acc=%v rej=%v", acc, rej)
	}
	// MountedTools 快照只含已挂载项。
	mounted := r.MountedTools("scope-1")
	if len(mounted) != 1 || !mounted["plug_a"] {
		t.Fatalf("MountedTools 快照不符: %v", mounted)
	}
	// 未挂载 scope：空 map（非 nil）。
	if m := r.MountedTools("nope"); m == nil || len(m) != 0 {
		t.Fatalf("无挂载记录 scope 应返空 map: %v", m)
	}
}

// TestMountForScope_ScopeIsolation 挂载集按 scope 隔离；卸载后可见集收缩。
func TestMountForScope_ScopeIsolation(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "plug_a"})

	r.MountForScope("parent", "meta", []string{"plug_a"})
	r.MountForScope("child-1", "meta", []string{"plug_a"})

	// child-2 未挂载。
	if m := r.MountedTools("parent"); !m["plug_a"] {
		t.Fatal("parent 应含挂载")
	}
	if m := r.MountedTools("child-1"); !m["plug_a"] {
		t.Fatal("child-1 应含挂载")
	}
	if m := r.MountedTools("child-2"); len(m) != 0 {
		t.Fatal("child-2 不应含挂载")
	}

	// 卸载 child-1 不影响 parent。
	r.UnmountTools("child-1", []string{"plug_a"})
	if m := r.MountedTools("parent"); !m["plug_a"] {
		t.Fatal("parent 挂载不应被兄弟卸载影响")
	}
	if m := r.MountedTools("child-1"); len(m) != 0 {
		t.Fatal("child-1 卸载后应为空")
	}
	// 卸载未挂载工具 no-op。
	r.UnmountTools("child-1", []string{"plug_a"})
}

// TestToolCatalog_GroupedListing tool_catalog 按插件分组枚举天花板内工具，
// 含描述与已挂载标注，不含入参 schema（Description 之外无 JSON 结构）。
func TestToolCatalog_GroupedListing(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "plug_a"})
	r.Register(&fakePluginTool{name: "plug_b"})
	fake := &fakePluginManager{infos: []PluginInfo{
		{ID: "seq", Name: "Sequential Thinking", State: "running", Tools: []string{"plug_a", "plug_b"}},
	}}
	r.SetPluginManager(fake)

	ctx := WithRoleID(WithAgentID(context.Background(), "meta-1"), "meta")
	// 通过 Dispatch 走正式路径（含 ctx 作用域）。
	out := r.DispatchForTest(ctx, "tool_catalog", nil)

	body := out.Output
	if !strings.Contains(body, "【插件 seq】") {
		t.Fatalf("应按插件分组输出，got: %s", body)
	}
	if !strings.Contains(body, "Sequential Thinking") {
		t.Fatalf("应含插件名，got: %s", body)
	}
	if !strings.Contains(body, "plug_a") || !strings.Contains(body, "插件测试工具 plug_a") {
		t.Fatalf("应含天花板内工具与一句话描述，got: %s", body)
	}
	if strings.Contains(body, "plug_b") {
		t.Fatalf("天花板外工具不应列出，got: %s", body)
	}
	// 无 schema：输出不应出现 JSON 结构痕迹（引号键/花括号工具定义）。
	if strings.Contains(body, `"type":`) || strings.Contains(body, `"properties":`) {
		t.Fatalf("catalog 不应含入参 schema，got: %s", body)
	}
	// 未挂载时无 ✓ 标注。
	if strings.Contains(body, "✓ plug_a") {
		t.Fatalf("未挂载不应标 ✓，got: %s", body)
	}
}

// TestToolCatalog_Unwired 未注入管理面：返回未接线错误。
func TestToolCatalog_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := WithRoleID(WithAgentID(context.Background(), "m"), "meta")
	out := r.DispatchForTest(ctx, "tool_catalog", nil)
	if out.Success || !strings.Contains(out.Error, "未接线") {
		t.Fatalf("未接线应报错: %+v", out)
	}
}

// TestToolMount_AcceptAndReject tool_mount 闭环：天花板内接受、越界拒绝并说明。
func TestToolMount_AcceptAndReject(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "plug_a"})
	r.Register(&fakePluginTool{name: "plug_b"})

	ctx := WithRoleID(WithAgentID(context.Background(), "meta-1"), "meta")

	// 混合：plug_a 接受、plug_b 拒绝。
	out := r.DispatchForTest(ctx, "tool_mount", map[string]any{"tools": []any{"plug_a", "plug_b"}, "reason": "测试"})
	if !out.Success || !strings.Contains(out.Output, "已挂载 1 个") || !strings.Contains(out.Output, "拒绝 1 个") {
		t.Fatalf("混合挂载结果不符: %+v", out)
	}
	if !strings.Contains(out.Output, "plug_b") || !strings.Contains(out.Output, "天花板") {
		t.Fatalf("越界拒绝应说明原因: %+v", out)
	}
	// 挂载生效：catalog 标注 ✓。
	fake := &fakePluginManager{infos: []PluginInfo{{ID: "seq", State: "running", Tools: []string{"plug_a"}}}}
	r.SetPluginManager(fake)
	cat := r.DispatchForTest(ctx, "tool_catalog", nil)
	if !strings.Contains(cat.Output, "✓ plug_a") {
		t.Fatalf("已挂载工具应标 ✓，got: %s", cat.Output)
	}
}

// TestToolMount_EmptyArgs 空 tools：校验拒绝。
func TestToolMount_EmptyArgs(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	ctx := WithRoleID(WithAgentID(context.Background(), "m"), "meta")
	out := r.DispatchForTest(ctx, "tool_mount", map[string]any{})
	if out.Category != ResultCategoryValidationRejected {
		t.Fatalf("空 tools 应校验拒绝: %+v", out)
	}
	out2 := r.DispatchForTest(ctx, "tool_unmount", map[string]any{})
	if out2.Category != ResultCategoryValidationRejected {
		t.Fatalf("空 tools 应校验拒绝: %+v", out2)
	}
}

// TestToolUnmount_Removes tool_unmount 卸载后 schema 侧不可见、catalog 无 ✓。
func TestToolUnmount_Removes(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "plug_a"})
	ctx := WithRoleID(WithAgentID(context.Background(), "meta-1"), "meta")

	r.DispatchForTest(ctx, "tool_mount", map[string]any{"tools": []any{"plug_a"}})
	if m := r.MountedTools("meta-1"); !m["plug_a"] {
		t.Fatal("挂载后应可见")
	}
	out := r.DispatchForTest(ctx, "tool_unmount", map[string]any{"tools": []any{"plug_a"}})
	if !out.Success || !strings.Contains(out.Output, "已卸载 1 个") {
		t.Fatalf("卸载结果不符: %+v", out)
	}
	if m := r.MountedTools("meta-1"); len(m) != 0 {
		t.Fatal("卸载后挂载集应为空")
	}
}

// TestPluginInstall_AutoMount plugin_install 成功后自动挂载新工具到调用者作用域；
// 天花板外工具不挂载并在输出中回告。
func TestPluginInstall_AutoMount(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	r.Register(&fakePluginTool{name: "seq_tool"})
	r.Register(&fakePluginTool{name: "plug_b"})
	fake := &fakePluginManager{}
	r.SetPluginManager(fake)

	ctx := WithRoleID(WithAgentID(context.Background(), "meta-1"), "meta")

	// 无审批 hook（nil）：直接执行。
	out := r.DispatchForTest(ctx, "plugin_install", map[string]any{"id": "seq"})
	if !out.Success {
		t.Fatalf("安装应成功: %+v", out)
	}
	// fake Install 返回工具 [<id>_tool] = [seq_tool]：已挂载到 meta-1。
	m := r.MountedTools("meta-1")
	if !m["seq_tool"] {
		t.Fatalf("安装成功应自动挂载工具: %v", m)
	}
	if !strings.Contains(out.Output, "已自动挂载") {
		t.Fatalf("输出应含自动挂载说明: %s", out.Output)
	}
}

// TestPluginInstall_AutoMountCeiling 安装的插件工具超出调用者天花板：不挂载并回告。
func TestPluginInstall_AutoMountCeiling(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r.SetPluginVisibility(mountVis)
	// fake Install 对 id "p" 返回工具 "p_tool"：仅 meta 可见。
	r.Register(&fakePluginTool{name: "p_tool"})
	fake := &fakePluginManager{}
	r.SetPluginManager(fake)

	// domain 角色安装：p_tool 超出 domain 天花板 → 拒绝挂载并回告。
	ctx := WithRoleID(WithAgentID(context.Background(), "dom-1"), "domain")
	out := r.DispatchForTest(ctx, "plugin_install", map[string]any{"id": "p"})
	if !out.Success {
		t.Fatalf("安装本身应成功: %+v", out)
	}
	if !strings.Contains(out.Output, "未挂载") || !strings.Contains(out.Output, "p_tool") {
		t.Fatalf("越界自动挂载应回告: %s", out.Output)
	}
	if m := r.MountedTools("dom-1"); len(m) != 0 {
		t.Fatalf("天花板外工具不应挂载: %v", m)
	}
}

// DispatchForTest 是测试便捷入口：走正式 Dispatch 路径（含 ctx 注入）。
func (r *Registry) DispatchForTest(ctx context.Context, name string, args map[string]any) *Result {
	res, _ := r.Dispatch(ctx, name, args)
	if res == nil {
		return &Result{Tool: name, Error: "nil result"}
	}
	return res
}
