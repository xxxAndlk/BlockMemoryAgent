package model

import (
	"fmt"
	"strings"
	"time"
)

// CallerStats 按调用者聚合统计。
// 设计意图：用于报表中按调用方维度展示 LLM 消耗。
type CallerStats struct {
	Caller       string        // 调用方标识
	CallCount    int           // 该调用方的调用次数
	TotalInput   int           // 该调用方总输入 token
	TotalOutput  int           // 该调用方总输出 token
	AvgDuration  time.Duration // 该调用方平均耗时
	TimeoutCount int           // 该调用方超时/错误次数
}

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

// StatsByCaller 按调用者分组统计。
//
// 职责：遍历 records，按 Caller 字段聚合调用数/token/耗时/超时数。
// 返回：
//   - []CallerStats: 各调用方的聚合结果（无 caller 的归入 "unknown"）
//
// 并发安全：读锁保护。
func (t *LLMCallTracker) StatsByCaller() []CallerStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// 内部聚合结构
	type agg struct {
		count    int
		input    int
		output   int
		totalDur time.Duration
		timeouts int
	}
	m := make(map[string]*agg)
	// 遍历记录聚合
	for _, r := range t.records {
		caller := r.Caller
		if caller == "" {
			caller = "unknown" // 缺失 caller 归入 unknown
		}
		if m[caller] == nil {
			m[caller] = &agg{}
		}
		a := m[caller]
		a.count++
		a.input += r.InputTokens
		a.output += r.OutputTokens
		a.totalDur += r.Duration
		if r.TimedOut || r.Err != nil {
			a.timeouts++ // 超时或错误都计入
		}
	}

	// 把聚合结果转为 CallerStats 切片
	var results []CallerStats
	for caller, a := range m {
		avg := time.Duration(0)
		if a.count > 0 {
			avg = a.totalDur / time.Duration(a.count)
		}
		results = append(results, CallerStats{
			Caller:       caller,
			CallCount:    a.count,
			TotalInput:   a.input,
			TotalOutput:  a.output,
			AvgDuration:  avg,
			TimeoutCount: a.timeouts,
		})
	}
	return results
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

// Report 生成格式化报告。
//
// 职责：汇总总量、按调用者分组、最近 10 条调用，输出可读文本。
// 副作用：调用 StatsByCaller（内部加锁）。
// 并发安全：本方法加读锁；注意 StatsByCaller 会再次加锁（无死锁，因非递归调用是顺序的）。
func (t *LLMCallTracker) Report() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var b strings.Builder
	// 报告头
	b.WriteString("=== LLM Call Report ===\n")
	b.WriteString(fmt.Sprintf("Total Calls: %d | Timeouts: %d | SlowMode: %v\n", t.callCount, t.timeoutCount, t.slowMode))

	// 统计总 token
	inTotal, outTotal := 0, 0
	for _, r := range t.records {
		inTotal += r.InputTokens
		outTotal += r.OutputTokens
	}
	b.WriteString(fmt.Sprintf("Total Input Tokens: %d | Total Output Tokens: %d | Total: %d\n", inTotal, outTotal, inTotal+outTotal))

	// 按调用者分组输出
	if len(t.records) > 0 {
		b.WriteString("\n--- By Caller ---\n")
		stats := t.StatsByCaller()
		for _, s := range stats {
			b.WriteString(fmt.Sprintf("  %-30s calls=%-3d in=%-6d out=%-6d avg=%v timeouts=%d\n",
				s.Caller, s.CallCount, s.TotalInput, s.TotalOutput, s.AvgDuration.Round(time.Millisecond), s.TimeoutCount))
		}
	}

	// 最近 10 条调用
	if len(t.records) > 0 {
		b.WriteString("\n--- Recent Calls ---\n")
		// 取最后 10 条的起始下标
		start := len(t.records) - 10
		if start < 0 {
			start = 0
		}
		for i, r := range t.records[start:] {
			// 状态标记
			status := "OK"
			if r.TimedOut {
				status = "TIMEOUT"
			} else if r.Err != nil {
				status = "ERROR"
			}
			b.WriteString(fmt.Sprintf("  #%d [%s] caller=%s in=%d out=%d dur=%v status=%s\n",
				start+i+1, r.Timestamp.Format("15:04:05"), r.Caller, r.InputTokens, r.OutputTokens, r.Duration.Round(time.Millisecond), status))
		}
	}

	return b.String()
}
