package tool

// plugin_tools_test.go 覆盖插件管理工具组（TODO #51）：
//   - 未接线：五工具全部返回"未接线"错误；
//   - plugin_search：检索结果与不可用来源 note 格式化透传；
//   - plugin_install：目录条目安装 / 手工 manifest 组装 / 缺 id 校验拒绝 /
//     Destructive 审批门（允许放行、拒绝不执行）；
//   - plugin_enable / plugin_disable：透传 Manager 并带结果信息；
//   - plugin_list：列表格式化；
//   - Schema 暴露：五工具经 SchemaSource 兜底路径对 LLM 可见。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// fakePluginManager 是 PluginManager 的测试替身。
type fakePluginManager struct {
	installed atomic.Int32 // Install 调用计数（审批拒绝断言用）
	installReq InstallRequest
	searchCalls atomic.Int32
	enableCalls atomic.Int32
	disableCalls atomic.Int32
	infos  []PluginInfo
	search []SearchResult
	notes  []string
}

func (f *fakePluginManager) List() []PluginInfo { return f.infos }
func (f *fakePluginManager) Get(id string) (PluginInfo, bool) {
	for _, p := range f.infos {
		if p.ID == id {
			return p, true
		}
	}
	return PluginInfo{}, false
}
func (f *fakePluginManager) Enable(ctx context.Context, id string) error {
	f.enableCalls.Add(1)
	return nil
}
func (f *fakePluginManager) Disable(ctx context.Context, id string) error {
	f.disableCalls.Add(1)
	return nil
}
func (f *fakePluginManager) Install(ctx context.Context, req InstallRequest) (PluginInfo, error) {
	f.installed.Add(1)
	f.installReq = req
	return PluginInfo{ID: req.ID, Kind: "mcp", State: "running", Enabled: req.Enabled, Tools: []string{req.ID + "_tool"}}, nil
}
func (f *fakePluginManager) Search(ctx context.Context, query string) ([]SearchResult, []string) {
	f.searchCalls.Add(1)
	return f.search, f.notes
}

// newPluginToolsRegistry 构造注册了 plugin_* 工具并注入 fake 管理面的 Registry。
func newPluginToolsRegistry(t *testing.T, mgr PluginManager) (*Registry, *fakePluginManager) {
	t.Helper()
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	fake := mgr.(*fakePluginManager)
	r.SetPluginManager(fake)
	return r, fake
}

// TestPluginTools_Unwired 未注入管理面：五工具返回未接线错误。
func TestPluginTools_Unwired(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	for _, name := range []string{"plugin_search", "plugin_install", "plugin_enable", "plugin_disable", "plugin_list"} {
		res, _ := r.Dispatch(context.Background(), name, map[string]any{"query": "x", "id": "x"})
		if res.Success || !strings.Contains(res.Error, "未接线") {
			t.Fatalf("%s 未接线应报错，got %+v", name, res)
		}
	}
}

// TestPluginSearch_Formatting 检索结果格式化：条目 + 来源 + note 透传。
func TestPluginSearch_Formatting(t *testing.T) {
	fake := &fakePluginManager{
		search: []SearchResult{
			{ID: "sequential_thinking", Name: "Sequential Thinking", Kind: "mcp", Source: "builtin", Description: "思维链"},
			{ID: "web_search", Kind: "mcp", Source: "config", State: "running", Tools: []string{"search", "scrape"}},
		},
		notes: []string{"registry registry.modelcontextprotocol.io 不可用: timeout"},
	}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_search", map[string]any{"query": "搜索"})
	if !res.Success {
		t.Fatalf("search 应成功，got %+v", res)
	}
	out := res.Output
	for _, want := range []string{"sequential_thinking", "思维链", "内置目录", "web_search", "已配置", "状态=running", "工具=search,scrape", "plugin_install(id=", "registry.modelcontextprotocol.io 不可用"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺 %q: %s", want, out)
		}
	}
}

// TestPluginInstall_ByID 目录条目安装：请求只带 id，默认 enabled=true。
func TestPluginInstall_ByID(t *testing.T) {
	fake := &fakePluginManager{}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_install", map[string]any{"id": "fetch"})
	if !res.Success {
		t.Fatalf("install 应成功，got %+v", res)
	}
	if fake.installed.Load() != 1 || fake.installReq.ID != "fetch" || !fake.installReq.Enabled {
		t.Fatalf("install 请求透传错误: %+v", fake.installReq)
	}
	if !strings.Contains(res.Output, "安装成功") || !strings.Contains(res.Output, "fetch_tool") {
		t.Fatalf("输出缺安装结果: %s", res.Output)
	}
}

// TestPluginInstall_ManualManifest 手工 manifest：transport/command/args/env 组装进 Settings。
func TestPluginInstall_ManualManifest(t *testing.T) {
	fake := &fakePluginManager{}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_install", map[string]any{
		"id":        "custom",
		"transport": "stdio",
		"command":   "uvx",
		"args":      []any{"my-mcp"},
		"env":       map[string]any{"KEY": "val"},
		"roles":     []any{"meta"},
	})
	if !res.Success {
		t.Fatalf("install 应成功，got %+v", res)
	}
	req := fake.installReq
	if req.Settings["transport"] != "stdio" || req.Settings["command"] != "uvx" {
		t.Fatalf("settings 组装错误: %+v", req.Settings)
	}
	args, _ := req.Settings["args"].([]any)
	if len(args) != 1 || args[0] != "my-mcp" {
		t.Fatalf("args 组装错误: %v", req.Settings["args"])
	}
	env, _ := req.Settings["env"].(map[string]string)
	if env["KEY"] != "val" {
		t.Fatalf("env 组装错误: %v", req.Settings["env"])
	}
	if len(req.Roles) != 1 || req.Roles[0] != "meta" {
		t.Fatalf("roles 组装错误: %v", req.Roles)
	}
}

// TestPluginInstall_MissingID 缺 id：校验拒绝（validation_rejected）。
func TestPluginInstall_MissingID(t *testing.T) {
	fake := &fakePluginManager{}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_install", map[string]any{})
	if res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("缺 id 应校验拒绝，got %+v", res)
	}
	if fake.installed.Load() != 0 {
		t.Fatal("校验拒绝不应调用 Install")
	}
}

// TestPluginInstall_ApprovalGate 审批门：Destructive 工具经 approvalHook 二次确认；
// 拒绝不执行，放行才调用 Manager。
func TestPluginInstall_ApprovalGate(t *testing.T) {
	// 拒绝路径。
	fake := &fakePluginManager{}
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	var hookName string
	r.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		hookName = name
		return false, nil
	})
	r.SetPluginManager(fake)
	res, _ := r.Dispatch(context.Background(), "plugin_install", map[string]any{"id": "fetch"})
	if res.Success || !strings.Contains(res.Error, "已被用户拒绝") {
		t.Fatalf("拒绝后应失败，got %+v", res)
	}
	if hookName != "plugin_install" {
		t.Fatalf("审批应针对 plugin_install，got %q", hookName)
	}
	if fake.installed.Load() != 0 {
		t.Fatal("审批拒绝不应调用 Install")
	}
	// 放行路径。
	fake2 := &fakePluginManager{}
	r2 := NewBuiltinRegistry(t.TempDir(), nil, nil)
	r2.SetApprovalHook(func(ctx context.Context, name string, args map[string]any) (bool, error) {
		return true, nil
	})
	r2.SetPluginManager(fake2)
	res2, _ := r2.Dispatch(context.Background(), "plugin_install", map[string]any{"id": "fetch"})
	if !res2.Success || fake2.installed.Load() != 1 {
		t.Fatalf("审批放行后应安装成功，got %+v", res2)
	}
}

// TestPluginToggle_EnableDisable enable/disable 透传并带结果信息。
func TestPluginToggle_EnableDisable(t *testing.T) {
	fake := &fakePluginManager{
		infos: []PluginInfo{{ID: "web_search", Kind: "mcp", State: "running", Enabled: true, Tools: []string{"search"}}},
	}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_enable", map[string]any{"id": "web_search"})
	if !res.Success || fake.enableCalls.Load() != 1 || !strings.Contains(res.Output, "已启用") {
		t.Fatalf("enable 失败: %+v", res)
	}
	res, _ = r.Dispatch(context.Background(), "plugin_disable", map[string]any{"id": "web_search"})
	if !res.Success || fake.disableCalls.Load() != 1 || !strings.Contains(res.Output, "已停用") {
		t.Fatalf("disable 失败: %+v", res)
	}
	// 缺 id 校验拒绝。
	res, _ = r.Dispatch(context.Background(), "plugin_enable", map[string]any{})
	if res.Success || res.Category != ResultCategoryValidationRejected {
		t.Fatalf("缺 id 应校验拒绝，got %+v", res)
	}
}

// TestPluginList_Formatting 列表格式化：id/形态/状态/工具/缺 env。
func TestPluginList_Formatting(t *testing.T) {
	fake := &fakePluginManager{
		infos: []PluginInfo{
			{ID: "web_search", Kind: "mcp", State: "running", Enabled: true, Tools: []string{"search", "scrape"}},
			{ID: "github", Kind: "mcp", State: "registered", Enabled: false, MissingEnv: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}},
		},
	}
	r, _ := newPluginToolsRegistry(t, fake)
	res, _ := r.Dispatch(context.Background(), "plugin_list", map[string]any{})
	if !res.Success {
		t.Fatalf("list 应成功，got %+v", res)
	}
	for _, want := range []string{"web_search", "running", "工具=search,scrape", "github", "缺env=GITHUB_PERSONAL_ACCESS_TOKEN"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("输出缺 %q: %s", want, res.Output)
		}
	}
}

// TestPluginTools_VisibleInSchema 五工具经 SchemaSource 兜底路径暴露给 LLM。
func TestPluginTools_VisibleInSchema(t *testing.T) {
	r := NewBuiltinRegistry(t.TempDir(), nil, nil)
	names := map[string]bool{}
	for _, td := range r.Schema() {
		names[td.Name()] = true
	}
	for _, want := range []string{"plugin_search", "plugin_install", "plugin_enable", "plugin_disable", "plugin_list"} {
		if !names[want] {
			t.Fatalf("Schema 缺 %s", want)
		}
	}
}
