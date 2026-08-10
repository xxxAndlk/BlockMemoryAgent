package middleware

// llm.go 实现 LLM 链（TODO #19 Phase 1 起点）：
// RetryLLM + CallLLM 迁移自 ReActAgent.generate 的重试/超时/审计逻辑，
// 行为保持不变（4xx 快速失败、DeadlineExceeded 不重试、指数退避、空响应不重试）。

import (
	"context"
	"errors"
	"time"

	"github.com/go-kratos/blades"
)

// LLMCall 是 LLM 链的终端调用（真正发往模型的单次调用）。
type LLMCall func(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error)

// LLMCtx 是 LLM 链的请求上下文（由 ReActAgent.generate 构造）。
// Resp 由终端 CallLLM 填充；RetryLLM 在重试前清空。
type LLMCtx struct {
	Request *blades.ModelRequest // 模型请求
	Resp    *blades.ModelResponse // 终端调用结果
	Call    LLMCall              // 终端调用（CallLLM 注入，可被测试替换）
}

// shouldRetryLLM 判定 LLM 调用错误是否值得重试：
//   - 会话取消（ctx.Err() 非空）不重试（用户/上层主动行为）；
//   - 单次调用超时（DeadlineExceeded）不重试（慢推理模型重试只会重复超时，
//     实证：glm-5.2 thinking 180s ×4 = 12min "重复思考不前进"）；
//   - 其余瞬时错误重试。
func shouldRetryLLM(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.DeadlineExceeded)
}

// RetryLLM 是 LLM 调用重试中间件：最多 retries 次重试（不含首次），
// 每次重试前指数退避（初始 backoff 翻倍）。next 失败且 shouldRetry 判定可重试时继续。
func RetryLLM(retries int, backoff time.Duration, shouldRetry func(ctx context.Context, err error) bool) Middleware[LLMCtx] {
	if shouldRetry == nil {
		shouldRetry = shouldRetryLLM
	}
	return func(next Handler[LLMCtx]) Handler[LLMCtx] {
		return func(ctx context.Context, c *LLMCtx) error {
			attempts := retries + 1
			if attempts < 1 {
				attempts = 1
			}
			b := backoff
			if b <= 0 {
				b = 100 * time.Millisecond
			}
			var lastErr error
			for i := 0; i < attempts; i++ {
				if i > 0 {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(b):
					}
					b *= 2
				}
				c.Resp = nil
				if err := next(ctx, c); err != nil {
					lastErr = err
					if !shouldRetry(ctx, err) {
						return err
					}
					continue
				}
				// 空响应属模型异常，不重试（重试大概率同样为空）。
				if c.Resp == nil || c.Resp.Message == nil {
					return errors.New("llm returned empty response")
				}
				return nil
			}
			return lastErr
		}
	}
}

// CallLLM 是 LLM 链的调用层中间件：为 next（终端调用）附加单次调用超时。
// llmTimeout<=0 时不设超时（仅受会话 ctx 取消控制）。
func CallLLM(llmTimeout time.Duration) Middleware[LLMCtx] {
	return func(next Handler[LLMCtx]) Handler[LLMCtx] {
		return func(ctx context.Context, c *LLMCtx) error {
			callCtx := ctx
			cancel := func() {}
			if llmTimeout > 0 {
				callCtx, cancel = context.WithTimeout(ctx, llmTimeout)
			}
			defer cancel()
			return next(callCtx, c)
		}
	}
}

// TerminalCall 是 LLM 链终端 handler：调用 c.Call（真正的模型调用）并把结果写入 c.Resp。
// 与 RetryLLM/CallLLM 组合：RetryLLM(CallLLM(TerminalCall))。
func TerminalCall(ctx context.Context, c *LLMCtx) error {
	resp, err := c.Call(ctx, c.Request)
	if err != nil {
		return err
	}
	c.Resp = resp
	return nil
}
