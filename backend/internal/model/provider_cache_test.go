package model

// provider_cache_test.go 验证 TODO #40 块 2 缓存可观测：三 provider 把缓存命中/未命中
// token 经 blades.Message.Metadata 透传（cache_hit_tokens / cache_miss_tokens）。

import (
	"strings"
	"testing"

	"github.com/go-kratos/blades"
)

// TestSetCacheUsageMeta_ZeroNoop 全零不写缓存 key（防噪声；Metadata 可能被
// NewAssistantMessage 预初始化成空 map，断言不含 key 即可）。
func TestSetCacheUsageMeta_ZeroNoop(t *testing.T) {
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	setCacheUsageMeta(msg, 0, 0)
	if _, ok := msg.Metadata["cache_hit_tokens"]; ok {
		t.Fatalf("zero cache usage must not write cache_hit_tokens, got: %v", msg.Metadata)
	}
	if _, ok := msg.Metadata["cache_miss_tokens"]; ok {
		t.Fatalf("zero cache usage must not write cache_miss_tokens, got: %v", msg.Metadata)
	}
}

// TestSetCacheUsageMeta_Positive 正数写入两个 key。
func TestSetCacheUsageMeta_Positive(t *testing.T) {
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	setCacheUsageMeta(msg, 100, 200)
	if got := msg.Metadata["cache_hit_tokens"]; got != int64(100) {
		t.Fatalf("cache_hit_tokens = %v, want 100", got)
	}
	if got := msg.Metadata["cache_miss_tokens"]; got != int64(200) {
		t.Fatalf("cache_miss_tokens = %v, want 200", got)
	}
}

// TestOpenAIChat_ParseCacheMeta DeepSeek 原生 prompt_cache_hit/miss_tokens 解析进 Metadata。
func TestOpenAIChat_ParseCacheMeta(t *testing.T) {
	raw := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":150,"completion_tokens":50,"total_tokens":200,"prompt_cache_hit_tokens":120,"prompt_cache_miss_tokens":30}}`
	resp, err := parseChatResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parseChatResponse: %v", err)
	}
	md := resp.Message.Metadata
	if got := md["cache_hit_tokens"]; got != int64(120) {
		t.Fatalf("cache_hit_tokens = %v, want 120", got)
	}
	if got := md["cache_miss_tokens"]; got != int64(30) {
		t.Fatalf("cache_miss_tokens = %v, want 30", got)
	}
}

// TestOpenAIChat_ParseNoCacheFields 无 cache 字段时不写 Metadata。
func TestOpenAIChat_ParseNoCacheFields(t *testing.T) {
	raw := `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
	"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	resp, err := parseChatResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parseChatResponse: %v", err)
	}
	if _, ok := resp.Message.Metadata["cache_hit_tokens"]; ok {
		t.Fatal("no cache fields must not write cache_hit_tokens")
	}
}

// TestOpenAIResponses_CachedTokens openai-responses 的 input_tokens_details.cached_tokens
// 作 hit、input-cached 作 miss。
func TestOpenAIResponses_CachedTokens(t *testing.T) {
	raw := `{"id":"x","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],
	"usage":{"input_tokens":200,"output_tokens":50,"total_tokens":250,"input_tokens_details":{"cached_tokens":170}}}`
	resp, err := parseResponsesAPIResponse([]byte(raw))
	if err != nil {
		t.Fatalf("parseResponsesAPIResponse: %v", err)
	}
	md := resp.Message.Metadata
	if got := md["cache_hit_tokens"]; got != int64(170) {
		t.Fatalf("cache_hit_tokens = %v, want 170", got)
	}
	if got := md["cache_miss_tokens"]; got != int64(30) {
		t.Fatalf("cache_miss_tokens = %v, want 30", got)
	}
}

// TestResponsesFinishReason_NoToolCalls 防回归：工具调用场景 finish reason 不变。
func TestResponsesFinishReason_NoToolCalls(t *testing.T) {
	if r := responsesFinishReason(&responsesAPIResponseBody{Status: "completed"}, false); r != "stop" {
		t.Fatalf("finish reason = %q, want stop", r)
	}
	if r := responsesFinishReason(nil, true); r != "tool_calls" {
		t.Fatalf("finish reason with tool calls = %q, want tool_calls", r)
	}
	if r := responsesFinishReason(&responsesAPIResponseBody{Status: "completed", IncompleteDetails: &struct {
		Reason string `json:"reason"`
	}{Reason: "max_output_tokens"}}, false); !strings.Contains(r, "length") {
		t.Fatalf("incomplete reason = %q, want contains length", r)
	}
}
