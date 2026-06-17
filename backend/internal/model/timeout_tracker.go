package model

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// TimeoutTracker LLM调用超时追踪器
type TimeoutTracker struct {
	mu          sync.RWMutex
	callCount   int
	timeoutCount int
	totalDur    time.Duration
	maxDur      time.Duration
	lastDur     time.Duration
	slowMode    bool // 连续超时后进入慢速模式，跳过LLM
}

// NewTimeoutTracker 创建超时追踪器
func NewTimeoutTracker() *TimeoutTracker {
	return &TimeoutTracker{}
}

// CallWithTimeout 带自适应超时的LLM调用
// fastTimeout: 正常模式超时
// slowTimeout: 已有成功调用时允许的较长超时（深度思考）
// 返回 (response, error, timedOut)
func (t *TimeoutTracker) CallWithTimeout(
	ctx context.Context,
	llm LLMClient,
	prompt string,
	fastTimeout time.Duration,
	slowTimeout time.Duration,
) (string, error, bool) {
	// 连续超时过多，直接跳过LLM
	if t.ShouldSkipLLM() {
		return "", fmt.Errorf("LLM skipped: %d consecutive timeouts", t.timeoutCount), true
	}

	// 自适应超时：之前有成功调用且不慢，允许更长超时
	timeout := fastTimeout
	if t.callCount > 0 && t.totalDur > 0 {
		avgDur := t.totalDur / time.Duration(t.callCount)
		if avgDur < 5*time.Second && t.timeoutCount == 0 {
			// API响应快且无超时，允许深度思考
			timeout = slowTimeout
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	resp, err := llm.Generate(timeoutCtx, prompt)
	dur := time.Since(start)

	t.RecordCall(dur, err)

	timedOut := false
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			timedOut = true
			err = fmt.Errorf("LLM call timed out after %v (attempt #%d)", timeout, t.callCount)
		}
	}

	return resp, err, timedOut
}

// RecordCall 记录一次LLM调用
func (t *TimeoutTracker) RecordCall(dur time.Duration, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.callCount++
	t.lastDur = dur
	t.totalDur += dur

	if dur > t.maxDur {
		t.maxDur = dur
	}

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

// ShouldSkipLLM 是否应该跳过LLM调用
func (t *TimeoutTracker) ShouldSkipLLM() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.slowMode
}

// Stats 获取超时统计
func (t *TimeoutTracker) Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	avg := time.Duration(0)
	if t.callCount > 0 {
		avg = t.totalDur / time.Duration(t.callCount)
	}
	return t.callCount, t.timeoutCount, avg, t.maxDur
}

// StatsString 统计摘要
func (t *TimeoutTracker) StatsString() string {
	calls, timeouts, avg, max := t.Stats()
	return fmt.Sprintf("calls=%d, timeouts=%d, avg=%v, max=%v, skip=%v", calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), t.slowMode)
}
