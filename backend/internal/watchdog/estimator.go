package watchdog

// TokenEstimator 根据文本估算 token 数量。
//
// 实现可以基于字节比例、字符数、分词器等不同策略。
type TokenEstimator interface {
	// Estimate 估算 text 对应的 token 数。
	//
	// 参数 text：待估算文本。
	// 返回：估算 token 数。
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
	// 空文本直接返回 0，避免返回无意义的 1。
	if text == "" {
		return 0
	}
	// 按字节长度除以 4 后向上取整（+1），作为 token 经验值。
	return len(text)/4 + 1
}
