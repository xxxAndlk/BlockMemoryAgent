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
	if cfg.Agent.DAGEnabled {
		t.Fatal("dag_enabled 应为 false")
	}
	// 块记忆写入开关未在 config.yaml 中显式配置时，applyDefaults 应兜底为 true。
	if cfg.Agent.BlockMemoryWriteEnabled == nil || !*cfg.Agent.BlockMemoryWriteEnabled {
		t.Fatal("block_memory_write_enabled 默认应为 true")
	}
	// Spec 强制默认开启：config.yaml 显式 true（塔防实证 WriteSpec 有用）。
	if cfg.Agent.SpecEnforcementEnabled == nil || !*cfg.Agent.SpecEnforcementEnabled {
		t.Fatal("spec_enforcement_enabled 默认应为 true")
	}
	// 派发执行模式引擎参数（TODO #29）：config.yaml 显式配置 + applyDefaults 兜底。
	if cfg.Agent.ReflectionMaxRounds != 2 {
		t.Fatalf("reflection_max_rounds 应为 2，got %d", cfg.Agent.ReflectionMaxRounds)
	}
	if cfg.Agent.PlanExecuteMaxSteps != 8 {
		t.Fatalf("plan_execute_max_steps 应为 8，got %d", cfg.Agent.PlanExecuteMaxSteps)
	}
	// 失败打捞超时（TODO #33）：config.yaml 显式 60（思考型模型下限）。
	if cfg.Agent.SalvageLLMTimeoutSec != 60 {
		t.Fatalf("salvage_llm_timeout_sec 应为 60，got %d", cfg.Agent.SalvageLLMTimeoutSec)
	}
	// 输入补全默认开启（TODO #36）：config.yaml 显式 true。
	if cfg.Agent.PromptEnhance == nil || !*cfg.Agent.PromptEnhance {
		t.Fatal("prompt_enhance 默认应为 true")
	}
	// 软停止销毁倒计时（TODO #37）：config.yaml 显式 300。
	if cfg.Agent.StopDestroyCountdownSec != 300 {
		t.Fatalf("stop_destroy_countdown_sec 应为 300，got %d", cfg.Agent.StopDestroyCountdownSec)
	}
}
