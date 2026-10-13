package bundle

// bundle_test.go 覆盖插件目录包加载（设计文档 §3.3、Phase 7）：
//   - .claude-plugin/plugin.json 元数据解析；
//   - .mcp.json server 展开 + ${CLAUDE_PLUGIN_ROOT} 替换 + 环境变量注入；
//   - bma-plugin.json 展开 process 条目 + ${PLUGIN_DIR} 替换 + 三清单共存优先级；
//   - skills/*/SKILL.md frontmatter 解析；
//   - Scan 目录扫描（非插件包跳过、排序稳定）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree 在 dir 下按 path→content 写文件。
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadBundleFull(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".claude-plugin/plugin.json": `{
			"name": "My Search Plugin",
			"version": "1.2.0",
			"description": "搜索插件"
		}`,
		".mcp.json": `{
			"mcpServers": {
				"search": {
					"command": "uvx",
					"args": ["free-search-mcp", "--root", "${CLAUDE_PLUGIN_ROOT}/data"],
					"env": {"SEARCH_KEY": "${CLAUDE_PLUGIN_ROOT}/key.txt"}
				},
				"remote": {
					"url": "https://example.com/mcp"
				}
			}
		}`,
		"skills/web/SKILL.md": `---
name: web-search
description: 联网搜索技能
domain: research
tags: search, web
---
# Web Search
搜索步骤说明
`,
	})
	b, err := loadBundle(dir, "my-plugin")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	if b.Name != "My Search Plugin" || b.Version != "1.2.0" || b.Description != "搜索插件" {
		t.Fatalf("plugin.json 元数据解析错误: %+v", b)
	}
	if len(b.Servers) != 2 {
		t.Fatalf("应展开 2 个 server，got %d", len(b.Servers))
	}
	// stdio server：${CLAUDE_PLUGIN_ROOT} 替换为绝对路径（Servers 按名排序，按名取）。
	var search, remote MCPServer
	for _, sv := range b.Servers {
		if sv.Name == "search" {
			search = sv
		}
		if sv.Name == "remote" {
			remote = sv
		}
	}
	if search.Command != "uvx" {
		t.Fatalf("search server 解析错误: %+v", search)
	}
	abs, _ := filepath.Abs(dir)
	if len(search.Args) != 3 || search.Args[2] != abs+"/data" {
		t.Fatalf("${CLAUDE_PLUGIN_ROOT} 替换错误: %v", search.Args)
	}
	if search.Env["SEARCH_KEY"] != abs+"/key.txt" {
		t.Fatalf("env 替换错误: %v", search.Env)
	}
	if search.Env["CLAUDE_PLUGIN_ROOT"] != abs {
		t.Fatalf("应注入 CLAUDE_PLUGIN_ROOT: %v", search.Env)
	}
	// http server。
	if remote.URL != "https://example.com/mcp" || remote.Command != "" {
		t.Fatalf("http server 解析错误: %+v", remote)
	}
	// skill。
	if len(b.Skills) != 1 {
		t.Fatalf("应解析 1 个 skill，got %d", len(b.Skills))
	}
	s := b.Skills[0]
	if s.SkillID != "my_plugin_web_search" {
		t.Fatalf("SkillID 应带目录前缀: %q", s.SkillID)
	}
	if s.Name != "web-search" || s.Description != "联网搜索技能" || s.Domain != "research" {
		t.Fatalf("skill frontmatter 解析错误: %+v", s)
	}
	if !strings.Contains(s.UsageExample, "搜索步骤说明") {
		t.Fatalf("skill 正文应作为 UsageExample: %q", s.UsageExample)
	}
	if len(s.Tags) != 2 || s.Tags[0] != "search" {
		t.Fatalf("tags 解析错误: %v", s.Tags)
	}
}

func TestLoadBundleMinimal(t *testing.T) {
	dir := t.TempDir()
	// 仅 .mcp.json：目录名兜底元数据。
	writeTree(t, dir, map[string]string{
		".mcp.json": `{"mcpServers": {"s1": {"command": "npx", "args": ["-y", "x"]}}}`,
	})
	b, err := loadBundle(dir, "bare")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	if b.Name != "bare" || len(b.Servers) != 1 || b.Servers[0].Name != "s1" {
		t.Fatalf("最小包解析错误: %+v", b)
	}
}

func TestLoadBundleNotABundle(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadBundle(dir, "random"); err == nil {
		t.Fatal("无 plugin.json/.mcp.json 的目录应报错")
	}
}

func TestScanSkipsNonBundlesAndSorts(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"plugins.d/b-plug/.mcp.json":   `{"mcpServers": {"s": {"command": "a"}}}`,
		"plugins.d/a-plug/.mcp.json":   `{"mcpServers": {"s": {"command": "b"}}}`,
		"plugins.d/loose/readme.txt":   "not a plugin",
	})
	bundles, warnings := Scan(filepath.Join(root, "plugins.d"))
	if len(bundles) != 2 {
		t.Fatalf("应发现 2 个插件包，got %d", len(bundles))
	}
	if bundles[0].Dir != "a-plug" || bundles[1].Dir != "b-plug" {
		t.Fatalf("输出应按目录名排序: %v, %v", bundles[0].Dir, bundles[1].Dir)
	}
	if len(warnings) != 0 {
		t.Fatalf("loose 目录应被跳过而非告警: %v", warnings)
	}
}

func TestScanMissingDir(t *testing.T) {
	bundles, warnings := Scan(filepath.Join(t.TempDir(), "nope"))
	if len(bundles) != 0 || len(warnings) != 0 {
		t.Fatalf("缺失目录应返回空: bundles=%v warnings=%v", bundles, warnings)
	}
}

func TestLoadBundleBMAPluginJSON(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		// 三种清单共存：元数据 bma-plugin.json 优先于 plugin.json；
		// .mcp.json 的 server 与 bma-plugin.json 的 process 条目各自展开。
		".claude-plugin/plugin.json": `{"name": "Old Name", "version": "0.0.1", "description": "旧描述"}`,
		".mcp.json":                  `{"mcpServers": {"s1": {"command": "npx"}}}`,
		"bma-plugin.json": `{
			"name": "wechat-ilink",
			"version": "1.0.0",
			"description": "微信 iLink 直连桥",
			"plugins": [
				{"id": "wechat-ilink-bridge", "kind": "process",
				 "settings": {"command": "node", "args": ["bridge.mjs"], "cwd": "${PLUGIN_DIR}",
				              "env": {"ILINK_HOME": "${PLUGIN_DIR}/data"},
				              "health_url": "http://127.0.0.1:4000/health", "auto_restart": true}}
			]
		}`,
	})
	b, err := loadBundle(dir, "wechat-ilink")
	if err != nil {
		t.Fatalf("loadBundle: %v", err)
	}
	// 元数据：bma-plugin.json 优先于 .claude-plugin/plugin.json。
	if b.Name != "wechat-ilink" || b.Version != "1.0.0" || b.Description != "微信 iLink 直连桥" {
		t.Fatalf("bma-plugin.json 元数据优先解析错误: %+v", b)
	}
	// 共存：.mcp.json server 仍展开。
	if len(b.Servers) != 1 {
		t.Fatalf(".mcp.json 应共存展开 1 个 server，got %d", len(b.Servers))
	}
	// process 条目展开。
	if len(b.Processes) != 1 {
		t.Fatalf("应展开 1 个 process 条目，got %d", len(b.Processes))
	}
	pr := b.Processes[0]
	if pr.ID != "wechat-ilink-bridge" || pr.Kind != "process" {
		t.Fatalf("process 条目解析错误: %+v", pr)
	}
	abs, _ := filepath.Abs(dir)
	if pr.Settings["cwd"] != abs {
		t.Fatalf("cwd 的 ${PLUGIN_DIR} 展开错误: %v", pr.Settings["cwd"])
	}
	env, _ := pr.Settings["env"].(map[string]any)
	if env["ILINK_HOME"] != abs+"/data" {
		t.Fatalf("env 内嵌套 ${PLUGIN_DIR} 展开错误: %v", env)
	}
	if pr.Settings["command"] != "node" || pr.Settings["health_url"] != "http://127.0.0.1:4000/health" {
		t.Fatalf("settings 透传错误: %+v", pr.Settings)
	}
	if pr.Settings["auto_restart"] != true {
		t.Fatalf("布尔 settings 应原样保留: %v", pr.Settings["auto_restart"])
	}
}

func TestLoadBundleBMAPluginInvalid(t *testing.T) {
	// kind 不支持 / 缺 id：解析报错（Scan 记警告）。
	for name, content := range map[string]string{
		"bad_kind":   `{"plugins": [{"id": "x", "kind": "mcp", "settings": {}}]}`,
		"missing_id": `{"plugins": [{"kind": "process", "settings": {"command": "node"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"bma-plugin.json": content})
			if _, err := loadBundle(dir, "bad"); err == nil {
				t.Fatal("非法 bma-plugin.json 应报错")
			}
		})
	}
}

func TestScanBMAPluginOnly(t *testing.T) {
	// 仅含 bma-plugin.json 的目录也算插件包。
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"plugins.d/wx/bma-plugin.json": `{"name": "wx", "plugins": [{"id": "p1", "kind": "process", "settings": {"command": "node"}}]}`,
	})
	bundles, warnings := Scan(filepath.Join(root, "plugins.d"))
	if len(warnings) != 0 {
		t.Fatalf("不应有警告: %v", warnings)
	}
	if len(bundles) != 1 || len(bundles[0].Processes) != 1 {
		t.Fatalf("仅 bma-plugin.json 的目录应被识别为插件包: %+v", bundles)
	}
}

func TestParseSkillMDNoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(p, []byte("just body"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := parseSkillMD(p, "b")
	if err != nil {
		t.Fatal(err)
	}
	if s.SkillID != "b_plain" || s.UsageExample != "just body" {
		t.Fatalf("无 frontmatter 解析错误: %+v", s)
	}
}
