package model

// retry_provider.go 在 provider 层包装 blades.ModelProvider，对 Generate 调用做 3 次重试。
// 可重试状态（超时、网络错误、空响应、5xx、429）触发重试，3 次后仍失败返回错误给上级；
// 4xx 客户端错误（400/401/403/404/422，请求本身非法）不重试，快速失败避免白等；
// 例外：provider 实现 maxTokensClamper 且自愈成功（如 max_tokens 超限被钳制）时继续重试。
// ctx 主动取消（context.Canceled）不重试，直接返回，避免用户取消后继续烧 token。
// NewStreaming 直接委托底层，仅首错（未产出任何增量时）重试；
// react_agent.generate 在更高层已有重试，覆盖流式路径。

import (
	"context" // 上下文传递与取消判定
	"errors"  // errors.Is 判定取消类型
	"fmt"     // 错误格式化
	"log"     // 记录每次重试与最终失败，便于排查
	"strings" // 4xx 错误标记匹配
	"time"    // 退避与超时

	"github.com/go-kratos/blades" // blades.ModelProvider 与 ModelRequest/Response
)

// providerMaxRetries 是单次逻辑调用的最大尝试次数（含首次）。
// 3 次 = 首次 + 2 次重试，覆盖瞬时网络抖动与 429/503 等可重试状态。
const providerMaxRetries = 3

// providerRetryInitialBackoff 是首次重试前的退避时长，每次翻倍，封顶 2s。
// 500ms 起步覆盖大多数 API 限速的冷却窗口；2s 封顶避免长延迟拖垮主循环。
// 用 var 而非 const 便于测试覆盖为短时长，避免测试等待 500ms。
var providerRetryInitialBackoff = 500 * time.Millisecond

// providerRetryMaxBackoff 是退避时长上限，避免指数退避无限增长。
const providerRetryMaxBackoff = 2 * time.Second

// nonRetryableStatusMarkers 是不可重试的 HTTP 状态码片段（如 " 400 "）。
// 4xx 客户端错误（请求格式/鉴权/参数）由请求本身决定，重试必然同样失败：
// 实证 domain 的 400 配对错误被完整重发 3 次，白等 3 倍拒绝耗时。
// 408（超时）与 429（限速）可重试，不在列表内。
var nonRetryableStatusMarkers = []string{
	" 400 ", " 401 ", " 403 ", " 404 ", " 422 ",
	"invalid_request_error", "authentication_error", "permission_error", "not_found_error",
}

// isNonRetryableErr 判定错误是否为不可重试的客户端错误（4xx）。
func isNonRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	// 模型不支持图片输入：环境性能力缺失，重试必然复现，且文案可能不含 " 400 "
	// 片段（openai-chat 的 "openai-chat 400: %s" 无尾随空格）。
	if IsImageInputUnsupported(err) {
		return true
	}
	msg := err.Error()
	for _, marker := range nonRetryableStatusMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// maxTokensClamper 由 provider 选择性实现：检测到 max_tokens 超限错误时把自身
// 输出上限钳制到端点声明的值并返回 true。retryProvider 据此把原本不可重试的 4xx
// 视为可重试一次——请求参数已被修正，重试不再必然失败（如 ark 端点对 kimi 系
// 硬上限 32768，配 65536 时全部 400；钳制后重试即通过）。
type maxTokensClamper interface {
	ClampMaxTokensOnError(err error) bool
}

// trySelfHeal 若底层 provider 支持自愈且本次错误可修复，返回 true（已修复，可重试）。
func (p *retryProvider) trySelfHeal(err error) bool {
	if err == nil {
		return false
	}
	c, ok := p.inner.(maxTokensClamper)
	return ok && c.ClampMaxTokensOnError(err)
}

// retryProvider 包装 blades.ModelProvider，对 Generate 做 3 次重试。
// NewStreaming 直接委托底层，保留流式语义与 react_agent 层的重试覆盖。
type retryProvider struct {
	inner blades.ModelProvider // 底层真实 provider（anthropic/openai-chat/openai-responses/ollama）
	name  string               // provider 名称，用于日志定位
}

// Name 返回底层 provider 的名称。
func (p *retryProvider) Name() string {
	if p == nil || p.inner == nil {
		return ""
	}
	return p.inner.Name()
}

// ModelName 返回模型名（构造时传入的 cfg.Model，见 wrapWithRetry）。
// 实现 react_agent.llmModelName 期望的 interface{ ModelName() string }：
// ReActAgent.llm 持有的是本包装器（经 GetBladesProvider 透出），此前无此方法导致
// session_logs.model 与 [react] llm start 日志的 model= 全空，无法按模型聚合耗时。
func (p *retryProvider) ModelName() string {
	if p == nil {
		return ""
	}
	return p.name
}

// Generate 调用底层 provider.Generate，非成功状态重试最多 providerMaxRetries 次。
// 成功条件：err == nil && resp != nil && resp.Message != nil。
// ctx.Canceled 不重试（用户主动取消）；ctx.DeadlineExceeded 视为超时，重试。
// 退避：500ms × 2^(attempt-1)，封顶 2s；sleep 期间监听 ctx.Done() 以便及时取消。
// 全部失败后返回最后一次错误，上层（react_agent.generate / retryGenerate）可见并决定是否再重试。
func (p *retryProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	var lastErr error
	backoff := providerRetryInitialBackoff
	for attempt := 1; attempt <= providerMaxRetries; attempt++ {
		// 整体 ctx 已取消则立即返回，不再重试
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp, err := p.inner.Generate(ctx, req)
		// 成功：err=nil 且响应非空（含 Message）。空响应视为异常，继续重试。
		if err == nil && resp != nil && resp.Message != nil {
			return resp, nil
		}
		// 记录最后一次错误；err==nil 但空响应时构造明确错误信息便于排查。
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("empty model response from provider %s", p.name)
		}
		// 用户主动取消不重试，直接返回。
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 4xx 客户端错误由请求本身决定，重试必然同样失败，快速返回。
		// 例外：provider 自愈成功（如 max_tokens 钳制）后参数已修正，继续重试。
		if isNonRetryableErr(lastErr) {
			if p.trySelfHeal(lastErr) {
				log.Printf("[model] Generate self-healed: provider=%s attempt=%d err=%v", p.name, attempt, lastErr)
			} else {
				log.Printf("[model] Generate non-retryable: provider=%s attempt=%d err=%v", p.name, attempt, lastErr)
				return nil, lastErr
			}
		}
		// 末次尝试失败不再退避，直接结束循环返回错误。
		if attempt < providerMaxRetries {
			log.Printf("[model] Generate retry: provider=%s attempt=%d/%d backoff=%v err=%v",
				p.name, attempt, providerMaxRetries, backoff, lastErr)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			backoff *= 2
			if backoff > providerRetryMaxBackoff {
				backoff = providerRetryMaxBackoff
			}
		}
	}
	log.Printf("[model] Generate exhausted retries: provider=%s attempts=%d last_err=%v",
		p.name, providerMaxRetries, lastErr)
	return nil, fmt.Errorf("provider %s generate failed after %d retries: %w",
		p.name, providerMaxRetries, lastErr)
}

// NewStreaming 委托底层 provider 的流式实现，并在首错时重试。
// 仅在尚未 yield 任何增量时重试（增量已发出则无法撤回，避免重复输出）。
// ctx 主动取消不重试；超时/网络错误/5xx 重试最多 providerMaxRetries 次。
func (p *retryProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	type streamingProvider interface {
		NewStreaming(context.Context, *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error]
	}
	sp, ok := p.inner.(streamingProvider)
	if !ok {
		return func(yield func(*blades.ModelResponse, error) bool) {
			yield(nil, fmt.Errorf("provider %s does not support streaming", p.name))
		}
	}
	return func(yield func(*blades.ModelResponse, error) bool) {
		backoff := providerRetryInitialBackoff
		for attempt := 1; attempt <= providerMaxRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			started := false // 是否已 yield 增量；true 后不再重试
			stream := sp.NewStreaming(ctx, req)
			var finalErr error
			for resp, err := range stream {
				if err != nil {
					finalErr = err
					break
				}
				if resp != nil {
					started = true
					if !yield(resp, nil) {
						return
					}
				}
			}
			if finalErr == nil {
				return
			}
			// 已发增量则不重试（撤回不了），直接透传错误
			if started {
				yield(nil, finalErr)
				return
			}
			// 用户主动取消不重试
			if errors.Is(finalErr, context.Canceled) || ctx.Err() != nil {
				yield(nil, finalErr)
				return
			}
			// 4xx 客户端错误由请求本身决定，重试必然同样失败，快速返回。
			// 例外：provider 自愈成功（如 max_tokens 钳制）后参数已修正，继续重试。
			if isNonRetryableErr(finalErr) {
				if p.trySelfHeal(finalErr) {
					log.Printf("[model] stream self-healed: provider=%s attempt=%d err=%v", p.name, attempt, finalErr)
				} else {
					log.Printf("[model] stream non-retryable: provider=%s attempt=%d err=%v", p.name, attempt, finalErr)
					yield(nil, finalErr)
					return
				}
			}
			if attempt < providerMaxRetries {
				log.Printf("[model] stream retry: provider=%s attempt=%d/%d backoff=%v err=%v",
					p.name, attempt, providerMaxRetries, backoff, finalErr)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					yield(nil, ctx.Err())
					return
				}
				backoff *= 2
				if backoff > providerRetryMaxBackoff {
					backoff = providerRetryMaxBackoff
				}
				continue
			}
			log.Printf("[model] stream exhausted retries: provider=%s attempts=%d last_err=%v",
				p.name, providerMaxRetries, finalErr)
			yield(nil, fmt.Errorf("provider %s stream failed after %d retries: %w",
				p.name, providerMaxRetries, finalErr))
			return
		}
	}
}

// wrapWithRetry 用 retryProvider 包装底层 provider。
// 在 createBladesProvider 中调用，确保所有 provider（anthropic/openai-chat/openai-responses/ollama）
// 都获得 3 次重试能力。name 用 cfg.Model 便于日志定位具体模型。
func wrapWithRetry(inner blades.ModelProvider, name string) blades.ModelProvider {
	if inner == nil {
		return nil
	}
	return &retryProvider{inner: inner, name: name}
}
