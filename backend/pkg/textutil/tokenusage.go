package textutil

import (
	"strconv"
	"strings"
	"time"
)

// ParseTokenUsage 从 token 使用消息中提取输入/输出 Token 数。
// 期望消息中包含 "in=<n>" 与 "out=<n>" 标记。
//
// 参数: msg 待解析的消息字符串。
// 返回: in 输入 Token 数，out 输出 Token 数；未找到标记时返回 0。
func ParseTokenUsage(msg string) (in, out int) {
	// 提取 in= 后的整数值作为输入 Token 数。
	in = extractIntAfter(msg, "in=")
	// 提取 out= 后的整数值作为输出 Token 数。
	out = extractIntAfter(msg, "out=")
	// 使用命名返回值，直接返回。
	return
}

// ParseDurationFromTokenUsage 从 token 使用消息中提取执行耗时。
// 期望消息中包含 "dur=<duration>" 标记，duration 需符合 time.ParseDuration 格式。
//
// 参数: msg 待解析的消息字符串。
// 返回: 解析成功返回对应 time.Duration；未找到或解析失败返回 0。
func ParseDurationFromTokenUsage(msg string) time.Duration {
	// 定位 "dur=" 在消息中的位置。
	idx := strings.Index(msg, "dur=")
	// 未找到标记时直接返回 0。
	if idx < 0 {
		return 0
	}
	// 计算 duration 子串的起始位置。
	start := idx + len("dur=")
	// end 从 start 开始向后移动，直到遇到空白或字符串末尾。
	end := start
	for end < len(msg) && msg[end] != ' ' && msg[end] != '\t' {
		end++
	}
	// 若 duration 子串为空，返回 0。
	if end <= start {
		return 0
	}
	// 尝试解析 duration 子串；解析成功则返回，失败保持 0。
	if dur, err := time.ParseDuration(msg[start:end]); err == nil {
		return dur
	}
	return 0
}

// extractIntAfter 从字符串 s 中定位 marker，并提取其后的整数值。
// 支持可选的前导空白与负号。
//
// 参数:
//   - s: 待搜索的字符串。
//   - marker: 要定位的标记。
//
// 返回: marker 后紧跟的整数；未找到 marker 或无法解析时返回 0。
func extractIntAfter(s, marker string) int {
	// 定位标记位置。
	idx := strings.Index(s, marker)
	if idx < 0 {
		return 0
	}
	// 计算数字起始位置（跳过标记本身）。
	start := idx + len(marker)
	// 跳过后续空白字符，允许 "in= 123" 这种写法。
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	// 累积解析十进制数字。
	n := 0
	// 记录是否为负数。
	signed := false
	// 若存在负号，标记 signed 并跳过。
	if start < len(s) && s[start] == '-' {
		signed = true
		start++
	}
	// 逐位读取数字并累加。
	for start < len(s) && s[start] >= '0' && s[start] <= '9' {
		n = n*10 + int(s[start]-'0')
		start++
	}
	// 若带有负号，结果取反。
	if signed {
		n = -n
	}
	return n
}

// ParseInt 是对 strconv.Atoi 的薄封装，解析失败时返回 0。
//
// 参数: s 要解析的十进制整数字符串。
// 返回: 解析后的整数；错误被忽略并返回 0。
func ParseInt(s string) int {
	// 调用标准库解析整数，忽略错误。
	n, _ := strconv.Atoi(s)
	return n
}
