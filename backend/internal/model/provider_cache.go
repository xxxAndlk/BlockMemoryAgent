package model

// provider_cache.go 实现 TODO #40 块 2 缓存命中率可观测：
// blades.TokenUsage 是外部库类型（不可扩展字段），三个 provider 把各自解析的
// 缓存命中/未命中 token 经 blades.Message.Metadata 透传
// （key: cache_hit_tokens / cache_miss_tokens，int64），
// logLLMCall（react_agent.go）读取后落 LLMCallRecord 日志，供会话聚合与 TUI 展示。
//
// 各 provider 的语义映射：
//   - openai-chat（DeepSeek 原生）：prompt_cache_hit_tokens / prompt_cache_miss_tokens；
//   - openai-responses：input_tokens_details.cached_tokens 作 hit，input - cached 作 miss；
//   - anthropic：CacheReadInputTokens 作 hit，CacheCreationInputTokens 作 miss。

import "github.com/go-kratos/blades"

// setCacheUsageMeta 把缓存命中/未命中 token 写入消息 Metadata（仅 >0 时写，防噪声）。
func setCacheUsageMeta(msg *blades.Message, hit, miss int64) {
	if hit <= 0 && miss <= 0 {
		return
	}
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]any)
	}
	if hit > 0 {
		msg.Metadata["cache_hit_tokens"] = hit
	}
	if miss > 0 {
		msg.Metadata["cache_miss_tokens"] = miss
	}
}
