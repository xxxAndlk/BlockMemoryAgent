package config

// external_agents_test.go 覆盖四类契约：正常加载、缺文件零行为变化、${VAR:default} 插值、归一化。

import (
	"os"
	"path/filepath"
	"testing"
)

func writeExtAgentsTmp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "external_agents.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 缺文件返回空配置不报错（零行为变化契约）。
func TestLoadExternalAgentsConfig_MissingFile(t *testing.T) {
	cfg, err := LoadExternalAgentsConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("expected empty registry, got %d", len(cfg.Agents))
	}
}

// 正常加载：字段齐全。
func TestLoadExternalAgentsConfig_Load(t *testing.T) {
	p := writeExtAgentsTmp(t, `agents:
  claude_code:
    enabled: true
    command: claude
    args: ["-p", "{{task}}"]
    timeout_sec: 600
    description: "Claude Code"
`)
	cfg, err := LoadExternalAgentsConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := cfg.Agents["claude_code"]
	if !ok {
		t.Fatal("claude_code missing")
	}
	if !e.Enabled || e.Command != "claude" || e.TimeoutSec != 600 || e.Description != "Claude Code" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if len(e.Args) != 2 || e.Args[1] != "{{task}}" {
		t.Fatalf("unexpected args: %v", e.Args)
	}
}

// ${VAR:default} 环境变量插值（复用 resolveEnvInValue）。
func TestLoadExternalAgentsConfig_EnvInterp(t *testing.T) {
	t.Setenv("BMA_EXT_TEST_CMD", "mycli")
	t.Setenv("BMA_EXT_TEST_DESC", "插值描述")
	p := writeExtAgentsTmp(t, `agents:
  custom:
    enabled: true
    command: ${BMA_EXT_TEST_CMD:fallback}
    args: ["{{task}}"]
    timeout_sec: 60
    description: "${BMA_EXT_TEST_DESC:未设置}"
`)
	cfg, err := LoadExternalAgentsConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Agents["custom"]
	if e.Command != "mycli" || e.Description != "插值描述" {
		t.Fatalf("env interp failed: %+v", e)
	}
}

// 归一化：空 command 丢弃；timeout_sec<=0 回落 1800；args 无 {{task}} 补占位符为最后 argv。
func TestLoadExternalAgentsConfig_Normalize(t *testing.T) {
	p := writeExtAgentsTmp(t, `agents:
  broken:
    enabled: true
    command: ""
    args: ["{{task}}"]
    timeout_sec: 30
  zero_timeout:
    enabled: true
    command: foo
    args: ["-p"]
    timeout_sec: 0
  neg_timeout:
    enabled: true
    command: bar
    args: []
    timeout_sec: -5
`)
	cfg, err := LoadExternalAgentsConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Agents["broken"]; ok {
		t.Fatal("empty command entry should be dropped")
	}
	e := cfg.Agents["zero_timeout"]
	if e.TimeoutSec != 1800 {
		t.Fatalf("timeout 0 should fall back to 1800, got %d", e.TimeoutSec)
	}
	if len(e.Args) != 2 || e.Args[len(e.Args)-1] != "{{task}}" {
		t.Fatalf("placeholder should be appended, got %v", e.Args)
	}
	n := cfg.Agents["neg_timeout"]
	if n.TimeoutSec != 1800 {
		t.Fatalf("negative timeout fallback failed: %d", n.TimeoutSec)
	}
	if len(n.Args) != 1 || n.Args[0] != "{{task}}" {
		t.Fatalf("empty args should gain placeholder: %v", n.Args)
	}
}

// 损坏 yaml 报错（不静默吞）。
func TestLoadExternalAgentsConfig_BadYAML(t *testing.T) {
	p := writeExtAgentsTmp(t, "agents: [unclosed")
	if _, err := LoadExternalAgentsConfig(p); err == nil {
		t.Fatal("bad yaml should error")
	}
}
