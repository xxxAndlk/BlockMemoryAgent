package watchdog

// TokenEstimator 根据文本估算 token 数量。
//
// 实现可以基于字节比例、字符数、分词器等不同策略。
type TokenEstimator interface {
	Estimate(text string) int
}

// ByteRatioEstimator 使用“4 字节 ≈ 1 token”的经验公式估算 token 数。
//
// 注意：按字节（byte）而非 rune 计算，以兼容当前 Watchdog 的阈值设定。
type ByteRatioEstimator struct{}

// Estimate 实现 TokenEstimator。
//
// 公式：len(text)/4 + 1；空文本返回 0。
func (ByteRatioEstimator) Estimate(text string) int {
	if text == "" {
		return 0
	}
	return len(text)/4 + 1
}
