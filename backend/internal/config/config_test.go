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
	if cfg.Agent.ReflectionMaxRounds != 3 {
		t.Fatalf("reflection_max_rounds 应为 3，got %d", cfg.Agent.ReflectionMaxRounds)
	}
	if cfg.Agent.PlanExecuteMaxSteps != 12 {
		t.Fatalf("plan_execute_max_steps 应为 12，got %d", cfg.Agent.PlanExecuteMaxSteps)
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
	// 探索预算三档（任务 31 配置化；TODO #35 放开预算）：config.yaml 显式 60/60/120。
	if cfg.Agent.ExploreBudget != 60 {
		t.Fatalf("explore_budget 应为 60，got %d", cfg.Agent.ExploreBudget)
	}
	if cfg.Agent.ExploreBudgetDomain != 60 {
		t.Fatalf("explore_budget_domain 应为 60，got %d", cfg.Agent.ExploreBudgetDomain)
	}
	if cfg.Agent.ExploreBudgetPostWrite != 120 {
		t.Fatalf("explore_budget_post_write 应为 120，got %d", cfg.Agent.ExploreBudgetPostWrite)
	}
	// task 双档上限（TODO #35）：config.yaml 显式 3000/4000。
	if cfg.Agent.TaskMaxRunes != 3000 {
		t.Fatalf("task_max_runes 应为 3000，got %d", cfg.Agent.TaskMaxRunes)
	}
	if cfg.Agent.TaskMaxRunesHard != 4000 {
		t.Fatalf("task_max_runes_hard 应为 4000，got %d", cfg.Agent.TaskMaxRunesHard)
	}
}

// TestApplyDefaults_ExploreBudgetDefaults 验证未配置时 applyDefaults 兜底 20/8/40。
func TestApplyDefaults_ExploreBudgetDefaults(t *testing.T) {
	c := &Config{}
	if err := c.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c.Agent.ExploreBudget != 20 {
		t.Fatalf("ExploreBudget 默认应为 20，got %d", c.Agent.ExploreBudget)
	}
	if c.Agent.ExploreBudgetDomain != 8 {
		t.Fatalf("ExploreBudgetDomain 默认应为 8，got %d", c.Agent.ExploreBudgetDomain)
	}
	if c.Agent.ExploreBudgetPostWrite != 40 {
		t.Fatalf("ExploreBudgetPostWrite 默认应为 40，got %d", c.Agent.ExploreBudgetPostWrite)
	}
}

// TestApplyDefaults_TaskRuneLimits 验证未配置时 task 双档上限兜底 3000/4000（TODO #35）。
func TestApplyDefaults_TaskRuneLimits(t *testing.T) {
	c := &Config{}
	if err := c.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c.Agent.TaskMaxRunes != 3000 {
		t.Fatalf("TaskMaxRunes 默认应为 3000，got %d", c.Agent.TaskMaxRunes)
	}
	if c.Agent.TaskMaxRunesHard != 4000 {
		t.Fatalf("TaskMaxRunesHard 默认应为 4000，got %d", c.Agent.TaskMaxRunesHard)
	}
	// 显式配置覆盖默认，不被兜底改写。
	c2 := &Config{}
	c2.Agent.TaskMaxRunes, c2.Agent.TaskMaxRunesHard = 5000, 6000
	if err := c2.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c2.Agent.TaskMaxRunes != 5000 || c2.Agent.TaskMaxRunesHard != 6000 {
		t.Fatalf("显式配置应保留，got %d/%d", c2.Agent.TaskMaxRunes, c2.Agent.TaskMaxRunesHard)
	}
}
