package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMergesSplitConfigs(t *testing.T) {
	// 复制仓库的 config/config.yaml 到临时目录，并叠加拆分配置验证合并行为。
	tmp := t.TempDir()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录失败: %v", err)
	}
	base := filepath.Join(repoRoot, "..", "..", "..", "config", "config.yaml")
	baseData, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("读取基准 config.yaml 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), baseData, 0644); err != nil {
		t.Fatalf("写入临时 config.yaml 失败: %v", err)
	}

	// infrastructure.yaml 覆盖 Redis 地址
	infra := []byte(`
redis:
  addr: "redis-override:6379"
`)
	if err := os.WriteFile(filepath.Join(tmp, "infrastructure.yaml"), infra, 0644); err != nil {
		t.Fatalf("写入 infrastructure.yaml 失败: %v", err)
	}

	// agent-policy.yaml 覆盖 Agent 参数
	policy := []byte(`
agent:
  max_total_dispatches: 42
`)
	if err := os.WriteFile(filepath.Join(tmp, "agent-policy.yaml"), policy, 0644); err != nil {
		t.Fatalf("写入 agent-policy.yaml 失败: %v", err)
	}

	cfg, err := Load(filepath.Join(tmp, "config.yaml"))
	if err != nil {
		t.Fatalf("加载合并配置失败: %v", err)
	}
	if cfg.Redis.Addr != "redis-override:6379" {
		t.Fatalf("infrastructure.yaml 未生效: redis.addr=%q", cfg.Redis.Addr)
	}
	if cfg.Agent.MaxTotalDispatches != 42 {
		t.Fatalf("agent-policy.yaml 未生效: agent.max_total_dispatches=%d", cfg.Agent.MaxTotalDispatches)
	}
	// 未覆盖字段仍保留原值
	if cfg.Postgres.MaxOpenConns != 25 {
		t.Fatalf("未覆盖字段被改动: postgres.max_open_conns=%d", cfg.Postgres.MaxOpenConns)
	}
}

func TestLoadBackwardCompatibleWhenSplitFilesMissing(t *testing.T) {
	// 仅存在 config.yaml 时，Load 必须仍能正常工作。
	tmp := t.TempDir()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatalf("获取当前目录失败: %v", err)
	}
	base := filepath.Join(repoRoot, "..", "..", "..", "config", "config.yaml")
	baseData, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("读取基准 config.yaml 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "config.yaml"), baseData, 0644); err != nil {
		t.Fatalf("写入临时 config.yaml 失败: %v", err)
	}

	cfg, err := Load(filepath.Join(tmp, "config.yaml"))
	if err != nil {
		t.Fatalf("独立 config.yaml 加载失败: %v", err)
	}
	if cfg.Agent.MaxTotalDispatches != 30 {
		t.Fatalf("默认值异常: agent.max_total_dispatches=%d", cfg.Agent.MaxTotalDispatches)
	}
}
