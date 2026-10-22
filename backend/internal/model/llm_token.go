package model

import (
	"fmt"          // 字符串格式化
	"unicode/utf8" // UTF-8 rune 统计
)

// token 估算标定（2026-10-22）：旧口径"非 ASCII 1 字 1 token / ASCII 4 字符 1 token"
// 对中文+JSON/代码混合内容明显偏低，导致 150K token 预算迟迟不触发压缩、请求实际
// 体积远超阈值（事故会话 prompt 峰值 34 万字符，单次 input 42.6 万 token）。
// 新口径按主流 tokenizer 的中位经验值：中文/全角 1.4 token/字，ASCII 3.2 字符/token。
// 估算仍不追求与某一家 tokenizer 对齐；偏高会提前压缩，代价可控，偏低会漏压。
const (
	nonASCIITokensPerRune = 1.4
	asciiCharsPerToken    = 3.2
	// ImageTokenEstimate 单张图片的估算 token 数（按 1024×1024 量级取中位值）。
	// 图片不走文本 tokenizer（按面积计费），旧估算器完全不计，是"估算正常但真实
	// 请求爆掉"的盲区。
	ImageTokenEstimate = 1600
)

// EstimateTokens 粗略估算文本的 token 数量。
// 策略见上方标定常量：中文/全角 1.4 token/字，ASCII 3.2 字符/token。
// 这是本地预算估算值，不追求与某一家 tokenizer 完全对齐。
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
	// 空文本直接返回 0，避免无意义计算
	if text == "" {
		return 0
	}
	return refineEstimate(text, 0)
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
	// 统计 rune 总数
	runes := utf8.RuneCountInString(text)
	// ASCII 字符数
	asciiCount := 0
	// 非 ASCII 字符数
	nonAsciiCount := 0
	// 遍历 rune 分类统计
	for _, r := range text {
		if r <= 127 {
			asciiCount++
		} else {
			nonAsciiCount++
		}
	}
	// 中文/全角 1.4 token/字，ASCII 3.2 字符 ≈ 1 token（JSON/代码比英文散文更密）。
	tokens := int(float64(nonAsciiCount)*nonASCIITokensPerRune + float64(asciiCount)/asciiCharsPerToken)
	// 若文本非空但计算结果小于 1，则至少返回 1
	if tokens < 1 && runes > 0 {
		tokens = 1
	}
	// 返回修正后的估算值（rough 参数保留以兼容旧签名）
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
	// 短 prompt 直接返回，无需截断
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
