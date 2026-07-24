package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesFeatureFlags(t *testing.T) {
	// 使用仓库根目录的 config/config.yaml 验证特性开关解析
	root, _ := os.Getwd()
	// 当前在 backend/internal/config，需上溯三级到项目根
	cfgPath := filepath.Join(root, "..", "..", "..", "config", "config.yaml")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("加载 config.yaml 失败: %v", err)
	}
	if cfg.Agent.InterruptEnabled {
		t.Fatal("interrupt_enabled 应为 false")
	}
	if cfg.Agent.QueueInjectEnabled {
		t.Fatal("queue_inject_enabled 应为 false")
	}
	if cfg.Agent.HumanClarifyEnabled {
		t.Fatal("human_clarify_enabled 应为 false")
	}
	if cfg.Agent.DAGEnabled {
		t.Fatal("dag_enabled 应为 false")
	}
	if cfg.Agent.PlanEnabled {
		t.Fatal("plan_enabled 应为 false")
	}
	if cfg.Agent.ReflectionEnabled {
		t.Fatal("reflection_enabled 应为 false")
	}
	// 块记忆写入开关未在 config.yaml 中显式配置时，applyDefaults 应兜底为 true。
	if cfg.Agent.BlockMemoryWriteEnabled == nil || !*cfg.Agent.BlockMemoryWriteEnabled {
		t.Fatal("block_memory_write_enabled 默认应为 true")
	}
	// 验证往返上限应有非零默认值。
	if cfg.Agent.VerificationMaxRounds != 5 {
		t.Fatalf("verification_max_rounds 默认应为 5, got %d", cfg.Agent.VerificationMaxRounds)
	}
	// 验证角色对应回退默认 [{code_assistant, test_assistant}]。
	if len(cfg.Agent.VerificationRolePairs) != 1 {
		t.Fatalf("expected 1 default role pair, got %d", len(cfg.Agent.VerificationRolePairs))
	}
	p := cfg.Agent.VerificationRolePairs[0]
	if p.CodeRole != "code_assistant" || p.TestRole != "test_assistant" {
		t.Fatalf("default pair mismatch: %+v", p)
	}
}
