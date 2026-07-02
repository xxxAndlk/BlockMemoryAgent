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
}
