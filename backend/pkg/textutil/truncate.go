package textutil

import "unicode/utf8"

// TruncateRunes 按 rune（字符）截断字符串，超长时追加 suffix。
//
// 参数:
//   - s: 原始字符串。
//   - n: 保留的最大 rune 数量；<=0 时返回空串，避免无意义截断。
//   - suffix: 截断后追加的后缀（如 "..."）。
//
// 返回: 截断后的字符串；未超限时返回原串。
func TruncateRunes(s string, n int, suffix string) string {
	// n <= 0 表示调用方不要求保留任何字符，直接返回空串。
	if n <= 0 {
		return ""
	}
	// 若原串 rune 数不超过 n，无需截断，直接返回原串以保持完整语义。
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	// 将字符串转换为 rune 切片，按字符边界安全切片，避免截断多字节字符。
	runes := []rune(s)
	// 取前 n 个 rune 并追加 suffix，组成截断结果。
	return string(runes[:n]) + suffix
}

// TruncateBytes 按字节截断字符串，超长时追加 suffix。
//
// 参数:
//   - s: 原始字符串。
//   - n: 保留的最大字节数；<=0 时返回空串，避免无意义截断。
//   - suffix: 截断后追加的后缀。
//
// 返回: 截断后的字符串；未超限时返回原串。
// 注意: 按字节截断可能破坏多字节 UTF-8 字符，适用于长度限制以字节计的场景。
func TruncateBytes(s string, n int, suffix string) string {
	// n <= 0 时直接返回空串。
	if n <= 0 {
		return ""
	}
	// 若原串字节长度不超过 n，无需截断。
	if len(s) <= n {
		return s
	}
	// 按字节切片并追加后缀；调用方需确认字节边界语义可接受。
	return s[:n] + suffix
}
