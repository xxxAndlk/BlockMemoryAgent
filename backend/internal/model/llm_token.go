package model

import (
	"fmt"
	"unicode/utf8"
)

// EstimateTokens 粗略估算文本的 token 数量。
// 策略：中文字符按 1 字 ≈ 1 token；ASCII 按 4 字符 ≈ 1 token。
// 这是本地调试的估算值，不追求与 OpenAI tokenizer 完全对齐。
//
// 参数：
//   - text: 待估算文本
//
// 返回：
//   - int: 估算 token 数（至少 0；非空文本至少 1）
//
// 副作用：无。
// 并发安全：纯函数。
func EstimateTokens(text string) int {
	if text == "" {
		return 0 // 空文本直接返回 0
	}
	var tokens int
	// 第一遍粗估：遍历 rune，非 ASCII 计 1，ASCII 也计 1（后续会被 refine 修正）
	for _, r := range text {
		if r > 127 {
			// 非 ASCII（中文、 emoji 等）按 1 字 1 token
			tokens++
		} else {
			// ASCII 按 4 字符 1 token，至少 1 token
			tokens += 1
		}
	}
	// 对 ASCII 总体再除 4，避免每个字符都累加导致偏高的误差
	// 重新用更精确的算法：统计 rune 数量
	return refineEstimate(text, tokens)
}

// refineEstimate 用更精确的算法重算 token 数。
// 参数：
//   - text: 原始文本
//   - rough: 第一遍粗估结果（当前未直接使用，保留参数以兼容旧签名）
//
// 返回：
//   - int: 修正后的 token 数
//
// 副作用：无。
func refineEstimate(text string, rough int) int {
	runes := utf8.RuneCountInString(text) // rune 总数
	asciiCount := 0                       // ASCII 字符数
	nonAsciiCount := 0                    // 非 ASCII 字符数
	for _, r := range text {
		if r <= 127 {
			asciiCount++
		} else {
			nonAsciiCount++
		}
	}
	// ASCII 部分 4 字符 ≈ 1 token，非 ASCII 1 字符 ≈ 1 token
	tokens := nonAsciiCount + asciiCount/4
	// 至少返回 1（若文本非空）
	if tokens < 1 && runes > 0 {
		tokens = 1
	}
	return tokens
}

// SummarizePrompt 截断 prompt 到前 N 个字符，保留首尾用于摘要展示。
//
// 设计意图：完整 prompt 可能极长，摘要需在有限篇幅内保留首尾关键信息。
// 参数：
//   - prompt: 原始提示词
//   - maxLen: 摘要最大长度（首部 maxLen/2，尾部 maxLen/4）
//
// 返回：
//   - string: 截断后的摘要（含 "... (truncated N chars) ..." 标记）
//
// 副作用：无。
func SummarizePrompt(prompt string, maxLen int) string {
	// 短 prompt 直接返回
	if len(prompt) <= maxLen {
		return prompt
	}
	// 取首部一半长度
	head := prompt[:maxLen/2]
	// 取尾部四分之一长度
	tail := prompt[len(prompt)-maxLen/4:]
	// 拼接首尾并标注被截断的字符数
	return head + "\n... (truncated " + fmt.Sprintf("%d", len(prompt)-maxLen/2-maxLen/4) + " chars) ...\n" + tail
}
