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
	if !cfg.Agent.DAGEnabled {
		t.Fatal("dag_enabled 应为 true")
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
	// 校验 judge 角色（TODO #43）：config.yaml 显式 prompt_reviewer。
	if cfg.Agent.JudgeRole != "prompt_reviewer" {
		t.Fatalf("judge_role 应为 prompt_reviewer，got %q", cfg.Agent.JudgeRole)
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
	// task 双档上限（TODO #35）：config.yaml 显式 3000/4000。
	if cfg.Agent.TaskMaxRunes != 3000 {
		t.Fatalf("task_max_runes 应为 3000，got %d", cfg.Agent.TaskMaxRunes)
	}
	if cfg.Agent.TaskMaxRunesHard != 4000 {
		t.Fatalf("task_max_runes_hard 应为 4000，got %d", cfg.Agent.TaskMaxRunesHard)
	}
	// 新会话默认工作目录（2026-09-26）：config.yaml 显式 workspace（解析到安装目录下）。
	if cfg.Agent.DefaultWorkDir != "workspace" {
		t.Fatalf("default_workdir 应为 workspace，got %q", cfg.Agent.DefaultWorkDir)
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

// TestApplyDefaults_HTTPAddrLoopback 验证 HTTP 地址缺省只绑本机回环（TODO #18 T27）：
// 未配置时兜底 127.0.0.1:10010（防局域网裸奔）；显式配置（0.0.0.0:10010 等）保留不被改写。
func TestApplyDefaults_HTTPAddrLoopback(t *testing.T) {
	c := &Config{}
	if err := c.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c.HTTP.Addr != "127.0.0.1:10010" {
		t.Fatalf("HTTP.Addr 默认应为 127.0.0.1:10010，got %q", c.HTTP.Addr)
	}
	c2 := &Config{}
	c2.HTTP.Addr = "0.0.0.0:10010"
	if err := c2.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c2.HTTP.Addr != "0.0.0.0:10010" {
		t.Fatalf("显式配置应保留，got %q", c2.HTTP.Addr)
	}
}

// TestApplyDefaults_ContextTokenBudget 验证未配置时上下文 token 阈值兜底 150000。
func TestApplyDefaults_ContextTokenBudget(t *testing.T) {
	c := &Config{}
	if err := c.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c.Agent.ContextTokenBudget != 150000 {
		t.Fatalf("ContextTokenBudget 默认应为 150000，got %d", c.Agent.ContextTokenBudget)
	}
	// 显式配置覆盖默认，不被兜底改写。
	c2 := &Config{}
	c2.Agent.ContextTokenBudget = 200000
	if err := c2.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if c2.Agent.ContextTokenBudget != 200000 {
		t.Fatalf("显式配置应保留，got %d", c2.Agent.ContextTokenBudget)
	}
}
