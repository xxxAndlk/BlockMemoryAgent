package textutil

import "testing"

// TestTruncateRunes 测试 TruncateRunes 按 rune 截断的行为。
// 覆盖未超长、超长追加后缀、中文多字节字符、空串、n<=0 等场景。
func TestTruncateRunes(t *testing.T) {
	// 定义测试用例：输入字符串、保留 rune 数、后缀、期望输出。
	cases := []struct {
		s      string
		n      int
		suffix string
		want   string
	}{
		{"hello", 10, "...", "hello"},         // 原串短于限制，应原样返回
		{"hello world", 5, "...", "hello..."}, // 超长时保留 5 个 rune 并追加后缀
		{"中文测试", 2, "...", "中文..."},           // 中文按 rune 截断，不破坏字符
		{"", 5, "...", ""},                    // 空串返回空串
		{"x", 0, "...", ""},                   // n<=0 返回空串
	}
	// 逐个执行用例并断言。
	for _, c := range cases {
		// 调用被测函数得到实际输出。
		got := TruncateRunes(c.s, c.n, c.suffix)
		// 实际输出与期望不符时报错。
		if got != c.want {
			t.Errorf("TruncateRunes(%q, %d, %q) = %q, want %q", c.s, c.n, c.suffix, got, c.want)
		}
	}
}

// TestTruncateBytes 测试 TruncateBytes 按字节截断的行为。
// 覆盖未超长、超长追加后缀、中文多字节字符按字节切分、空串、n<=0 等场景。
func TestTruncateBytes(t *testing.T) {
	// 定义测试用例。
	cases := []struct {
		s      string
		n      int
		suffix string
		want   string
	}{
		{"hello", 10, "...", "hello"},         // 原串短于限制，应原样返回
		{"hello world", 5, "...", "hello..."}, // 超长时保留 5 字节并追加后缀
		{"中文", 3, "...", "中..."},              // 中文字符 3 字节，按字节截断可能只保留部分字节
		{"", 5, "...", ""},                    // 空串返回空串
		{"x", 0, "...", ""},                   // n<=0 返回空串
	}
	// 逐个执行用例并断言。
	for _, c := range cases {
		got := TruncateBytes(c.s, c.n, c.suffix)
		if got != c.want {
			t.Errorf("TruncateBytes(%q, %d, %q) = %q, want %q", c.s, c.n, c.suffix, got, c.want)
		}
	}
}
