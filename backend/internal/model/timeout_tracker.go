package model

// 本文件实现 TimeoutTracker：LLM 调用自适应超时追踪器。
// 提供超时统计、连续超时进入慢速模式（跳过 LLM）等能力。
// 注意：LLMCallTracker 在此基础上扩展了 token/prompt 维度，新代码建议优先使用后者。

import (
	"context" // 上下文与超时
	"fmt"     // 错误格式化
	"sync"    // 读写锁保护并发访问
	"time"    // 时间统计
)

// TimeoutTracker LLM调用超时追踪器。
// 设计意图：记录调用次数/耗时/连续超时，自适应选择超时阈值，
// 并在连续超时达到阈值时进入慢速模式跳过 LLM，避免雪崩。
type TimeoutTracker struct {
	mu           sync.RWMutex  // 读写锁保护所有字段
	callCount    int           // 累计调用次数
	timeoutCount int           // 连续超时次数（成功时清零）
	totalDur     time.Duration // 累计耗时，用于计算平均
	maxDur       time.Duration // 历史最大耗时
	lastDur      time.Duration // 最近一次耗时
	slowMode     bool          // 连续超时后进入慢速模式，跳过LLM
}

// NewTimeoutTracker 创建超时追踪器。
//
// 返回：*TimeoutTracker（所有字段零值）。
// 副作用：无。
// 并发安全：返回实例可被多协程共享。
func NewTimeoutTracker() *TimeoutTracker {
	return &TimeoutTracker{}
}

// CallWithTimeout 带自适应超时的LLM调用。
//
// 职责：
//   - 慢速模式下直接跳过 LLM
//   - 根据历史平均耗时自适应选择 fast/slow 超时
//   - 执行调用并记录
//
// 参数：
//   - ctx: 上下文
//   - llm: LLM 客户端
//   - prompt: 提示词
//   - fastTimeout: 正常模式超时
//   - slowTimeout: 已有成功调用时允许的较长超时（深度思考）
//
// 返回：
//   - string: 模型回复
//   - error: 调用错误
//   - bool: 是否超时（兼容原接口，第三个返回值）
//
// 副作用：更新内部统计。
// 并发安全：通过 mu 锁保护。
func (t *TimeoutTracker) CallWithTimeout(
	ctx context.Context,
	llm LLMClient,
	prompt string,
	fastTimeout time.Duration,
	slowTimeout time.Duration,
) (string, error, bool) {
	// 连续超时过多，直接跳过LLM，避免雪崩
	if t.ShouldSkipLLM() {
		return "", fmt.Errorf("LLM skipped: %d consecutive timeouts", t.timeoutCount), true
	}

	// 自适应超时：之前有成功调用且不慢，允许更长超时
	timeout := fastTimeout
	if t.callCount > 0 && t.totalDur > 0 {
		// 计算历史平均耗时
		avgDur := t.totalDur / time.Duration(t.callCount)
		if avgDur < 5*time.Second && t.timeoutCount == 0 {
			// API响应快且无超时，允许深度思考
			timeout = slowTimeout
		}
	}

	// 构造带超时的子上下文
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 执行调用并计时
	start := time.Now()
	resp, err := llm.Generate(timeoutCtx, prompt)
	dur := time.Since(start)

	// 记录本次调用（含错误）
	t.RecordCall(dur, err)

	// 判断是否为超时错误
	timedOut := false
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			timedOut = true
			// 包装超时错误，附上当前尝试序号
			err = fmt.Errorf("LLM call timed out after %v (attempt #%d)", timeout, t.callCount)
		}
	}

	return resp, err, timedOut
}

// RecordCall 记录一次LLM调用。
//
// 职责：更新累计统计、连续超时计数与慢速模式标志。
// 参数：
//   - dur: 本次耗时
//   - err: 错误（nil 表示成功）
//
// 副作用：修改所有累计字段。
// 并发安全：通过 mu 写锁保护。
func (t *TimeoutTracker) RecordCall(dur time.Duration, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 更新累计统计
	t.callCount++
	t.lastDur = dur
	t.totalDur += dur

	// 更新最大耗时
	if dur > t.maxDur {
		t.maxDur = dur
	}

	// 错误处理：累加连续超时计数，达到阈值进入慢速模式
	if err != nil {
		t.timeoutCount++
		// 连续超时3次以上进入慢速模式
		if t.timeoutCount >= 3 {
			t.slowMode = true
		}
	} else {
		// 成功调用重置连续超时计数
		t.timeoutCount = 0
		t.slowMode = false
	}
}

// ShouldSkipLLM 是否应该跳过LLM调用。
//
// 返回：bool，true 表示当前处于慢速模式，调用方应跳过 LLM。
// 并发安全：读锁保护。
func (t *TimeoutTracker) ShouldSkipLLM() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.slowMode
}

// Stats 获取超时统计。
//
// 返回：
//   - callCount: 累计调用次数
//   - timeoutCount: 连续超时次数
//   - avgDur: 平均耗时
//   - maxDur: 最大耗时
//
// 并发安全：读锁保护。
func (t *TimeoutTracker) Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	avg := time.Duration(0)
	if t.callCount > 0 {
		avg = t.totalDur / time.Duration(t.callCount) // 计算平均
	}
	return t.callCount, t.timeoutCount, avg, t.maxDur
}

// StatsString 统计摘要。
//
// 返回：单行可读字符串，含调用数/超时数/平均/最大/慢速模式。
// 副作用：调用 Stats（内部加锁）。
// 并发安全：间接通过 Stats 加锁。
func (t *TimeoutTracker) StatsString() string {
	calls, timeouts, avg, max := t.Stats()
	return fmt.Sprintf("calls=%d, timeouts=%d, avg=%v, max=%v, skip=%v", calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), t.slowMode)
}
