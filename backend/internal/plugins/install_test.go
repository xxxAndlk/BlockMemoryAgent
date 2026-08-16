package plugins

// install_test.go 覆盖动态安装闭环（TODO #51）：
//   - 手工 manifest 安装：持久化 installed 文件 + 实例创建 + 立即启用；
//   - 目录条目解析安装（内置目录 / 检索缓存 / 远程 registry）；
//   - 失败回滚：enable 失败摘实例 + 移除已写条目，不留半安装状态；
//   - 冲突拒绝（plugins.yaml / installed / 运行实例已有同 ID）；
//   - 校验拒绝（非法 ID / 不可装形态 / 缺启动配置）不产生任何写入；
//   - 持久化跨 Reload 生效（新 Manager 从 installed 文件装载并启用）；
//   - 检索聚合（内置 + 已配置实例带状态）+ 远程 registry 契约解析。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// newInstallHarness 构造带 configDir 的 Manager（installed 文件落盘验证用）。
// 工厂记录每个插件 ID 收到的 settings（断言目录条目/手工配置透传）。
func newInstallHarness(t *testing.T) (*Manager, *tool.Registry, map[string]map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	settingsByID := map[string]map[string]any{}
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			settingsByID[id] = settings
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}},
				tools: []tool.Tool{fakeTool(id + "_tool")}}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	return mgr, reg, settingsByID, dir
}

// readInstalled 读取 installed 文件内容（不存在返回空字符串）。
func readInstalled(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, installedConfigName))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取 installed 文件失败: %v", err)
	}
	return string(data)
}

// TestInstall_ManualManifest 手工 manifest 安装：持久化 + 启用 + 工具注册。
func TestInstall_ManualManifest(t *testing.T) {
	mgr, reg, settingsByID, dir := newInstallHarness(t)
	ctx := context.Background()

	info, err := mgr.Install(ctx, Manifest{ID: "mysearch", Name: "MySearch", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "uvx", "args": []any{"my-search-mcp"}}, true)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	if info.State != StateRunning || !info.Enabled {
		t.Fatalf("安装后应 running+enabled，got %+v", info)
	}
	if !reg.Has("mysearch_tool") {
		t.Fatal("插件工具未注册")
	}
	// 持久化：installed 文件含条目。
	content := readInstalled(t, dir)
	if !strings.Contains(content, "mysearch") || !strings.Contains(content, "my-search-mcp") {
		t.Fatalf("installed 文件缺少条目: %s", content)
	}
	if !strings.Contains(content, "enabled: true") {
		t.Fatalf("installed 文件应持久化 enabled: true: %s", content)
	}
	// settings 原样透传给工厂。
	if got := settingsByID["mysearch"]; got["command"] != "uvx" {
		t.Fatalf("settings 未透传: %v", got)
	}
}

// TestInstall_FromCatalog 目录条目安装：只传 ID，按内置目录解析并安装。
func TestInstall_FromCatalog(t *testing.T) {
	mgr, reg, settingsByID, dir := newInstallHarness(t)
	adapter := NewToolManagerAdapter(mgr)
	ctx := context.Background()

	info, err := adapter.Install(ctx, tool.InstallRequest{ID: "sequential_thinking", Enabled: true})
	if err != nil {
		t.Fatalf("catalog install failed: %v", err)
	}
	if info.State != string(StateRunning) {
		t.Fatalf("目录安装后应 running，got %+v", info)
	}
	if !reg.Has("sequential_thinking_tool") {
		t.Fatal("目录条目工具未注册")
	}
	s := settingsByID["sequential_thinking"]
	if s["command"] != "npx" {
		t.Fatalf("目录条目 settings.command 应为 npx，got %v", s["command"])
	}
	args, _ := s["args"].([]any)
	if len(args) != 2 || args[1] != "@modelcontextprotocol/server-sequential-thinking" {
		t.Fatalf("目录条目 args 错误: %v", args)
	}
	if !strings.Contains(readInstalled(t, dir), "sequential_thinking") {
		t.Fatal("目录安装未持久化")
	}
}

// TestInstall_ManualEnvOverride 手工配置 env 覆盖目录条目。
func TestInstall_ManualEnvOverride(t *testing.T) {
	mgr, _, settingsByID, _ := newInstallHarness(t)
	adapter := NewToolManagerAdapter(mgr)
	ctx := context.Background()

	_, err := adapter.Install(ctx, tool.InstallRequest{
		ID:       "github",
		Settings: map[string]any{"env": map[string]any{"GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_test"}},
	})
	if err != nil {
		t.Fatalf("install with env override failed: %v", err)
	}
	env, _ := settingsByID["github"]["env"].(map[string]any)
	if env["GITHUB_PERSONAL_ACCESS_TOKEN"] != "ghp_test" {
		t.Fatalf("env 覆盖未生效: %v", settingsByID["github"])
	}
}

// TestInstall_RollbackOnEnableFailure enable 失败：摘实例 + 移除持久化条目。
func TestInstall_RollbackOnEnableFailure(t *testing.T) {
	dir := t.TempDir()
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}},
				startErr: errors.New("端点连接失败")}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	_, err := mgr.Install(context.Background(), Manifest{ID: "broken", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, true)
	if err == nil || !strings.Contains(err.Error(), "已回滚安装") {
		t.Fatalf("应返回回滚错误，got %v", err)
	}
	if _, ok := mgr.Get("broken"); ok {
		t.Fatal("失败后实例应已摘除")
	}
	if content := readInstalled(t, dir); strings.Contains(content, "broken") {
		t.Fatalf("失败后 installed 文件应无条目: %s", content)
	}
}

// TestInstall_ConflictRejected 同 ID 重复安装拒绝（installed 文件与运行实例双重校验）。
func TestInstall_ConflictRejected(t *testing.T) {
	mgr, _, _, dir := newInstallHarness(t)
	ctx := context.Background()
	manifest := Manifest{ID: "dup", Kind: KindMCP}
	settings := map[string]any{"transport": "stdio", "command": "npx"}
	if _, err := mgr.Install(ctx, manifest, settings, true); err != nil {
		t.Fatalf("首次安装失败: %v", err)
	}
	if _, err := mgr.Install(ctx, manifest, settings, true); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重复安装应拒绝，got %v", err)
	}
	content := readInstalled(t, dir)
	if strings.Count(content, "dup") > 1 {
		t.Fatalf("installed 文件不应有重复条目: %s", content)
	}
}

// TestInstall_ConflictWithPluginsYAML plugins.yaml 已有同 ID 时拒绝。
func TestInstall_ConflictWithPluginsYAML(t *testing.T) {
	dir := t.TempDir()
	yaml := "plugins:\n  web_search:\n    kind: mcp\n    enabled: true\n    settings:\n      transport: stdio\n      command: npx\n"
	if err := os.WriteFile(filepath.Join(dir, "plugins.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}}}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	_, err := mgr.Install(context.Background(), Manifest{ID: "web_search", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, true)
	if err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("plugins.yaml 冲突应拒绝，got %v", err)
	}
	if content := readInstalled(t, dir); content != "" {
		t.Fatalf("拒绝后不应写 installed 文件: %s", content)
	}
}

// TestInstall_Validation 校验拒绝表：不产生任何写入。
func TestInstall_Validation(t *testing.T) {
	cases := []struct {
		name     string
		manifest Manifest
		settings map[string]any
		wantErr  string
	}{
		{"非法ID", Manifest{ID: "a b/c", Kind: KindMCP}, map[string]any{"transport": "stdio", "command": "x"}, "ID"},
		{"未知形态", Manifest{ID: "x", Kind: Kind("plugin")}, map[string]any{}, "不可动态安装"},
		{"stdio缺命令", Manifest{ID: "x", Kind: KindMCP}, map[string]any{"transport": "stdio"}, "command"},
		{"docker缺镜像", Manifest{ID: "x", Kind: KindMCP}, map[string]any{"transport": "docker"}, "image"},
		{"http缺url", Manifest{ID: "x", Kind: KindMCP}, map[string]any{"transport": "http"}, "url"},
		{"未知transport", Manifest{ID: "x", Kind: KindMCP}, map[string]any{"transport": "websocket"}, "transport"},
		{"service缺镜像", Manifest{ID: "x", Kind: KindService}, map[string]any{}, "image"},
		{"destructive缺roles", Manifest{ID: "x", Kind: KindMCP},
			map[string]any{"transport": "stdio", "command": "x", "destructive": true}, "显式声明可见角色"},
		{"destructive通配roles", Manifest{ID: "x", Kind: KindMCP, Roles: []string{"*"}},
			map[string]any{"transport": "stdio", "command": "x", "destructive": true}, "显式声明可见角色"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr, _, _, dir := newInstallHarness(t)
			_, err := mgr.Install(context.Background(), c.manifest, c.settings, true)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("期望错误含 %q，got %v", c.wantErr, err)
			}
			if content := readInstalled(t, dir); content != "" {
				t.Fatalf("校验拒绝不应写 installed 文件: %s", content)
			}
		})
	}
}

// TestInstall_DestructiveRolesCeiling 天花板红线（TODO #52）：destructive 插件必须
// 显式授权角色；显式 roles 放行且安装条目落 roles 声明（重启后可见性一致）。
func TestInstall_DestructiveRolesCeiling(t *testing.T) {
	mgr, reg, settingsByID, dir := newInstallHarness(t)
	ctx := context.Background()
	// 显式 roles 放行：destructive + roles=["meta"]。
	manifest := Manifest{ID: "danger", Name: "Danger", Kind: KindMCP, Roles: []string{"meta"}}
	settings := map[string]any{"transport": "stdio", "command": "npx", "destructive": true}
	if _, err := mgr.Install(ctx, manifest, settings, true); err != nil {
		t.Fatalf("显式 roles 的 destructive 插件应可安装: %v", err)
	}
	// 持久化条目含 roles 声明（settings 归一化）。
	cfg, err := config.LoadPluginsConfig(filepath.Join(dir, installedConfigName))
	if err != nil {
		t.Fatalf("读 installed 配置失败: %v", err)
	}
	roles, _ := cfg.Plugins["danger"].Settings["roles"].([]any)
	if len(roles) != 1 || roles[0] != "meta" {
		t.Fatalf("installed 条目应落 roles=[meta] 声明，got %v", roles)
	}
	// 插件工厂收到同一 settings（含 roles）。
	if got := settingsByID["danger"]; got == nil || len(got["roles"].([]any)) != 1 {
		t.Fatalf("createInstance 应收到归一化 roles，got %v", got)
	}
	// 工具已注册（enable 成功）。
	if !reg.Has("danger_tool") {
		t.Fatal("destructive 插件工具应注册")
	}
	// 重启（新 Manager 装载 installed 文件）：roles 声明保留（工厂仿 mcpbridge 从 settings 解析）。
	re := NewManager(tool.NewBuiltinRegistry(t.TempDir(), nil, nil),
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			roles := []string{"*"}
			if rv, ok := settings["roles"].([]any); ok && len(rv) > 0 {
				roles = nil
				for _, r := range rv {
					if s, ok := r.(string); ok && s != "" {
						roles = append(roles, s)
					}
				}
			}
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: roles},
				tools: []tool.Tool{fakeTool(id + "_tool")}}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	if err := re.Load(ctx); err != nil {
		t.Fatalf("重载失败: %v", err)
	}
	info, ok := re.Get("danger")
	if !ok || len(info.Roles) != 1 || info.Roles[0] != "meta" {
		t.Fatalf("重启后 roles 声明应保留，got %+v", info)
	}
}

// TestInstall_DefaultRolesDeclared 无 roles 安装：落显式 ["*"] 声明（可审计）。
func TestInstall_DefaultRolesDeclared(t *testing.T) {
	mgr, _, _, dir := newInstallHarness(t)
	ctx := context.Background()
	if _, err := mgr.Install(ctx, Manifest{ID: "open", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "x"}, true); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	cfg, err := config.LoadPluginsConfig(filepath.Join(dir, installedConfigName))
	if err != nil {
		t.Fatalf("读 installed 配置失败: %v", err)
	}
	roles, _ := cfg.Plugins["open"].Settings["roles"].([]any)
	if len(roles) != 1 || roles[0] != "*" {
		t.Fatalf("缺省 roles 应显式落 [*] 声明，got %v", roles)
	}
}

// TestInstall_NoConfigDir 未注入 configDir 时拒绝安装（无法持久化）。
func TestInstall_NoConfigDir(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}}}, nil
		}),
		WithConfig(&config.PluginsConfig{}),
	)
	_, err := mgr.Install(context.Background(), Manifest{ID: "x", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, true)
	if err == nil || !strings.Contains(err.Error(), "configDir") {
		t.Fatalf("未配 configDir 应拒绝，got %v", err)
	}
}

// TestInstall_PersistsAcrossReload installed 条目跨 Reload 生效（新 Manager 装载即启用）。
func TestInstall_PersistsAcrossReload(t *testing.T) {
	mgr, _, _, dir := newInstallHarness(t)
	ctx := context.Background()
	if _, err := mgr.Install(ctx, Manifest{ID: "persist_me", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, true); err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	// 新 Manager（模拟重启）：同一 configDir，instances 表为空。
	reg2 := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr2 := NewManager(reg2,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}},
				tools: []tool.Tool{fakeTool(id + "_tool")}}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	if err := mgr2.Load(ctx); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	info, ok := mgr2.Get("persist_me")
	if !ok {
		t.Fatal("重启后应从 installed 文件恢复实例")
	}
	if info.State != StateRunning || !info.Enabled {
		t.Fatalf("重启后应自动启用，got %+v", info)
	}
	if !reg2.Has("persist_me_tool") {
		t.Fatal("重启后插件工具应注册")
	}
}

// TestInstall_DisabledPersists enabled=false 安装：持久化但不启用，重启后仍不启用。
func TestInstall_DisabledPersists(t *testing.T) {
	mgr, _, _, dir := newInstallHarness(t)
	ctx := context.Background()
	if _, err := mgr.Install(ctx, Manifest{ID: "lazy", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, false); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	info, ok := mgr.Get("lazy")
	if !ok || info.State != StateRegistered {
		t.Fatalf("enabled=false 安装后应 registered，got %+v", info)
	}

	reg2 := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr2 := NewManager(reg2,
		WithMCPFactory(func(id string, settings map[string]any, deps Deps) (Plugin, error) {
			return &fakePlugin{manifest: Manifest{ID: id, Name: id, Kind: KindMCP, Roles: []string{"*"}}}, nil
		}),
		WithConfigDir(dir),
		WithConfig(&config.PluginsConfig{}),
	)
	if err := mgr2.Load(ctx); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	info2, ok := mgr2.Get("lazy")
	if !ok || info2.Enabled {
		t.Fatalf("重启后应保持未启用，got %+v", info2)
	}
}

// TestSearch_Aggregation 检索聚合：内置目录命中 + 已配置实例带状态。
func TestSearch_Aggregation(t *testing.T) {
	mgr, _, _, _ := newInstallHarness(t)
	ctx := context.Background()
	if _, err := mgr.Install(ctx, Manifest{ID: "seq", Name: "Seq Think", Kind: KindMCP},
		map[string]any{"transport": "stdio", "command": "npx"}, true); err != nil {
		t.Fatalf("Install failed: %v", err)
	}
	// 内置目录：按描述子串命中。
	// 空 query 全量检索：内置目录 + 已配置实例都应在列。
	entries, _ := mgr.Search(ctx, "")
	var catalogHit bool
	for _, e := range entries {
		if e.ID == "sequential_thinking" && e.Source == sourceBuiltin {
			catalogHit = true
		}
	}
	if !catalogHit {
		t.Fatalf("内置目录未命中: %+v", entries)
	}
	// 已配置实例：带状态与工具。
	var configHit bool
	for _, e := range entries {
		if e.ID == "seq" && e.Source == sourceConfig && e.State == string(StateRunning) && len(e.Tools) == 1 {
			configHit = true
		}
	}
	if !configHit {
		t.Fatalf("已配置实例未带状态入检索: %+v", entries)
	}
	// 检索缓存：安装按 ID 可直接解析。
	entry, ok := mgr.resolveCatalogEntry(ctx, "seq")
	if !ok || entry.Source != sourceConfig {
		t.Fatalf("检索缓存解析失败: %+v", entry)
	}
}

// TestFetchRemoteRegistry_Contract 远程 registry 契约解析（httptest 离线验证）：
// isLatest 去重、http remotes 优先、npm stdio 兜底、不可安装跳过、错误 fail-soft。
func TestFetchRemoteRegistry_Contract(t *testing.T) {
	payload := `{"servers":[
		{"server":{"name":"org.alpha/alpha","title":"Alpha","description":"alpha search tool","version":"1.0.0",
			"remotes":[{"type":"streamable-http","url":"https://alpha.example/mcp"}]},
		 "_meta":{"io.modelcontextprotocol.registry/official":{"isLatest":false}}},
		{"server":{"name":"org.alpha/alpha","title":"Alpha","description":"alpha search tool","version":"1.0.1",
			"remotes":[{"type":"streamable-http","url":"https://alpha.example/mcp"}]},
		 "_meta":{"io.modelcontextprotocol.registry/official":{"isLatest":true}}},
		{"server":{"name":"org.beta/beta","title":"Beta","description":"beta npm server","version":"2.0.0",
			"packages":[{"registryType":"npm","identifier":"beta-mcp","version":"2.0.0","transport":{"type":"stdio"}}]}},
		{"server":{"name":"org.gamma/gamma","title":"Gamma","description":"gamma http only","version":"3.0.0",
			"remotes":[{"type":"sse","url":"https://gamma.example/sse"}]}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, payload)
	}))
	defer srv.Close()

	entries, notes := fetchRemoteRegistry(context.Background(), []string{srv.URL}, "beta")
	if len(notes) != 0 {
		t.Fatalf("不应有 note: %v", notes)
	}
	// query=beta 只命中 beta；alpha（两版本去重留 1.0.1）与 gamma 被过滤。
	if len(entries) != 1 || entries[0].ID != "org.beta-beta" {
		t.Fatalf("过滤/去重错误: %+v", entries)
	}
	if entries[0].Settings["transport"] != "stdio" || entries[0].Settings["command"] != "npx" {
		t.Fatalf("npm 转换错误: %+v", entries[0].Settings)
	}

	// 全量：alpha 去重留 isLatest、http 转换、gamma 不可安装跳过。
	entries, _ = fetchRemoteRegistry(context.Background(), []string{srv.URL}, "")
	if len(entries) != 2 {
		t.Fatalf("应剩 alpha + beta 两条，got %+v", entries)
	}
	var alpha *CatalogEntry
	for i := range entries {
		if entries[i].ID == "org.alpha-alpha" {
			alpha = &entries[i]
		}
	}
	if alpha == nil || alpha.Version != "1.0.1" || alpha.Settings["transport"] != "http" {
		t.Fatalf("alpha 解析错误: %+v", alpha)
	}

	// 来源不可达：note 记录且不 panic。
	_, notes = fetchRemoteRegistry(context.Background(), []string{"http://127.0.0.1:1/nope"}, "")
	if len(notes) == 0 {
		t.Fatal("不可达来源应有 note")
	}
}

// TestSanitizeID ID 净化：非法字符替换为连字符。
func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"ac.inference.sh/mcp": "ac.inference.sh-mcp",
		"simple":              "simple",
		"a b/c":               "a-b-c",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Fatalf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}
