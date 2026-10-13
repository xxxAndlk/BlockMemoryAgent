package plugins

// process 插件（kind=process）单元测试：
//   - settings 解析（缺省值 / 全字段 / args/env 双形态兼容）；
//   - Init 校验（command 必填）；
//   - 假长驻进程用 Go 测试 helper 进程（重 exec 测试二进制，见 TestHelperProcess），
//     不依赖 node/docker 等外部环境；
//   - Start→运行→Stop 幂等；崩溃自动重启（auto_restart 开/关）；
//   - health_url 探针（httptest.Server）就绪与超时；
//   - Manager 集成：kind=process 实例创建、无工具注册。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestHelperProcess 不是真正的测试：作为子进程被重 exec（os.Executable()），
// 按 HELPER_MODE 扮演假长驻进程；普通测试运行时直接返回（开销可忽略）。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	switch os.Getenv("HELPER_MODE") {
	case "sleep":
		select {} // 永久阻塞，等 Stop 杀
	case "exit":
		os.Exit(1) // 立即崩溃，观察自动重启
	}
	os.Exit(0)
}

// helperSettings 构造重 exec 测试二进制的 process 插件 settings。
func helperSettings(t *testing.T, mode string) map[string]any {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"command": exe,
		"args":    []any{"-test.run=TestHelperProcess"},
		"env": map[string]any{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            mode,
		},
	}
}

// newProcess 构造可直接访问内部字段的 process 插件实例。
func newProcess(t *testing.T, id string, settings map[string]any) *processPlugin {
	t.Helper()
	p, ok := newProcessPlugin(id, settings, nil).(*processPlugin)
	if !ok {
		t.Fatal("newProcessPlugin 应返回 *processPlugin")
	}
	return p
}

// startCount 读取累计 spawn 次数（重启观察用）。
func (p *processPlugin) startCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.starts
}

// waitForCond 轮询条件直至满足或超时。
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

func TestProcessSettingsFromMapDefaults(t *testing.T) {
	s := processSettingsFromMap(map[string]any{})
	if s.Command != "" || s.HealthURL != "" || s.Cwd != "" {
		t.Fatalf("缺省字段应为空: %+v", s)
	}
	if !s.AutoRestart {
		t.Fatal("auto_restart 缺省应为 true")
	}
	if s.ReadyTimeout != processReadyTimeoutDefault {
		t.Fatalf("ReadyTimeout 缺省应为 %v, got %v", processReadyTimeoutDefault, s.ReadyTimeout)
	}
}

func TestProcessSettingsFromMapFull(t *testing.T) {
	s := processSettingsFromMap(map[string]any{
		"command":      "node",
		"args":         []any{"bridge.mjs", "--port", "4000"},
		"env":          map[string]any{"ILINK_TOKEN": "tok"},
		"cwd":          "/plugins/wechat-ilink",
		"health_url":   "http://127.0.0.1:4000/health",
		"auto_restart": false,
		"ready_timeout": float64(10),
		"name":         "微信 iLink 直连桥",
		"version":      "1.0.0",
		"description":  "微信桥接",
	})
	if s.Command != "node" {
		t.Fatalf("command 解析错误: %q", s.Command)
	}
	if len(s.Args) != 3 || s.Args[0] != "bridge.mjs" {
		t.Fatalf("args 解析错误: %v", s.Args)
	}
	if s.Env["ILINK_TOKEN"] != "tok" {
		t.Fatalf("env 解析错误: %v", s.Env)
	}
	if s.Cwd != "/plugins/wechat-ilink" || s.HealthURL != "http://127.0.0.1:4000/health" {
		t.Fatalf("cwd/health_url 解析错误: %+v", s)
	}
	if s.AutoRestart {
		t.Fatal("auto_restart 显式 false 应生效")
	}
	if s.ReadyTimeout != 10*time.Second {
		t.Fatalf("ready_timeout 解析错误: %v", s.ReadyTimeout)
	}
	if s.Name != "微信 iLink 直连桥" || s.Version != "1.0.0" || s.Description != "微信桥接" {
		t.Fatalf("展示元数据解析错误: %+v", s)
	}
}

func TestProcessSettingsArgsEnvCompat(t *testing.T) {
	// 程序化构造的 []string / map[string]string 形态也应兼容（bundle 展开管线）。
	s := processSettingsFromMap(map[string]any{
		"command": "node",
		"args":    []string{"a.mjs"},
		"env":     map[string]string{"K": "V"},
	})
	if len(s.Args) != 1 || s.Args[0] != "a.mjs" {
		t.Fatalf("[]string args 兼容失败: %v", s.Args)
	}
	if s.Env["K"] != "V" {
		t.Fatalf("map[string]string env 兼容失败: %v", s.Env)
	}
}

func TestProcessPluginInitRequiresCommand(t *testing.T) {
	p := newProcess(t, "x", map[string]any{})
	if err := p.Init(context.Background(), Deps{}); err == nil {
		t.Fatal("缺 command 应报错")
	}
	p = newProcess(t, "x", map[string]any{"command": "node"})
	if err := p.Init(context.Background(), Deps{}); err != nil {
		t.Fatalf("有 command 不应报错: %v", err)
	}
	// process 插件不注入工具。
	if tools := p.Tools(); len(tools) != 0 {
		t.Fatalf("process 插件 Tools 应为空, got %d", len(tools))
	}
	m := p.Manifest()
	if m.Kind != KindProcess || m.Name != "x" {
		t.Fatalf("Manifest 错误: %+v", m)
	}
}

func TestProcessPluginStartStopIdempotent(t *testing.T) {
	p := newProcess(t, "sleeper", helperSettings(t, "sleep"))
	ctx := context.Background()
	if err := p.Init(ctx, Deps{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	if p.startCount() != 1 {
		t.Fatalf("应 spawn 1 次, got %d", p.startCount())
	}
	// Stop 幂等：连续两次均成功；停止后不再重启。
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("第一次 Stop 失败: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("第二次 Stop 应幂等成功: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if p.startCount() != 1 {
		t.Fatalf("Stop 后不应重启, got %d 次 spawn", p.startCount())
	}
}

func TestProcessPluginAutoRestart(t *testing.T) {
	// 进程立即退出，auto_restart 默认开：应观察到重启尝试（starts ≥ 2）。
	p := newProcess(t, "crasher", helperSettings(t, "exit"))
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	defer func() { _ = p.Stop(ctx) }()
	waitForCond(t, 10*time.Second, func() bool { return p.startCount() >= 2 },
		"auto_restart 应触发重启（starts ≥ 2）")
}

func TestProcessPluginNoAutoRestart(t *testing.T) {
	settings := helperSettings(t, "exit")
	settings["auto_restart"] = false
	p := newProcess(t, "oneshot", settings)
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 进程退出后监视 goroutine 应收尾（不重启）。
	p.mu.Lock()
	done := p.doneCh
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("auto_restart 关闭时进程退出后监视 goroutine 应退出")
	}
	if p.startCount() != 1 {
		t.Fatalf("不应重启, got %d 次 spawn", p.startCount())
	}
	// 进程已自然退出，Stop 仍应幂等成功。
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
}

func TestProcessPluginHealthURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	settings := helperSettings(t, "sleep")
	settings["health_url"] = srv.URL
	settings["ready_timeout"] = float64(5)
	p := newProcess(t, "healthy", settings)
	ctx := context.Background()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("探针就绪应 Start 成功: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
}

func TestProcessPluginHealthTimeout(t *testing.T) {
	// 探针恒 500：Start 应在 ready_timeout 后报错并杀进程（不转入重启循环）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	settings := helperSettings(t, "sleep")
	settings["health_url"] = srv.URL
	settings["ready_timeout"] = float64(1)
	p := newProcess(t, "unhealthy", settings)
	ctx := context.Background()
	err := p.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "健康探针超时") {
		t.Fatalf("应报健康探针超时, got %v", err)
	}
	// Start 失败已回收：再 Stop 幂等成功，且无重启。
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Start 失败后 Stop 应幂等: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if p.startCount() != 1 {
		t.Fatalf("探针失败不应重启, got %d 次 spawn", p.startCount())
	}
}

func TestManagerProcessKindLifecycle(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg, WithConfig(&config.PluginsConfig{Plugins: map[string]config.PluginConfig{
		"wechat_bridge": {
			Kind: "process",
			Settings: map[string]any{
				"command": "node",
				"args":    []any{"bridge.mjs"},
			},
		},
	}}))
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	info, ok := mgr.Get("wechat_bridge")
	if !ok {
		t.Fatal("实例未创建")
	}
	if info.Kind != "process" || info.State != StateRegistered {
		t.Fatalf("状态错误: %+v", info)
	}
	// enabled 缺省 false：未 spawn 进程，无工具注册。
	if len(info.Tools) != 0 {
		t.Fatalf("不应注册任何工具, got %v", info.Tools)
	}
}

// TestManagerBundleProcessExpansion bma-plugin.json → bundle 展开 → Manager 端到端：
// 实例 ID bundle/<目录>/<id>、默认启用（spawn helper 假长驻进程）、
// bundle 元数据成为实例展示名、${PLUGIN_DIR} 展开为目录绝对路径。
func TestManagerBundleProcessExpansion(t *testing.T) {
	bundlesDir := t.TempDir()
	plugDir := filepath.Join(bundlesDir, "wx")
	if err := os.MkdirAll(plugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"name":    "微信 iLink 直连桥",
		"version": "1.0.0",
		"plugins": []any{map[string]any{
			"id":   "bridge",
			"kind": "process",
			"settings": map[string]any{
				"command": exe,
				"args":    []any{"-test.run=TestHelperProcess"},
				"env": map[string]any{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "sleep",
				},
				"cwd": "${PLUGIN_DIR}",
			},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugDir, "bma-plugin.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	mgr := NewManager(reg,
		WithConfig(&config.PluginsConfig{}),
		WithBundlesDir(bundlesDir),
	)
	ctx := context.Background()
	if err := mgr.Load(ctx); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	info, ok := mgr.Get("bundle/wx/bridge")
	if !ok {
		t.Fatal("bundle 展开实例未创建")
	}
	if info.Kind != "process" {
		t.Fatalf("Kind 应为 process, got %q", info.Kind)
	}
	if info.Name != "微信 iLink 直连桥" || info.Version != "1.0.0" {
		t.Fatalf("bundle 元数据应成为实例展示名: %+v", info)
	}
	// bundle 插件默认启用：无 health_url，进程活着即 running。
	if info.State != StateRunning {
		t.Fatalf("默认启用应 running: %+v", info)
	}
	// Disable 热停（杀 helper 进程）。
	if err := mgr.Disable(ctx, "bundle/wx/bridge"); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}
	info, _ = mgr.Get("bundle/wx/bridge")
	if info.State != StateStopped {
		t.Fatalf("Disable 后应 stopped: %+v", info)
	}
}
