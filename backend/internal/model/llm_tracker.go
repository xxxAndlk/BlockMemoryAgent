package model

// 本文件实现 LLMCallTracker：LLM 调用全量追踪器。
// 在自适应超时基础上扩展 Token 统计、Prompt 摘要、按调用者聚合等能力，
// 兼容原 TimeoutTracker 接口（CallWithTimeout/Stats/StatsString）。

import (
	"context" // 上下文与超时
	"fmt"     // 字符串格式化
	"sync"    // 读写锁保护并发访问
	"time"    // 时间统计
)

// CallRecord 单次 LLM 调用记录。
// 设计意图：持久化每次调用的元信息，供后续审计/调试/聚合分析。
type CallRecord struct {
	Caller        string        `json:"caller"`          // 调用方标识（如 MetaAgent/DomainAgent 节点名）
	PromptSummary string        `json:"prompt_summary"`  // 截断后的 prompt 摘要
	Prompt        string        `json:"prompt"`          // 完整 prompt（用于 session_logs 持久化）
	Response      string        `json:"response"`        // 完整 response（用于 session_logs 持久化）
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
	mu             sync.RWMutex                      // 读写锁保护所有字段
	callCount      int                               // 累计调用次数
	timeoutCount   int                               // 连续超时次数（成功时清零）
	totalDur       time.Duration                     // 累计耗时，用于计算平均
	maxDur         time.Duration                     // 历史最大耗时
	lastDur        time.Duration                     // 最近一次耗时
	slowMode       bool                              // 连续超时后进入慢速模式，跳过 LLM
	records        []CallRecord                      // 每次调用的详细记录
	recordCallback func(context.Context, CallRecord) // 可选：每次记录后的回调（用于写入 session_logs）
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

// SetRecordCallback 设置每次 LLM 调用记录后的回调。
// 回调接收 context 与 CallRecord 副本，典型用途是写入 session_logs 表。
func (t *LLMCallTracker) SetRecordCallback(cb func(context.Context, CallRecord)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordCallback = cb
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

	// 记录本次调用（含 token/摘要/完整 prompt/response）
	t.RecordCall(ctx, dur, err, caller, summary, prompt, resp, inputTokens, outputTokens, timedOut)
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
	ctx context.Context,
	dur time.Duration,
	err error,
	caller string,
	promptSummary string,
	prompt string,
	response string,
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
	rec := CallRecord{
		Caller:        caller,
		PromptSummary: promptSummary,
		Prompt:        prompt,
		Response:      response,
		InputTokens:   inputTokens,
		OutputTokens:  outputTokens,
		Duration:      dur,
		Err:           err,
		TimedOut:      timedOut,
		Timestamp:     time.Now(),
	}
	t.records = append(t.records, rec)

	// 触发持久化回调（若已设置），不阻塞、不处理错误
	if t.recordCallback != nil {
		cb := t.recordCallback
		go cb(ctx, rec)
	}
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
