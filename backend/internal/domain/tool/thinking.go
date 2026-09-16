package tool

import "strings"

// thinking.go 定义会话级思考强度枚举（2026-09-16 会话级思考强度）：
// 会话创建时随档位一并选择、运行中可改，只覆盖本会话顶层 Agent 的角色思考档，
// 经 ModelFactory 的 thinking 覆盖通道热生效（下一次 LLM 调用即用新档，免重启）。
// 空串 = 跟随角色默认（roles.yaml / models.json role_bindings 解析结果，零覆盖）。

const (
	ThinkingOff    = "off"
	ThinkingLow    = "low"
	ThinkingMedium = "medium"
	ThinkingHigh   = "high"
)

// ValidThinking 校验会话思考强度（空串合法 = 跟随角色默认）。
func ValidThinking(t string) bool {
	switch t {
	case "", ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh:
		return true
	}
	return false
}

// NormalizeThinking 归一化思考强度输入（trim + lower）；非枚举值由调用方 ValidThinking 拒绝。
func NormalizeThinking(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}
