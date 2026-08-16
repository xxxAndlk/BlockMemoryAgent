package plugins

// manager_test.go 覆盖插件管理器状态机（设计文档 §4.1）：
//   - Enable 成功：Init → Start → 工具注册进 Registry；
//   - Enable 失败（Start 失败 / 工具名冲突）：回滚（已注册工具摘除、插件 Stop）；
//   - Disable：工具从 Registry 摘除、Schema 不再暴露；
//   - 重复 Enable / Disable 幂等；RequiresEnv 缺失拒绝 enable；
//   - 断线降级：onBridgeDown 摘工具 + degraded，onBridgeUp 恢复 running；
//   - 角色可见性：ToolVisibleForRole 按 Manifest.Roles 判定；
//   - Reload：新增/移除/修改插件差异应用。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/skill"
	"github.com/google/jsonschema-go/jsonschema"
)

// ---- fake 插件 ----

type fakePlugin struct {
	manifest   Manifest
	initErr    error
	startErr   error
	stopCalls  atomic.Int32
	startCalls atomic.Int32
	tools      []tool.Tool
}

func (f *fakePlugin) Manifest() Manifest { return f.manifest }
func (f *fakePlugin) Init(ctx context.Context, deps Deps) error {
	return f.initErr
}
func (f *fakePlugin) Start(ctx context.Context) error {
	f.startCalls.Add(1)
	return f.startErr
}
func (f *fakePlugin) Stop(ctx context.Context) error {
	f.stopCalls.Add(1)
	return nil
}
func (f *fakePlugin) Tools() []tool.Tool { return f.tools }

type fakePluginTool struct{ name string }

func (f *fakePluginTool) Name() string      { return f.name }
func (f *fakePluginTool) Aliases() []string { return nil }
func (f *fakePluginTool) Description() string { return "fake 工具 " + f.name }
func (f *fakePluginTool) InputSchema() *jsonschema.Schema { return nil }
func (f *fakePluginTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	return &tool.Result{Tool: f.name, Success: true, Output: "ok"}
}

func fakeTool(name string) tool.Tool { return &fakePluginTool{name: name} }

// mgrHarness 是测试脚手架：每 ID 独立 fake 插件实例 + 注入配置。
type mgrHarness struct {
	mgr         *Manager
	reg         *tool.Registry
	created     map[string]*fakePlugin
	createdList []*fakePlugin // 按创建顺序记录全部实例（Reload 重建可验证）
}

// newHarness 构造 Manager；mk 为每插件 ID 的 fake 构造器（缺省：工具名 = id+"_tool"）。
func newHarness(t *testing.T, mk func(id string) *fakePlugin) *mgrHarness {
	t.Helper()
	if mk == nil {
		mk = func(id string) *fakePlugin {
			return &fakePlugin{tools: []tool.Tool{fakeTool(id + "_tool")}}
		}
	}
	h := &mgrHarness{
		reg:     tool.NewBuiltinRegistry(t.TempDir(), nil, nil),
		created: map[string]*fakePlugin{},
	}
	h.mgr = NewManager(h.reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			fp := mk(id)
			// 合并而非覆盖 manifest：测试自定义的 Roles/RequiresEnv 必须保留。
			if fp.manifest.ID == "" {
				fp.manifest.ID = id
			}
			if fp.manifest.Name == "" {
				fp.manifest.Name = id
			}
			if fp.manifest.Kind == "" {
				fp.manifest.Kind = KindMCP
			}
			if len(fp.manifest.Roles) == 0 {
				fp.manifest.Roles = []string{"*"}
			}
			h.created[id] = fp
			h.createdList = append(h.createdList, fp)
			return fp, nil
		}),
		WithConfig(&config.PluginsConfig{}),
	)
	return h
}

// setCfg 覆盖注入配置（测试用；生产路径经 Load 从文件装载）。
func (h *mgrHarness) setCfg(cfg *config.PluginsConfig) {
	h.mgr.mu.Lock()
	h.mgr.cfg = cfg
	h.mgr.mu.Unlock()
}

func cfgWith(id, kind string, enabled bool, settings map[string]any) *config.PluginsConfig {
	return &config.PluginsConfig{Plugins: map[string]config.PluginConfig{
		id: {Kind: kind, EnabledFlag: boolPtr(enabled), Settings: settings},
	}}
}

// TestEnableRegistersTools 验证 Enable 成功路径：工具注册、Schema 可见、状态 running。
func TestEnableRegistersTools(t *testing.T) {
	h := newHarness(t, func(id string) *fakePlugin {
		return &fakePlugin{tools: []tool.Tool{fakeTool("web_search"), fakeTool("research")}}
	})
	ctx := context.Background()
	h.setCfg(cfgWith("web", "mcp", false, nil))
	if err := h.mgr.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := h.mgr.Enable(ctx, "web"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	info, ok := h.mgr.Get("web")
	if !ok || info.State != StateRunning {
		t.Fatalf("启用后应为 running，got %+v", info)
	}
	if len(info.Tools) != 2 {
		t.Fatalf("工具清单应为 2，got %v", info.Tools)
	}
	// Registry 可见 + Schema 可见（动态工具兜底路径）。
	if !h.reg.Has("web_search") {
		t.Fatal("web_search 应已注册")
	}
	found := false
	for _, s := range h.reg.Schema() {
		if s.Name() == "web_search" {
			found = true
		}
	}
	if !found {
		t.Fatal("Schema 应暴露 web_search")
	}
}

// TestEnableIdempotent 验证重复 Enable 幂等（不重复 Start/注册）。
func TestEnableIdempotent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	if err := h.mgr.Enable(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Enable(ctx, "p"); err != nil {
		t.Fatalf("重复 Enable 应幂等: %v", err)
	}
	if h.created["p"].startCalls.Load() != 1 {
		t.Fatalf("Start 应只调用一次，got %d", h.created["p"].startCalls.Load())
	}
}

// TestEnableRollbackOnStartFailure 验证 Start 失败回滚：无工具残留、状态 registered。
func TestEnableRollbackOnStartFailure(t *testing.T) {
	h := newHarness(t, func(id string) *fakePlugin {
		return &fakePlugin{tools: []tool.Tool{fakeTool("t1")}, startErr: fmt.Errorf("connect refused")}
	})
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	if err := h.mgr.Enable(ctx, "p"); err == nil {
		t.Fatal("Start 失败应返回错误")
	}
	if h.reg.Has("t1") {
		t.Fatal("Start 失败不应注册工具")
	}
	info, _ := h.mgr.Get("p")
	if info.State != StateRegistered {
		t.Fatalf("失败后应回滚到 registered，got %s", info.State)
	}
	if info.LastError == "" {
		t.Fatal("应记录 last_error")
	}
}

// TestEnableRollbackOnToolCollision 验证工具名冲突回滚：先注册的工具被摘除。
func TestEnableRollbackOnToolCollision(t *testing.T) {
	h := newHarness(t, nil)
	h.reg.Register(fakeTool("p_tool")) // 预占 p 的工具名
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	if err := h.mgr.Enable(ctx, "p"); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("工具冲突应报错，got %v", err)
	}
	if h.created["p"].stopCalls.Load() == 0 {
		t.Fatal("回滚应调用插件 Stop")
	}
}

// TestDisableUnregistersTools 验证 Disable：工具摘除、状态 stopped、重复 Disable 幂等。
func TestDisableUnregistersTools(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	_ = h.mgr.Enable(ctx, "p")
	if !h.reg.Has("p_tool") {
		t.Fatal("Enable 后工具应注册")
	}
	if err := h.mgr.Disable(ctx, "p"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if h.reg.Has("p_tool") {
		t.Fatal("Disable 后工具应摘除")
	}
	info, _ := h.mgr.Get("p")
	if info.State != StateStopped {
		t.Fatalf("状态应为 stopped，got %s", info.State)
	}
	if err := h.mgr.Disable(ctx, "p"); err != nil {
		t.Fatalf("重复 Disable 应幂等: %v", err)
	}
	// 重新 Enable 可恢复（Start 重新执行）。
	if err := h.mgr.Enable(ctx, "p"); err != nil {
		t.Fatalf("Disable 后重新 Enable 应成功: %v", err)
	}
	if !h.reg.Has("p_tool") {
		t.Fatal("重新 Enable 后工具应再次注册")
	}
}

// TestEnableRejectsMissingEnv 验证 RequiresEnv 缺失拒绝 enable 并给出原因。
func TestEnableRejectsMissingEnv(t *testing.T) {
	h := newHarness(t, func(id string) *fakePlugin {
		return &fakePlugin{tools: []tool.Tool{fakeTool("t1")},
			manifest: Manifest{ID: id, Name: id, Kind: KindMCP, RequiresEnv: []string{"BMA_MISSING_KEY_XYZ"}}}
	})
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	err := h.mgr.Enable(ctx, "p")
	if err == nil || !strings.Contains(err.Error(), "BMA_MISSING_KEY_XYZ") {
		t.Fatalf("缺失 env 应拒绝并点名变量，got %v", err)
	}
	info, _ := h.mgr.Get("p")
	if len(info.MissingEnv) != 1 || info.MissingEnv[0] != "BMA_MISSING_KEY_XYZ" {
		t.Fatalf("MissingEnv 应列出缺失变量，got %v", info.MissingEnv)
	}
}

// TestLoadAutoEnables 验证 Load 自动 enable enabled:true 的插件，false 的保持 registered。
func TestLoadAutoEnables(t *testing.T) {
	h := newHarness(t, nil)
	h.setCfg(cfgWith("p", "mcp", true, nil))
	ctx := context.Background()
	if err := h.mgr.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !h.reg.Has("p_tool") {
		t.Fatal("enabled:true 插件应随 Load 自动启用")
	}
	// enabled 缺省 = false：不自动启用。
	h2 := newHarness(t, nil)
	h2.setCfg(cfgWith("q", "mcp", false, nil))
	_ = h2.mgr.Load(ctx)
	if h2.reg.Has("q_tool") {
		t.Fatal("enabled:false 插件不应自动启用")
	}
	info, _ := h2.mgr.Get("q")
	if info.State != StateRegistered {
		t.Fatalf("未启用插件应停在 registered，got %s", info.State)
	}
}

// TestBridgeDegradeRecover 验证断线降级与恢复回调。
func TestBridgeDegradeRecover(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)
	_ = h.mgr.Enable(ctx, "p")

	// 模拟断线：Manager 直接驱动回调（桥的 supervise 循环经 SetLifecycleHooks 接入）。
	h.mgr.onBridgeDown("p", fmt.Errorf("connection lost"))
	if h.reg.Has("p_tool") {
		t.Fatal("断线应自动摘除工具")
	}
	info, _ := h.mgr.Get("p")
	if info.State != StateDegraded {
		t.Fatalf("断线后应为 degraded，got %s", info.State)
	}

	// 模拟重连成功：新工具列表注册。
	newTools := []tool.Tool{fakeTool("p_tool"), fakeTool("t2")}
	h.mgr.onBridgeUp("p", newTools)
	if !h.reg.Has("t2") {
		t.Fatal("重连应重新注册工具")
	}
	info, _ = h.mgr.Get("p")
	if info.State != StateRunning || len(info.Tools) != 2 {
		t.Fatalf("重连后应为 running + 2 工具，got %+v", info)
	}
}

// TestToolVisibleForRole 验证角色可见性：Roles 缺省全可见、限定角色精确匹配。
func TestToolVisibleForRole(t *testing.T) {
	h := newHarness(t, func(id string) *fakePlugin {
		roles := []string{"*"}
		if id == "restricted" {
			roles = []string{"meta"}
		}
		return &fakePlugin{tools: []tool.Tool{fakeTool(id + "_tool")},
			manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: roles}}
	})
	ctx := context.Background()
	h.setCfg(&config.PluginsConfig{Plugins: map[string]config.PluginConfig{
		"open":       {Kind: "mcp", EnabledFlag: boolPtr(false)},
		"restricted": {Kind: "mcp", EnabledFlag: boolPtr(false)},
	}})
	_ = h.mgr.Load(ctx)
	_ = h.mgr.Enable(ctx, "open")
	_ = h.mgr.Enable(ctx, "restricted")

	if owned, visible := h.mgr.ToolVisibility("domain", "open_tool"); !owned || !visible {
		t.Fatal("Roles [*] 应对任何角色可见")
	}
	if owned, visible := h.mgr.ToolVisibility("meta", "restricted_tool"); !owned || !visible {
		t.Fatal("Roles [meta] 应对 meta 可见")
	}
	if owned, visible := h.mgr.ToolVisibility("domain", "restricted_tool"); !owned || visible {
		t.Fatal("Roles [meta] 不应向 domain 暴露")
	}
	if owned, _ := h.mgr.ToolVisibility("meta", "ReadFile"); owned {
		t.Fatal("非插件工具应 owned=false（白名单语义不变）")
	}
}

// TestReloadDiff 验证 Reload：移除插件被停用、新增插件被装载。
func TestReloadDiff(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", true, nil))
	if err := h.mgr.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.reg.Has("p_tool") {
		t.Fatal("初始装载应启用 p")
	}

	// Reload：移除 p、新增 q。
	h.setCfg(cfgWith("q", "mcp", true, nil))
	if err := h.mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if h.reg.Has("p_tool") {
		t.Fatal("Reload 移除 p 后其工具应摘除")
	}
	if !h.reg.Has("q_tool") {
		t.Fatal("Reload 应装载并启用 q")
	}
	if _, ok := h.mgr.Get("p"); ok {
		t.Fatal("Reload 后 p 应被移除")
	}
}

// TestReloadConfigChange 验证配置变更触发重建（settings 不同 → 新实例）。
func TestReloadConfigChange(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", true, map[string]any{"command": "uvx"}))
	if err := h.mgr.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.reg.Has("p_tool") {
		t.Fatal("初始装载应启用 p")
	}
	// 同一 ID 改 settings：重建实例（工厂再次调用、Start 在新实例上执行）。
	h.setCfg(cfgWith("p", "mcp", true, map[string]any{"command": "npx"}))
	if err := h.mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if !h.reg.Has("p_tool") {
		t.Fatal("配置变更重建后工具应保留")
	}
	if len(h.createdList) != 2 {
		t.Fatalf("配置变更应重建实例（工厂调用两次），got %d 次", len(h.createdList))
	}
	if h.createdList[1].startCalls.Load() != 1 {
		t.Fatalf("新实例应被启用（Start 一次），got %d", h.createdList[1].startCalls.Load())
	}
	// 旧实例已 Stop。
	if h.createdList[0].stopCalls.Load() == 0 {
		t.Fatal("旧实例应被 Stop")
	}
}

// TestStopAll 验证 StopAll 停用全部插件。
func TestStopAll(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(&config.PluginsConfig{Plugins: map[string]config.PluginConfig{
		"a": {Kind: "mcp", EnabledFlag: boolPtr(false)},
		"b": {Kind: "mcp", EnabledFlag: boolPtr(false)},
	}})
	_ = h.mgr.Load(ctx)
	_ = h.mgr.Enable(ctx, "a")
	_ = h.mgr.Enable(ctx, "b")
	if err := h.mgr.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	if h.reg.Has("a_tool") || h.reg.Has("b_tool") {
		t.Fatal("StopAll 应摘除全部插件工具")
	}
}

// TestLoadFromConfigDir 验证从真实 plugins.yaml 文件装载（含 env 插值）。
func TestLoadFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("BMA_TEST_PLUGIN_CMD", "uvx")
	defer os.Unsetenv("BMA_TEST_PLUGIN_CMD")
	if err := os.WriteFile(filepath.Join(dir, "plugins.yaml"),
		[]byte("plugins:\n  p:\n    kind: mcp\n    enabled: true\n    settings:\n      command: ${BMA_TEST_PLUGIN_CMD}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	created := map[string]*fakePlugin{}
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			fp := &fakePlugin{tools: []tool.Tool{fakeTool("t1")}}
			created[id] = fp
			return fp, nil
		}),
		WithConfigDir(dir),
	)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reg.Has("t1") {
		t.Fatal("应从 plugins.yaml 装载并自动启用 p")
	}
}

// TestRegisterBuiltin 验证 builtin 插件程序化注册 + enable。
func TestRegisterBuiltin(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	fp := &fakePlugin{tools: []tool.Tool{fakeTool("core_tool")}}
	fp.manifest = Manifest{ID: "core", Name: "core", Kind: KindBuiltin}
	mgr := NewManager(reg, WithConfig(&config.PluginsConfig{}))
	mgr.RegisterBuiltin(fp)
	_ = mgr.Load(context.Background())
	if err := mgr.Enable(context.Background(), "core"); err != nil {
		t.Fatalf("builtin enable: %v", err)
	}
	if !reg.Has("core_tool") {
		t.Fatal("builtin 插件工具应注册")
	}
}

// TestBundleIntegration 验证 plugins.d/ 目录包展开（设计文档 §7.2）：
// Scan → bundle/<dir>/<server> 插件自动装载启用；目录整包禁用生效；reload 摘除。
func TestBundleIntegration(t *testing.T) {
	root := t.TempDir()
	writeFile := func(p, content string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("plugins.d/my-plug/.mcp.json", `{"mcpServers": {"search": {"command": "uvx", "args": ["free-search-mcp"]}}}`)

	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	h := &mgrHarness{reg: reg, created: map[string]*fakePlugin{}}
	h.mgr = NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			fp := &fakePlugin{tools: []tool.Tool{fakeTool("bundle_tool")}}
			h.created[id] = fp
			return fp, nil
		}),
		WithBundlesDir(filepath.Join(root, "plugins.d")),
		WithSkillPool(nil),
	)
	ctx := context.Background()
	if err := h.mgr.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// bundle 插件默认启用：工具已注册。
	pluginID := "bundle/my-plug/search"
	if _, ok := h.mgr.Get(pluginID); !ok {
		t.Fatalf("bundle 展开插件应存在: %s", pluginID)
	}
	if !reg.Has("bundle_tool") {
		t.Fatal("bundle 插件工具应默认注册")
	}
	// settings 应携带 command/args（透传 .mcp.json）。
	inst := h.mgr.getInstance(pluginID)
	if inst.config.Settings["command"] != "uvx" {
		t.Fatalf("bundle settings 透传错误: %v", inst.config.Settings)
	}

	// reload 摘除整个插件包目录 → 插件消失、工具摘除。
	if err := os.RemoveAll(filepath.Join(root, "plugins.d", "my-plug")); err != nil {
		t.Fatal(err)
	}
	if err := h.mgr.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := h.mgr.Get(pluginID); ok {
		t.Fatal("reload 后 bundle 插件应移除")
	}
	if reg.Has("bundle_tool") {
		t.Fatal("reload 后 bundle 工具应摘除")
	}
}

// TestBundleDirDisable 验证 plugins.yaml 目录级禁用（kind=bundle, enabled=false）。
func TestBundleDirDisable(t *testing.T) {
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins.d")
	if err := os.MkdirAll(filepath.Join(pluginsDir, "my-plug"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginsDir, "my-plug", ".mcp.json"),
		[]byte(`{"mcpServers": {"search": {"command": "uvx"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// configDir 里写 plugins.yaml：my-plug 整包禁用。
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "plugins.yaml"),
		[]byte("plugins:\n  my-plug:\n    kind: bundle\n    enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{tools: []tool.Tool{fakeTool("x")}}, nil
		}),
		WithConfigDir(cfgDir),
		WithBundlesDir(pluginsDir),
	)
	ctx := context.Background()
	if err := mgr.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := mgr.Get("bundle/my-plug/search"); ok {
		t.Fatal("目录级禁用后 bundle 插件不应装载")
	}
	if reg.Has("x") {
		t.Fatal("目录级禁用后工具不应注册")
	}
}

// TestBundleSkillsInjectedAndRemoved 验证 bundle skill 注入 pool 与 reload 摘除。
func TestBundleSkillsInjectedAndRemoved(t *testing.T) {
	root := t.TempDir()
	writeFile := func(p, content string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("plugins.d/plug/.mcp.json", `{"mcpServers": {"s": {"command": "x"}}}`)
	writeFile("plugins.d/plug/skills/web/SKILL.md", "---\nname: web\n---\n正文\n")

	pool := skill.NewPool()
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{tools: []tool.Tool{fakeTool("t")}}, nil
		}),
		WithBundlesDir(filepath.Join(root, "plugins.d")),
		WithSkillPool(pool),
	)
	ctx := context.Background()
	if err := mgr.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if pool.Get("plug_web") == nil {
		t.Fatal("bundle skill 应注入 skill pool")
	}
	// 移除目录 → reload 摘除 skill。
	if err := os.RemoveAll(filepath.Join(root, "plugins.d", "plug")); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if pool.Get("plug_web") != nil {
		t.Fatal("reload 后消失目录的 skill 应摘除")
	}
}

// TestConcurrentEnableDisable 热插拔压测（设计文档 §8.2 简化版）：
// 并发 enable/disable 同一插件与 Schema 读取，无死锁、无 panic、终态一致。
func TestConcurrentEnableDisable(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.setCfg(cfgWith("p", "mcp", false, nil))
	_ = h.mgr.Load(ctx)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = h.mgr.Enable(ctx, "p")
				_ = h.mgr.Disable(ctx, "p")
				_, _ = h.mgr.Get("p")
				_ = h.mgr.List()
				_ = h.reg.Schema()
			}
		}()
	}
	wg.Wait()
	// 终态：已停用（最后操作是 Disable 或幂等成功）。
	info, ok := h.mgr.Get("p")
	if !ok {
		t.Fatal("插件实例应存在")
	}
	if info.State != StateStopped && info.State != StateRunning {
		t.Fatalf("终态异常: %s", info.State)
	}
	if h.reg.Has("p_tool") != (info.State == StateRunning) {
		t.Fatalf("注册表与状态不一致: state=%s has=%v", info.State, h.reg.Has("p_tool"))
	}
}
