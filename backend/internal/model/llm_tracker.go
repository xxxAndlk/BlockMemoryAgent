package model

// 本文件实现 LLMCallTracker：LLM 调用全量追踪器。
// 统一负责自适应超时、Token 统计、Prompt/Response 记录与持久化回调。

import (
	"context" // 上下文与超时控制
	"errors"  // 错误类型判断
	"fmt"     // 字符串格式化
	"strings" // caller 分层判断
	"sync"    // 读写锁保护并发访问
	"time"    // 时间统计

	"github.com/go-kratos/blades" // blades.ModelProvider / Generator 用于流式累积
)

// maxRetries 单次逻辑调用的最大尝试次数（含首次）。
// P0-1：超时/错误后重试，避免单次网络抖动导致整体失败。
const maxRetries = 3

// retryGenerate 以最多 maxRetries 次尝试调用 llm.Generate，失败后按指数退避重试。
// 复用给 CallWithTimeout 的重试逻辑与 VerifyConnectivity 的连通性探测。
//
// 参数：
//   - ctx: 上下文（提供整体取消；每 attempt 叠加 perAttemptTimeout）
//   - llm: LLM 客户端
//   - prompt: 提示词
//   - perAttemptTimeout: 单次尝试的超时
//
// 返回：
//   - string: 成功时的模型回复
//   - error: 最后一次尝试的错误（全部失败时）
//   - bool: 是否发生过超时（用于电路熔断判定）
//
// 退避策略：500ms × 2^(attempt-1)，封顶 2s；sleep 期间监听 ctx.Done() 以便及时取消。
func retryGenerate(ctx context.Context, llm LLMClient, prompt string, perAttemptTimeout time.Duration) (string, error, bool) {
	// 记录最后一次错误，用于全部失败后返回
	var lastErr error
	// 标记整个重试过程中是否出现过超时
	timedOut := false
	// 初始退避时长
	backoff := 500 * time.Millisecond
	// 循环执行最多 maxRetries 次尝试
	for attempt := 1; attempt <= maxRetries; attempt++ {
		// 整体 ctx 已取消则立即返回，不再重试
		if err := ctx.Err(); err != nil {
			return "", err, false
		}
		// 为本次 attempt 创建带单独超时的子上下文
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		// 发起 LLM 生成调用
		resp, err := llm.Generate(attemptCtx, prompt)
		// 在 cancel 前判定是否为超时（cancel 后 Err() 会变为 Canceled）
		deadlineExceeded := attemptCtx.Err() == context.DeadlineExceeded
		// 释放本次 attempt 的子上下文资源
		cancel()
		// 调用成功则直接返回结果
		if err == nil {
			return resp, nil, false
		}
		// 记录最后一次错误
		lastErr = err
		// 若本次 attempt 超时，则标记整个过程出现过超时
		if deadlineExceeded {
			timedOut = true
		}
		// 末次尝试失败不再退避，直接结束循环返回错误
		if attempt < maxRetries {
			// 等待退避时间，或整体 ctx 取消时提前退出
			select {
			case <-time.After(backoff):
				// 退避结束，继续下一次尝试
			case <-ctx.Done():
				// 上下文取消，立即返回取消错误
				return "", ctx.Err(), false
			}
			// 指数退避：每次翻倍
			backoff *= 2
			// 封顶 2 秒，避免退避过长
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
	// 全部尝试失败，返回最后一次错误与是否超时
	return "", lastErr, timedOut
}

// streamingProvider 是 BladesClient 暴露底层流式能力的接口（Provider().NewStreaming）。
type streamingProvider interface {
	NewStreaming(context.Context, *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error]
}

// retryStreamGenerate 是 retryGenerate 的流式版本（同重试/退避/超时策略）：
// 走 NewStreaming 累积完整响应，供 CallLightweightWithRetry 使用。
// 决策固化（2026-08-10 事故）：方舟 coding 端点对"可能超过 10 分钟的操作"拒绝非流式 POST，
// 轻量调用走非流式 Generate 时 5 组 exhausted retries 全挂（打捞/摘要/事实提取残废）——
// 流式是长任务端点的事实要求，轻量链路必须走流式。
// 客户端无流式实现（测试 fake 等）时回退非流式 retryGenerate（行为不变，meta 为 nil）。
// 返回语义在 retryGenerate 基础上增加第二个返回值：末次流式分块消息的 Metadata
// （provider 侧透传的 cache_hit_tokens / cache_miss_tokens 经此上传，TODO #40）。
func retryStreamGenerate(ctx context.Context, llm LLMClient, prompt string, perAttemptTimeout time.Duration) (string, map[string]any, error, bool) {
	sp, ok := llm.(streamingProvider)
	if !ok {
		text, err, timedOut := retryGenerate(ctx, llm, prompt, perAttemptTimeout)
		return text, nil, err, timedOut
	}
	var lastErr error
	timedOut := false
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", nil, err, false
		}
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		req := &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage(prompt)}}
		// 累积流式分块：取最后一个非 nil 分块（各 provider 约定末块为完整累积响应）。
		var text string
		var meta map[string]any
		lastErr = nil // 逐轮重置：首轮错误不得残留导致成功轮被跳过
		stream := sp.NewStreaming(attemptCtx, req)
		for resp, err := range stream {
			if err != nil {
				lastErr = err
				break
			}
			if resp != nil && resp.Message != nil {
				text = streamMessageText(resp.Message)
				if resp.Message.Metadata != nil {
					meta = resp.Message.Metadata
				}
			}
		}
		// 在 cancel 前判定超时（cancel 后 Err() 会变为 Canceled）。
		deadlineExceeded := attemptCtx.Err() == context.DeadlineExceeded
		cancel()
		if lastErr == nil {
			if text == "" {
				// 流式正常结束但无内容：视为空响应（与 retryGenerate 的空响应语义一致）。
				lastErr = errors.New("empty streaming response")
			} else {
				return text, meta, nil, false
			}
		}
		if deadlineExceeded {
			timedOut = true
		}
		if attempt < maxRetries {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", nil, ctx.Err(), false
			}
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
	return "", nil, lastErr, timedOut
}

// streamMessageText 提取 blades 消息的全部文本部分（忽略工具调用等非文本部分）。
func streamMessageText(m *blades.Message) string {
	var sb strings.Builder
	for _, p := range m.Parts {
		if tp, ok := p.(blades.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}

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
	Err           error         `json:"error,omitempty"` // 调用错误（成功时为 nil，序列化时省略）
	TimedOut      bool          `json:"timed_out"`       // 是否超时
	Timestamp     time.Time     `json:"timestamp"`       // 调用时间戳
}

// LayerStats 某一层模型的调用统计（P3-7：模型分层可观测性）。
type LayerStats struct {
	Layer        string `json:"layer"`         // 层名：meta / domain / lightweight / assistant / other
	Calls        int    `json:"calls"`         // 调用次数
	InputTokens  int    `json:"input_tokens"`  // 累计输入 token
	OutputTokens int    `json:"output_tokens"` // 累计输出 token
	Errors       int    `json:"errors"`        // 失败次数
}

// LLMCallTracker LLM调用全量追踪器（超时统计 + Token 消耗 + Prompt 记录 + 模型分层统计）。
// 设计意图：统一负责 LLM 调用的自适应超时、Token 统计、Prompt/Response 记录与持久化回调。
type LLMCallTracker struct {
	mu                sync.RWMutex                      // 读写锁保护所有字段
	callCount         int                               // 累计调用次数
	timeoutCount      int                               // 连续超时次数（成功时清零）
	consecutiveCancel int                               // 连续取消次数（成功时清零）
	consecutiveError  int                               // 连续其它错误次数（成功时清零）
	totalDur          time.Duration                     // 累计耗时，用于计算平均
	maxDur            time.Duration                     // 历史最大耗时
	lastDur           time.Duration                     // 最近一次耗时
	slowMode          bool                              // 连续超时后进入慢速模式，跳过 LLM
	records           []CallRecord                      // 每次调用的详细记录
	recordCallback    func(context.Context, CallRecord) // 可选：每次记录后的回调（用于写入 session_logs）
	layerStats        map[string]*LayerStats            // 按模型层聚合的统计（P3-7）
}

// NewLLMCallTracker 创建一个新的 LLM 调用追踪器。
//
// 返回：*LLMCallTracker（records 初始化为空切片）。
// 副作用：无。
// 并发安全：返回实例可被多协程共享。
func NewLLMCallTracker() *LLMCallTracker {
	// 初始化追踪器，预分配空切片与 map 避免 nil 指针问题
	return &LLMCallTracker{
		records:    make([]CallRecord, 0), // 预分配空切片避免 nil
		layerStats: make(map[string]*LayerStats),
	}
}

// SetRecordCallback 设置每次 LLM 调用记录后的回调函数。
// 回调接收 context 与 CallRecord 副本，典型用途是写入 session_logs 表。
func (t *LLMCallTracker) SetRecordCallback(cb func(context.Context, CallRecord)) {
	// 加写锁保护 recordCallback 字段
	t.mu.Lock()
	// 退出时释放写锁
	defer t.mu.Unlock()
	// 保存回调
	t.recordCallback = cb
}

// CallWithTimeout 带自适应超时的 LLM 调用。
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
		// 返回错误并标记 timedOut=true，提示调用方本次未真正调用
		return "", fmt.Errorf("LLM skipped: %d consecutive timeouts", t.timeoutCount), true
	}

	// 自适应超时：默认使用 fastTimeout
	timeout := fastTimeout
	// 若已有成功调用记录，则根据历史平均耗时决定是否使用更长的 slowTimeout
	if t.callCount > 0 && t.totalDur > 0 {
		// 计算历史平均耗时
		avgDur := t.totalDur / time.Duration(t.callCount)
		// 平均耗时低且无超时记录时，允许更长的深度思考超时
		if avgDur < 5*time.Second && t.timeoutCount == 0 {
			timeout = slowTimeout
		}
	}

	// 预先估算输入 token 与摘要（调用前 prompt 已知）
	inputTokens := EstimateTokens(prompt)
	summary := SummarizePrompt(prompt, 500)

	// 已取消的 ctx 不重试，直接记录并返回
	if err := ctx.Err(); err != nil {
		// 区分 deadline exceeded 与主动取消，便于日志定位
		timedOut := errors.Is(err, context.DeadlineExceeded)
		// 记录此次失败的调用
		t.RecordCall(ctx, 0, err, caller, summary, prompt, "", inputTokens, 0, timedOut)
		return "", err, timedOut
	}

	// P0-1：带重试的调用（最多 maxRetries 次，指数退避）
	// 单次逻辑调用 → 单次 RecordCall，保留现有回调契约与电路熔断语义。
	start := time.Now()
	resp, err, retryTimedOut := retryGenerate(ctx, llm, prompt, timeout)
	// 计算实际耗时
	dur := time.Since(start)
	// 估算输出 token
	outputTokens := EstimateTokens(resp)

	// 判断是否为超时：重试期间任意 attempt 超时，或整体 ctx 超时
	timedOut := retryTimedOut || ctx.Err() == context.DeadlineExceeded
	// 出错且存在超时时，包装错误信息便于排查
	if err != nil && timedOut {
		err = fmt.Errorf("LLM call timed out after %v per attempt (%d retries): %w", timeout, maxRetries, err)
	}

	// 记录本次逻辑调用（含 token/摘要/完整 prompt/response）
	t.RecordCall(ctx, dur, err, caller, summary, prompt, resp, inputTokens, outputTokens, timedOut)
	// 返回模型回复、错误与超时标记
	return resp, err, timedOut
}

// RecordCall 记录一次 LLM 调用（含 token 和 prompt 信息）。
//
// 职责：更新累计统计、连续超时计数、慢速模式标志，并追加 CallRecord。
// 参数：
//   - ctx: 上下文（透传给回调）
//   - dur: 本次耗时
//   - err: 错误（nil 表示成功）
//   - caller: 调用方标识
//   - promptSummary: prompt 摘要
//   - prompt: 完整 prompt
//   - response: 完整 response
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
	// 加写锁保护所有内部字段
	t.mu.Lock()
	// 函数退出时释放写锁
	defer t.mu.Unlock()

	// 更新累计调用次数
	t.callCount++
	// 记录最近一次耗时
	t.lastDur = dur
	// 累加总耗时
	t.totalDur += dur

	// 更新历史最大耗时
	if dur > t.maxDur {
		t.maxDur = dur
	}

	// 错误分类处理：仅 context.DeadlineExceeded 计入连续超时并触发慢速模式；
	// context.Canceled 与其它错误分别计数，不触发慢速模式。
	if err != nil {
		// 根据错误类型进入不同分支
		switch {
		case errors.Is(err, context.Canceled):
			// 取消错误：增加连续取消计数，重置超时与其他错误计数
			t.consecutiveCancel++
			t.timeoutCount = 0
			t.consecutiveError = 0
		case errors.Is(err, context.DeadlineExceeded):
			// 超时错误：增加连续超时计数，重置取消与其他错误计数
			t.timeoutCount++
			t.consecutiveCancel = 0
			t.consecutiveError = 0
			// 连续 3 次超时进入慢速模式
			if t.timeoutCount >= 3 {
				t.slowMode = true
			}
		default:
			// 其他通用错误：增加连续错误计数，重置取消与超时计数
			t.consecutiveError++
			t.consecutiveCancel = 0
			t.timeoutCount = 0
		}
	} else {
		// 成功调用：重置所有连续计数与慢速模式
		t.timeoutCount = 0
		t.consecutiveCancel = 0
		t.consecutiveError = 0
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
	// 将记录追加到切片尾部
	t.records = append(t.records, rec)

	// P3-7：按模型层聚合统计
	layer := CallerToLayer(caller)
	// 从 map 中查找该层统计对象
	ls, ok := t.layerStats[layer]
	// 若不存在则新建并放入 map
	if !ok {
		ls = &LayerStats{Layer: layer}
		t.layerStats[layer] = ls
	}
	// 更新该层统计
	ls.Calls++
	ls.InputTokens += inputTokens
	ls.OutputTokens += outputTokens
	// 若本次调用失败，则错误计数加一
	if err != nil {
		ls.Errors++
	}

	// 触发持久化回调（若已设置），不阻塞、不处理错误
	if t.recordCallback != nil {
		// 先取出回调指针，避免在 goroutine 执行期间被修改导致异常
		cb := t.recordCallback
		// 异步执行回调，避免阻塞 RecordCall 主流程
		go cb(ctx, rec)
	}
}

// ShouldSkipLLM 返回当前是否应该跳过 LLM 调用。
//
// 返回：bool，true 表示当前处于慢速模式，调用方应跳过 LLM。
// 并发安全：读锁保护。
func (t *LLMCallTracker) ShouldSkipLLM() bool {
	// 加读锁保护 slowMode 字段
	t.mu.RLock()
	// 退出时释放读锁
	defer t.mu.RUnlock()
	// 返回慢速模式标志
	return t.slowMode
}

// CallerToLayer 把调用方标识映射到模型分层（P3-7）。
//
// 规则：
//   - 包含 "MetaAgent" 或 caller="meta" → meta
//   - 包含 "DomainAgent" 或 caller="domain" → domain
//   - 包含 "Lightweight" 或 caller="lightweight" → lightweight
//   - 包含 "助手" 或 "Assistant" → assistant
//   - 其他 → other
//
// 参数：
//   - caller: 调用方标识字符串。
//
// 返回：对应的层名字符串。
func CallerToLayer(caller string) string {
	// 空字符串归为 other
	if caller == "" {
		return "other"
	}
	// 转小写后做不区分大小写的匹配
	lower := strings.ToLower(caller)
	// 按优先级依次匹配
	switch {
	case lower == "meta" || strings.Contains(lower, "metaagent"):
		return "meta"
	case lower == "domain" || strings.Contains(lower, "domainagent") || strings.Contains(lower, "subdomain"):
		return "domain"
	case lower == "lightweight" || strings.Contains(lower, "lightweight"):
		return "lightweight"
	case strings.Contains(lower, "助手") || strings.Contains(lower, "assistant"):
		return "assistant"
	default:
		return "other"
	}
}

// LayerStatsSnapshot 返回按模型层聚合的调用统计快照（P3-7）。
//
// 返回：[]LayerStats 副本。
// 并发安全：读锁保护；返回副本，避免外部修改内部状态。
func (t *LLMCallTracker) LayerStatsSnapshot() []LayerStats {
	// 加读锁保护 layerStats map
	t.mu.RLock()
	// 退出时释放读锁
	defer t.mu.RUnlock()
	// 预分配与 map 等长的切片
	out := make([]LayerStats, 0, len(t.layerStats))
	// 遍历 map，将每个 LayerStats 值拷贝到切片
	for _, ls := range t.layerStats {
		out = append(out, *ls)
	}
	// 返回聚合统计快照
	return out
}
