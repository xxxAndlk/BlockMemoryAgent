package config

// plugins_test.go 覆盖插件配置加载（设计文档 §5）：
//   - plugins.yaml 解析（kind/enabled/settings）；
//   - ${VAR} / ${VAR:default} 环境变量插值（含嵌套 map/slice）；
//   - 文件缺失 → 空配置不报错；
//   - 深合并：config.yaml plugins 段为基底、plugins.yaml 覆盖。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPluginsConfig(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("BMA_TEST_PLUGIN_KEY", "secret-value")
	defer os.Unsetenv("BMA_TEST_PLUGIN_KEY")
	content := `
plugins:
  web_search:
    kind: mcp
    enabled: true
    settings:
      transport: stdio
      command: uvx
      args: ["free-search-mcp"]
      env:
        API_KEY: ${BMA_TEST_PLUGIN_KEY}
  computer_use:
    kind: mcp
    settings:
      destructive: true
`
	path := filepath.Join(dir, "plugins.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadPluginsConfig(path)
	if err != nil {
		t.Fatalf("LoadPluginsConfig: %v", err)
	}
	ws, ok := cfg.Plugins["web_search"]
	if !ok || ws.Kind != "mcp" || !ws.Enabled() {
		t.Fatalf("web_search 解析错误: %+v", ws)
	}
	if ws.Settings["command"] != "uvx" {
		t.Fatalf("command 解析错误: %v", ws.Settings["command"])
	}
	// 嵌套 env 插值。
	env, ok := ws.Settings["env"].(map[string]any)
	if !ok || env["API_KEY"] != "secret-value" {
		t.Fatalf("嵌套 env 插值错误: %v", ws.Settings["env"])
	}
	// args 切片保留。
	args, ok := ws.Settings["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "free-search-mcp" {
		t.Fatalf("args 解析错误: %v", ws.Settings["args"])
	}
	// 缺省 enabled=false。
	cu, ok := cfg.Plugins["computer_use"]
	if !ok || cu.Enabled() {
		t.Fatal("enabled 缺省应为 false")
	}
}

func TestLoadPluginsConfigMissingFile(t *testing.T) {
	cfg, err := LoadPluginsConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("缺失文件应返回空配置而非错误: %v", err)
	}
	if cfg == nil || len(cfg.Plugins) != 0 {
		t.Fatalf("缺失文件应返回空配置: %+v", cfg)
	}
}

func TestLoadPluginsMerge(t *testing.T) {
	dir := t.TempDir()
	// config.yaml 侧 plugins 段（基底）。
	base := &PluginsConfig{Plugins: map[string]PluginConfig{
		"a": {Kind: "mcp", EnabledFlag: boolP(true), Settings: map[string]any{"command": "uvx", "args": []any{"x"}}},
		"b": {Kind: "mcp", Settings: map[string]any{"command": "npx"}},
	}}
	// plugins.yaml 覆盖 a（保留 args，覆盖 command），新增 c。
	if err := os.WriteFile(filepath.Join(dir, "plugins.yaml"),
		[]byte("plugins:\n  a:\n    settings:\n      command: npx2\n  c:\n    kind: mcp\n    enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadPlugins(filepath.Join(dir, "config.yaml"), base)
	if err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	a := merged.Plugins["a"]
	if a.Settings["command"] != "npx2" {
		t.Fatalf("plugins.yaml 应覆盖 command: %v", a.Settings)
	}
	args, ok := a.Settings["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "x" {
		t.Fatalf("未覆盖字段应保留（深合并）: %v", a.Settings)
	}
	if !a.Enabled() {
		t.Fatal("enabled 标志应保留")
	}
	if _, ok := merged.Plugins["c"]; !ok {
		t.Fatal("plugins.yaml 新增插件应并入")
	}
	if merged.Plugins["b"].Settings["command"] != "npx" {
		t.Fatal("基底独有插件应保留")
	}
}

func TestResolveEnvInValue(t *testing.T) {
	os.Setenv("BMA_TEST_DEFAULT", "")
	defer os.Unsetenv("BMA_TEST_DEFAULT")
	os.Setenv("BMA_TEST_SET", "v1")
	defer os.Unsetenv("BMA_TEST_SET")
	in := map[string]any{
		"a": "${BMA_TEST_SET}",
		"b": "${BMA_TEST_DEFAULT:fallback}",
		"c": "${BMA_TEST_UNSET:\"quoted\"}",
		"d": "plain",
		"e": []any{"${BMA_TEST_SET}", 42},
	}
	out := resolveEnvInValue(in).(map[string]any)
	if out["a"] != "v1" || out["b"] != "fallback" || out["c"] != "quoted" || out["d"] != "plain" {
		t.Fatalf("插值错误: %v", out)
	}
	if arr := out["e"].([]any); arr[0] != "v1" || arr[1] != 42 {
		t.Fatalf("切片插值错误: %v", arr)
	}
}

func boolP(b bool) *bool { return &b }
