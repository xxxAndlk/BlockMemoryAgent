package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// CallRecord 单次 LLM 调用记录
type CallRecord struct {
	Caller        string        `json:"caller"`
	PromptSummary string        `json:"prompt_summary"`
	InputTokens   int           `json:"input_tokens"`
	OutputTokens  int           `json:"output_tokens"`
	Duration      time.Duration `json:"duration"`
	Err           error         `json:"error,omitempty"`
	TimedOut      bool          `json:"timed_out"`
	Timestamp     time.Time     `json:"timestamp"`
}

// LLMCallTracker LLM调用全量追踪器（超时统计 + Token 消耗 + Prompt 记录）
type LLMCallTracker struct {
	mu           sync.RWMutex
	callCount    int
	timeoutCount int
	totalDur     time.Duration
	maxDur       time.Duration
	lastDur      time.Duration
	slowMode     bool
	records      []CallRecord // 每次调用的详细记录
}

// NewLLMCallTracker 创建调用追踪器
func NewLLMCallTracker() *LLMCallTracker {
	return &LLMCallTracker{
		records: make([]CallRecord, 0),
	}
}

// EstimateTokens 粗略估算文本的 token 数量。
// 策略：中文字符按 1 字 ≈ 1 token；ASCII 按 4 字符 ≈ 1 token。
// 这是本地调试的估算值，不追求与 OpenAI tokenizer 完全对齐。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var tokens int
	for _, r := range text {
		if r > 127 {
			// 非 ASCII（中文、 emoji 等）按 1 字 1 token
			tokens++
		} else {
			// ASCII 按 4 字符 1 token，至少 1 token
			tokens += 1
		}
	}
	// 对 ASCII 总体再除 4，避免每个字符都累加导致偏高的误差
	// 重新用更精确的算法：统计 rune 数量
	return refineEstimate(text, tokens)
}

func refineEstimate(text string, rough int) int {
	runes := utf8.RuneCountInString(text)
	asciiCount := 0
	nonAsciiCount := 0
	for _, r := range text {
		if r <= 127 {
			asciiCount++
		} else {
			nonAsciiCount++
		}
	}
	// ASCII 部分 4 字符 ≈ 1 token，非 ASCII 1 字符 ≈ 1 token
	tokens := nonAsciiCount + asciiCount/4
	if tokens < 1 && runes > 0 {
		tokens = 1
	}
	return tokens
}

// SummarizePrompt 截断 prompt 到前 N 个字符，保留首尾用于摘要展示
func SummarizePrompt(prompt string, maxLen int) string {
	if len(prompt) <= maxLen {
		return prompt
	}
	head := prompt[:maxLen/2]
	tail := prompt[len(prompt)-maxLen/4:]
	return head + "\n... (truncated " + fmt.Sprintf("%d", len(prompt)-maxLen/2-maxLen/4) + " chars) ...\n" + tail
}

// CallWithTimeout 带自适应超时的 LLM 调用（兼容原 TimeoutTracker 接口）
func (t *LLMCallTracker) CallWithTimeout(
	ctx context.Context,
	llm LLMClient,
	prompt string,
	caller string,
	fastTimeout time.Duration,
	slowTimeout time.Duration,
) (string, error, bool) {
	// 连续超时过多，直接跳过 LLM
	if t.ShouldSkipLLM() {
		return "", fmt.Errorf("LLM skipped: %d consecutive timeouts", t.timeoutCount), true
	}

	// 自适应超时
	timeout := fastTimeout
	if t.callCount > 0 && t.totalDur > 0 {
		avgDur := t.totalDur / time.Duration(t.callCount)
		if avgDur < 5*time.Second && t.timeoutCount == 0 {
			timeout = slowTimeout
		}
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	inputTokens := EstimateTokens(prompt)
	summary := SummarizePrompt(prompt, 500)

	start := time.Now()
	resp, err := llm.Generate(timeoutCtx, prompt)
	dur := time.Since(start)
	outputTokens := EstimateTokens(resp)

	timedOut := false
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			timedOut = true
			err = fmt.Errorf("LLM call timed out after %v (attempt #%d)", timeout, t.callCount+1)
		}
	}

	t.RecordCall(dur, err, caller, summary, inputTokens, outputTokens, timedOut)
	return resp, err, timedOut
}

// RecordCall 记录一次 LLM 调用（含 token 和 prompt 信息）
func (t *LLMCallTracker) RecordCall(
	dur time.Duration,
	err error,
	caller string,
	promptSummary string,
	inputTokens int,
	outputTokens int,
	timedOut bool,
) {
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
		if t.timeoutCount >= 3 {
			t.slowMode = true
		}
	} else {
		t.timeoutCount = 0
		t.slowMode = false
	}

	t.records = append(t.records, CallRecord{
		Caller:        caller,
		PromptSummary: promptSummary,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		Duration:      dur,
		Err:           err,
		TimedOut:      timedOut,
		Timestamp:     time.Now(),
	})
}

// ShouldSkipLLM 是否应该跳过 LLM 调用
func (t *LLMCallTracker) ShouldSkipLLM() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.slowMode
}

// Stats 获取超时统计（兼容原接口）
func (t *LLMCallTracker) Stats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	avg := time.Duration(0)
	if t.callCount > 0 {
		avg = t.totalDur / time.Duration(t.callCount)
	}
	return t.callCount, t.timeoutCount, avg, t.maxDur
}

// StatsString 统计摘要（兼容原接口）
func (t *LLMCallTracker) StatsString() string {
	calls, timeouts, avg, max := t.Stats()
	totalIn, totalOut := t.TokenTotals()
	return fmt.Sprintf(
		"calls=%d, timeouts=%d, avg=%v, max=%v, skip=%v, inputTokens=%d, outputTokens=%d",
		calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), t.slowMode, totalIn, totalOut,
	)
}

// TokenTotals 获取总输入/输出 token
func (t *LLMCallTracker) TokenTotals() (inputTokens, outputTokens int) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, r := range t.records {
		inputTokens += r.InputTokens
		outputTokens += r.OutputTokens
	}
	return
}

// CallerStats 按调用者聚合统计
type CallerStats struct {
	Caller       string
	CallCount    int
	TotalInput   int
	TotalOutput  int
	AvgDuration  time.Duration
	TimeoutCount int
}

// StatsByCaller 按调用者分组统计
func (t *LLMCallTracker) StatsByCaller() []CallerStats {
	t.mu.RLock()
	defer t.mu.RUnlock()

	type agg struct {
		count     int
		input     int
		output    int
		totalDur  time.Duration
		timeouts  int
	}
	m := make(map[string]*agg)
	for _, r := range t.records {
		caller := r.Caller
		if caller == "" {
			caller = "unknown"
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
			a.timeouts++
		}
	}

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

// Records 获取所有调用记录（拷贝）
func (t *LLMCallTracker) Records() []CallRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]CallRecord, len(t.records))
	copy(out, t.records)
	return out
}

// Report 生成格式化报告
func (t *LLMCallTracker) Report() string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var b strings.Builder
	b.WriteString("=== LLM Call Report ===\n")
	b.WriteString(fmt.Sprintf("Total Calls: %d | Timeouts: %d | SlowMode: %v\n", t.callCount, t.timeoutCount, t.slowMode))

	inTotal, outTotal := 0, 0
	for _, r := range t.records {
		inTotal += r.InputTokens
		outTotal += r.OutputTokens
	}
	b.WriteString(fmt.Sprintf("Total Input Tokens: %d | Total Output Tokens: %d | Total: %d\n", inTotal, outTotal, inTotal+outTotal))

	if len(t.records) > 0 {
		b.WriteString("\n--- By Caller ---\n")
		stats := t.StatsByCaller()
		for _, s := range stats {
			b.WriteString(fmt.Sprintf("  %-30s calls=%-3d in=%-6d out=%-6d avg=%v timeouts=%d\n",
				s.Caller, s.CallCount, s.TotalInput, s.TotalOutput, s.AvgDuration.Round(time.Millisecond), s.TimeoutCount))
		}
	}

	if len(t.records) > 0 {
		b.WriteString("\n--- Recent Calls ---\n")
		start := len(t.records) - 10
		if start < 0 {
			start = 0
		}
		for i, r := range t.records[start:] {
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
