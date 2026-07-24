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
	// 不硬编码具体模型名（roles.yaml 会随可用模型切换），改为断言解析结果非空且内部一致。
	if cfg.MetaAgent.ModelConfig.Model == "" {
		t.Fatal("meta_agent.model_config.model 解析异常: 为空")
	}
	if cfg.DomainAgent.ModelConfig.Model == "" {
		t.Fatal("domain_agent.model_config.model 解析异常: 为空")
	}
	code := cfg.GetFixedRole("code_assistant")
	if code == nil {
		t.Fatal("fixed role code_assistant 未找到")
	}
	if code.ModelConfig.Temperature != 0.4 {
		t.Fatalf("code_assistant temperature 异常: got %f", code.ModelConfig.Temperature)
	}
	if code.ModelConfig.Model == "" {
		t.Fatal("code_assistant model 解析异常: 为空")
	}
	ui := cfg.GetFixedRole("ui_assistant")
	if ui == nil {
		t.Fatal("fixed role ui_assistant 未找到")
	}
	// ui_assistant 通过 YAML 锚点 *domain_model 复用 domain_agent 模型配置，
	// 断言锚点解析一致而非硬编码模型名，模型切换时本断言不会过期。
	if ui.ModelConfig.Model == "" {
		t.Fatal("ui_assistant model 解析异常: 为空")
	}
	if ui.ModelConfig.Model != cfg.DomainAgent.ModelConfig.Model {
		t.Fatalf("ui_assistant 应复用 domain_agent 模型配置(*domain_model 锚点): ui=%q domain=%q",
			ui.ModelConfig.Model, cfg.DomainAgent.ModelConfig.Model)
	}
}
