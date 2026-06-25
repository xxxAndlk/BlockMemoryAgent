package model

// 本文件实现 LLMCallTracker：LLM 调用全量追踪器。
// 在自适应超时基础上扩展 Token 统计、Prompt 摘要、按调用者聚合等能力，
// 兼容原 TimeoutTracker 接口（CallWithTimeout/Stats/StatsString）。

import (
	"context"      // 上下文与超时
	"fmt"          // 字符串格式化
	"strings"      // 字符串拼接（Report）
	"sync"         // 读写锁保护并发访问
	"time"         // 时间统计
	"unicode/utf8" // rune 计数，用于 token 估算
)

// CallRecord 单次 LLM 调用记录。
// 设计意图：持久化每次调用的元信息，供后续审计/调试/聚合分析。
type CallRecord struct {
	Caller        string        `json:"caller"`          // 调用方标识（如 MetaAgent/DomainAgent 节点名）
	PromptSummary string        `json:"prompt_summary"`  // 截断后的 prompt 摘要
	InputTokens   int           `json:"input_tokens"`    // 估算的输入 token 数
	OutputTokens  int           `json:"output_tokens"`   // 估算的输出 token 数
	Duration      time.Duration `json:"duration"`        // 调用耗时
	Err           error         `json:"error,omitempty"` // 调用错误（成功时为 nil，省略序列化）
	TimedOut      bool          `json:"timed_out"`       // 是否超时
	Timestamp     time.Time     `json:"timestamp"`       // 调用时间戳
}

// LLMCallTracker LLM调用全量追踪器（超时统计 + Token 消耗 + Prompt 记录）。
// 设计意图：在自适应超时（TimeoutTracker）基础上，叠加 token 与 prompt 维度，
// 用于成本观测与异常定位。
type LLMCallTracker struct {
	mu           sync.RWMutex  // 读写锁保护所有字段
	callCount    int           // 累计调用次数
	timeoutCount int           // 连续超时次数（成功时清零）
	totalDur     time.Duration // 累计耗时，用于计算平均
	maxDur       time.Duration // 历史最大耗时
	lastDur      time.Duration // 最近一次耗时
	slowMode     bool          // 连续超时后进入慢速模式，跳过 LLM
	records      []CallRecord  // 每次调用的详细记录
}

// NewLLMCallTracker 创建调用追踪器。
//
// 返回：*LLMCallTracker（records 初始化为空切片）。
// 副作用：无。
// 并发安全：返回实例可被多协程共享。
func NewLLMCallTracker() *LLMCallTracker {
	return &LLMCallTracker{
		records: make([]CallRecord, 0), // 预分配空切片避免 nil
	}
}

// EstimateTokens 粗略估算文本的 token 数量。
// 策略：中文字符按 1 字 ≈ 1 token；ASCII 按 4 字符 ≈ 1 token。
// 这是本地调试的估算值，不追求与 OpenAI tokenizer 完全对齐。
//
// 参数：
//   - text: 待估算文本
//
// 返回：
//   - int: 估算 token 数（至少 0；非空文本至少 1）
//
// 副作用：无。
// 并发安全：纯函数。
func EstimateTokens(text string) int {
	if text == "" {
		return 0 // 空文本直接返回 0
	}
	var tokens int
	// 第一遍粗估：遍历 rune，非 ASCII 计 1，ASCII 也计 1（后续会被 refine 修正）
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

// refineEstimate 用更精确的算法重算 token 数。
// 参数：
//   - text: 原始文本
//   - rough: 第一遍粗估结果（当前未直接使用，保留参数以兼容旧签名）
//
// 返回：
//   - int: 修正后的 token 数
//
// 副作用：无。
func refineEstimate(text string, rough int) int {
	runes := utf8.RuneCountInString(text) // rune 总数
	asciiCount := 0                       // ASCII 字符数
	nonAsciiCount := 0                    // 非 ASCII 字符数
	for _, r := range text {
		if r <= 127 {
			asciiCount++
		} else {
			nonAsciiCount++
		}
	}
	// ASCII 部分 4 字符 ≈ 1 token，非 ASCII 1 字符 ≈ 1 token
	tokens := nonAsciiCount + asciiCount/4
	// 至少返回 1（若文本非空）
	if tokens < 1 && runes > 0 {
		tokens = 1
	}
	return tokens
}

// SummarizePrompt 截断 prompt 到前 N 个字符，保留首尾用于摘要展示。
//
// 设计意图：完整 prompt 可能极长，摘要需在有限篇幅内保留首尾关键信息。
// 参数：
//   - prompt: 原始提示词
//   - maxLen: 摘要最大长度（首部 maxLen/2，尾部 maxLen/4）
//
// 返回：
//   - string: 截断后的摘要（含 "... (truncated N chars) ..." 标记）
//
// 副作用：无。
func SummarizePrompt(prompt string, maxLen int) string {
	// 短 prompt 直接返回
	if len(prompt) <= maxLen {
		return prompt
	}
	// 取首部一半长度
	head := prompt[:maxLen/2]
	// 取尾部四分之一长度
	tail := prompt[len(prompt)-maxLen/4:]
	// 拼接首尾并标注被截断的字符数
	return head + "\n... (truncated " + fmt.Sprintf("%d", len(prompt)-maxLen/2-maxLen/4) + " chars) ...\n" + tail
}

// CallWithTimeout 带自适应超时的 LLM 调用（兼容原 TimeoutTracker 接口）。
//
// 职责：
//   - 判断是否进入慢速模式（连续超时）→ 直接跳过 LLM
//   - 根据历史平均耗时自适应选择 fast/slow 超时
//   - 执行调用并记录（含 token/prompt 信息）
//
// 参数：
//   - ctx: 上下文
//   - llm: LLM 客户端
//   - prompt: 提示词
//   - caller: 调用方标识
//   - fastTimeout: 正常模式超时
//   - slowTimeout: 已有成功调用时允许的较长超时（深度思考）
//
// 返回：
//   - string: 模型回复
//   - error: 调用错误
//   - bool: 是否超时
//
// 副作用：更新内部统计与 records。
// 并发安全：通过 mu 锁保护。
func (t *LLMCallTracker) CallWithTimeout(
	ctx context.Context,
	llm LLMClient,
	prompt string,
	caller string,
	fastTimeout time.Duration,
	slowTimeout time.Duration,
) (string, error, bool) {
	// 连续超时过多，直接跳过 LLM，避免雪崩
	if t.ShouldSkipLLM() {
		return "", fmt.Errorf("LLM skipped: %d consecutive timeouts", t.timeoutCount), true
	}

	// 自适应超时：默认 fast，历史表现好则升级到 slow
	timeout := fastTimeout
	if t.callCount > 0 && t.totalDur > 0 {
		// 计算历史平均耗时
		avgDur := t.totalDur / time.Duration(t.callCount)
		if avgDur < 5*time.Second && t.timeoutCount == 0 {
			// 平均耗时低且无超时，允许更长的深度思考超时
			timeout = slowTimeout
		}
	}

	// 构造带超时的子上下文
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 预先估算输入 token 与摘要（调用前 prompt 已知）
	inputTokens := EstimateTokens(prompt)
	summary := SummarizePrompt(prompt, 500)

	// 执行调用并计时
	start := time.Now()
	resp, err := llm.Generate(timeoutCtx, prompt)
	dur := time.Since(start)
	// 估算输出 token
	outputTokens := EstimateTokens(resp)

	// 判断是否为超时错误
	timedOut := false
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			timedOut = true
			// 包装超时错误，附上当前尝试序号
			err = fmt.Errorf("LLM call timed out after %v (attempt #%d)", timeout, t.callCount+1)
		}
	}

	// 记录本次调用（含 token/摘要）
	t.RecordCall(dur, err, caller, summary, inputTokens, outputTokens, timedOut)
	return resp, err, timedOut
}

// RecordCall 记录一次 LLM 调用（含 token 和 prompt 信息）。
//
// 职责：更新累计统计、连续超时计数、慢速模式标志，并追加 CallRecord。
// 参数：
//   - dur: 本次耗时
//   - err: 错误（nil 表示成功）
//   - caller: 调用方标识
//   - promptSummary: prompt 摘要
//   - inputTokens: 输入 token 数
//   - outputTokens: 输出 token 数
//   - timedOut: 是否超时
//
// 副作用：修改所有累计字段，追加 records。
// 并发安全：通过 mu 写锁保护。
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
		if t.timeoutCount >= 3 {
			t.slowMode = true // 连续 3 次失败进入慢速模式
		}
	} else {
		// 成功调用重置连续超时计数与慢速模式
		t.timeoutCount = 0
		t.slowMode = false
	}

	// 追加详细记录
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

// ShouldSkipLLM 是否应该跳过 LLM 调用。
//
// 返回：bool，true 表示当前处于慢速模式，调用方应跳过 LLM。
// 并发安全：读锁保护。
func (t *LLMCallTracker) ShouldSkipLLM() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.slowMode
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
// 返回：string，多行报告。
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
