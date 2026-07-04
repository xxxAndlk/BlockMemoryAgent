package model

import (
	"fmt"
	"time"
)

// Stats 获取超时统计（兼容原接口）。
//
// 返回：
//   - callCount: 累计调用次数
//   - timeoutCount: 连续超时次数
//   - avgDur: 平均耗时
//   - maxDur: 最大耗时
//
// 并发安全：读锁保护。
func (t *LLMCallTracker) Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	avg := time.Duration(0)
	if t.callCount > 0 {
		avg = t.totalDur / time.Duration(t.callCount) // 计算平均
	}
	return t.callCount, t.timeoutCount, avg, t.maxDur
}

// StatsString 统计摘要（兼容原接口）。
//
// 返回：单行可读字符串，含调用数/超时数/平均/最大/慢速模式/token 总量。
// 副作用：调用 Stats 与 TokenTotals（均加锁）。
// 并发安全：间接通过子方法加锁。
func (t *LLMCallTracker) StatsString() string {
	calls, timeouts, avg, max := t.Stats()
	totalIn, totalOut := t.TokenTotals()
	return fmt.Sprintf(
		"calls=%d, timeouts=%d, avg=%v, max=%v, skip=%v, inputTokens=%d, outputTokens=%d",
		calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), t.slowMode, totalIn, totalOut,
	)
}

// TokenTotals 获取总输入/输出 token。
//
// 返回：
//   - inputTokens: 所有记录的输入 token 之和
//   - outputTokens: 所有记录的输出 token 之和
//
// 并发安全：读锁保护。
func (t *LLMCallTracker) TokenTotals() (inputTokens, outputTokens int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// 遍历累加
	for _, r := range t.records {
		inputTokens += r.InputTokens
		outputTokens += r.OutputTokens
	}
	return
}

// Records 获取所有调用记录（拷贝）。
//
// 设计意图：返回副本避免外部修改内部状态。
// 返回：[]CallRecord 拷贝。
// 并发安全：读锁保护。
func (t *LLMCallTracker) Records() []CallRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	// 拷贝切片避免泄漏内部引用
	out := make([]CallRecord, len(t.records))
	copy(out, t.records)
	return out
}

