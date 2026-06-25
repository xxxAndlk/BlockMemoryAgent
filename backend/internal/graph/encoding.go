package graph

import (
	"strings"
	"unicode/utf8"
)

// SanitizeBytes 把字节转成合法 UTF-8 字符串。
// 设计意图：项目统一 UTF-8，不做 GBK 回退。Windows cmd /c 输出若为 CP936，
// 非法字节被 strings.ToValidUTF8 替换为 U+FFFD，保留长度与可读性。
func SanitizeBytes(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}
