package config

// harness_modes.go Harness 根治开关的默认值与非法值回落（计划 §5.4）。
//
// 四组开关：
//
//	agent.logical_task_mode            off|shadow|enforce   默认 shadow
//	agent.action_transaction_mode      off|shadow|enforce   默认 off
//	agent.semantic_edit_mode           off|shadow|enforce   默认 off
//	agent.contract_verification_mode   off|shadow|enforce   默认 shadow
//
// 纪律：
//   - 默认只观察：logical_task/contract_verification 先跑 shadow 对拍，
//     action_transaction/semantic_edit 先关（它们会改变执行语义，需先有合同事实）；
//   - 非法值一律回落最保守档并打日志（不静默接受拼写错误）；
//   - 每个开关独立回退：关掉新功能不得删除事件、检查点或旧会话数据。

import "log"

// harnessModeOff / Shadow / Enforce 三档常量（与 team.LogicalTaskMode* 同值，
// config 包不 import team 以免配置层依赖业务层）。
const (
	HarnessModeOff     = "off"
	HarnessModeShadow  = "shadow"
	HarnessModeEnforce = "enforce"
)

// normalizeHarnessMode 归一化单个开关：非法值回落 fallback 并告警。
func normalizeHarnessMode(name, value, fallback string) string {
	switch v := lowerTrim(value); v {
	case HarnessModeOff, HarnessModeShadow, HarnessModeEnforce:
		return v
	case "":
		return fallback
	default:
		log.Printf("[config] illegal %s %q, fallback %s", name, value, fallback)
		return fallback
	}
}

func lowerTrim(s string) string {
	// 与 config.go 的既有模式解析保持同一口径（小写 + 去空白）。
	b := make([]byte, 0, len(s))
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	return string(b)
}

// applyHarnessModeDefaults 填充 Harness 根治开关默认值（幂等）。
func (c *Config) applyHarnessModeDefaults() {
	if c == nil {
		return
	}
	c.Agent.LogicalTaskMode = normalizeHarnessMode("logical_task_mode", c.Agent.LogicalTaskMode, HarnessModeShadow)
	c.Agent.ActionTransactionMode = normalizeHarnessMode("action_transaction_mode", c.Agent.ActionTransactionMode, HarnessModeOff)
	c.Agent.SemanticEditMode = normalizeHarnessMode("semantic_edit_mode", c.Agent.SemanticEditMode, HarnessModeOff)
	c.Agent.ContractVerificationMode = normalizeHarnessMode("contract_verification_mode", c.Agent.ContractVerificationMode, HarnessModeShadow)
	if c.Agent.CheckpointMaxFiles <= 0 {
		c.Agent.CheckpointMaxFiles = 20000
	}
	if c.Agent.LogicalTaskContractMaxRunes <= 0 {
		c.Agent.LogicalTaskContractMaxRunes = 2200
	}
}
