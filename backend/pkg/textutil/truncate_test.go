package textutil

import "testing"

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		s      string
		n      int
		suffix string
		want   string
	}{
		{"hello", 10, "...", "hello"},
		{"hello world", 5, "...", "hello..."},
		{"中文测试", 2, "...", "中文..."},
		{"", 5, "...", ""},
		{"x", 0, "...", ""},
	}
	for _, c := range cases {
		got := TruncateRunes(c.s, c.n, c.suffix)
		if got != c.want {
			t.Errorf("TruncateRunes(%q, %d, %q) = %q, want %q", c.s, c.n, c.suffix, got, c.want)
		}
	}
}

func TestTruncateBytes(t *testing.T) {
	cases := []struct {
		s      string
		n      int
		suffix string
		want   string
	}{
		{"hello", 10, "...", "hello"},
		{"hello world", 5, "...", "hello..."},
		{"中文", 3, "...", "中..."}, // 中 = 3 bytes
		{"", 5, "...", ""},
		{"x", 0, "...", ""},
	}
	for _, c := range cases {
		got := TruncateBytes(c.s, c.n, c.suffix)
		if got != c.want {
			t.Errorf("TruncateBytes(%q, %d, %q) = %q, want %q", c.s, c.n, c.suffix, got, c.want)
		}
	}
}
