package textutil

import "unicode/utf8"

// TruncateRunes 按 rune（字符）截断字符串，超长时追加 suffix。
// n <= 0 时返回空串，避免无意义截断。
func TruncateRunes(s string, n int, suffix string) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n]) + suffix
}

// TruncateBytes 按字节截断字符串，超长时追加 suffix。
// n <= 0 时返回空串，避免无意义截断。
func TruncateBytes(s string, n int, suffix string) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + suffix
}
