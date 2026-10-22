package model

import "testing"

// TestEstimateTokensCalibrated 验证中文/代码混合文本的估算不再严重偏低
// （2026-10-22：旧口径把 34 万字符估成 15 万 token，压缩阈值不触发）。
func TestEstimateTokensCalibrated(t *testing.T) {
	cases := []struct {
		name string
		text string
		min  int
		max  int
	}{
		{"纯中文 1000 字", repeatRune('中', 1000), 1200, 1600},
		{"纯 ASCII 3200 字符", repeatRune('a', 3200), 900, 1100},
		{"空串", "", 0, 0},
	}
	for _, c := range cases {
		got := EstimateTokens(c.text)
		if got < c.min || got > c.max {
			t.Errorf("%s: got %d, want [%d,%d]", c.name, got, c.min, c.max)
		}
	}
}

func repeatRune(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}
