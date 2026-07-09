package test

import (
	"path/filepath"
	"runtime"
	"testing"

	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
)

func TestRoleConfigParsesWithAnchors(t *testing.T) {
	// 定位仓库根目录下的 config/roles.yaml
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法获取当前文件路径")
	}
	root := filepath.Join(filepath.Dir(file), "..")
	rolesPath := filepath.Join(root, "config", "roles.yaml")

	cfg, err := pkgconfig.LoadRoleConfig(rolesPath)
	if err != nil {
		t.Fatalf("解析 roles.yaml 失败: %v", err)
	}
	if cfg.MetaAgent.MaxBlocks != 20 {
		t.Fatalf("meta_agent.max_blocks 解析异常: got %d", cfg.MetaAgent.MaxBlocks)
	}
	if cfg.DomainAgent.ModelConfig.Model != "kimi-for-coding" {
		t.Fatalf("domain_agent.model_config.model 解析异常: got %q", cfg.DomainAgent.ModelConfig.Model)
	}
	code := cfg.GetFixedRole("code_assistant")
	if code == nil {
		t.Fatal("fixed role code_assistant 未找到")
	}
	if code.ModelConfig.Temperature != 0.4 {
		t.Fatalf("code_assistant temperature 异常: got %f", code.ModelConfig.Temperature)
	}
	ui := cfg.GetFixedRole("ui_assistant")
	if ui == nil {
		t.Fatal("fixed role ui_assistant 未找到")
	}
	if ui.ModelConfig.Model != "kimi-for-coding" {
		t.Fatalf("ui_assistant model 异常: got %q", ui.ModelConfig.Model)
	}
}
