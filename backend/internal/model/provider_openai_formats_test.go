package model

// provider_openai_formats_test.go 覆盖 openai-chat 与 openai-responses 两种自研 provider 的
// 字段兼容行为：请求字段形态、响应解析、usage 变体回退、reasoning_content 双向流转、流式累积。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

// testTool 构造一个仅用于请求序列化测试的 blades 工具（handler 不会被调用）。
func testTool(name, desc string) tools.Tool {
	return tools.NewTool(name, desc, nil,
		tools.WithInputSchema(&jsonschema.Schema{Type: "object"}))
}

// chatTestRequest 构造含 system/user/assistant(带工具调用+reasoning)/tool 结果的完整请求。
func chatTestRequest() *blades.ModelRequest {
	assistant := blades.AssistantMessage()
	assistant.Parts = append(assistant.Parts,
		blades.TextPart{Text: "我来查一下"},
		blades.ToolPart{ID: "call_1", Name: "get_weather", Request: `{"city":"bj"}`},
	)
	assistant.Metadata = map[string]any{"reasoning_content": "用户要查天气"}
	toolResult := &blades.Message{
		Role:  blades.RoleTool,
		Parts: []blades.Part{blades.ToolPart{ID: "call_1", Name: "get_weather", Request: `{"city":"bj"}`, Response: "晴"}},
	}
	return &blades.ModelRequest{
		Instruction: blades.SystemMessage("你是助手"),
		Messages: []*blades.Message{
			blades.UserMessage("北京天气"),
			assistant,
			toolResult,
		},
		Tools: []tools.Tool{testTool("get_weather", "查询天气")},
	}
}

// ---------- openai-chat ----------

// TestChatMaxTokensField 验证按模型名选择 max_tokens / max_completion_tokens。
func TestChatMaxTokensField(t *testing.T) {
	cases := map[string]string{
		"gpt-4o":            "max_tokens",
		"gpt-4o-mini":       "max_tokens",
		"deepseek-v4-flash": "max_tokens",
		"deepseek-reasoner": "max_tokens",
		"kimi-k2":           "max_tokens",
		"glm-4.6":           "max_tokens",
		"o1":                "max_completion_tokens",
		"o3-mini":           "max_completion_tokens",
		"o4-mini":           "max_completion_tokens",
		"gpt-5":             "max_completion_tokens",
		"gpt-5.1-codex":     "max_completion_tokens",
		"openai/gpt-5":      "max_completion_tokens", // openrouter 风格前缀
		"azure/O3-PRO":      "max_completion_tokens", // 大小写不敏感
	}
	for model, want := range cases {
		if got := chatMaxTokensField(model); got != want {
			t.Errorf("chatMaxTokensField(%q) = %q, want %q", model, got, want)
		}
	}
}

// TestChatProvider_RequestFields 验证请求体字段形态：
// gpt-5 用 max_completion_tokens；temperature=0 不发送；reasoning_content 回传；
// tools 为嵌套 function 格式；assistant 工具调用成 tool_calls；tool 结果带 tool_call_id。
func TestChatProvider_RequestFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("missing auth header")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"晴天"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	p := newOpenAIChatProvider(types.AgentModelConfig{
		Provider: "openai-chat", Model: "gpt-5", APIKey: "k", BaseURL: srv.URL + "/v1", MaxTokens: 100,
	})
	resp, err := p.Generate(context.Background(), chatTestRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "晴天" {
		t.Fatalf("text = %q", resp.Message.Text())
	}

	// max token 字段：gpt-5 → max_completion_tokens，且不得出现 max_tokens。
	if gotBody["max_completion_tokens"].(float64) != 100 {
		t.Errorf("max_completion_tokens missing: %v", gotBody)
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Errorf("max_tokens should not be sent for gpt-5")
	}
	// temperature=0 不发送。
	if _, ok := gotBody["temperature"]; ok {
		t.Errorf("temperature should be omitted when 0")
	}
	// 消息序列：system 前缀 + user + assistant + tool。
	msgs := gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first message should be system instruction")
	}
	assistantMsg := msgs[2].(map[string]any)
	if assistantMsg["reasoning_content"] != "用户要查天气" {
		t.Errorf("reasoning_content not passed back: %v", assistantMsg)
	}
	tcs := assistantMsg["tool_calls"].([]any)
	if len(tcs) != 1 || tcs[0].(map[string]any)["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_calls malformed: %v", assistantMsg["tool_calls"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "晴" {
		t.Errorf("tool result malformed: %v", toolMsg)
	}
	// tools 为 chat 嵌套 function 格式。
	tool0 := gotBody["tools"].([]any)[0].(map[string]any)
	if tool0["type"] != "function" || tool0["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tools malformed: %v", tool0)
	}
	// usage 透传。
	if resp.Message.TokenUsage.InputTokens != 10 || resp.Message.TokenUsage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Message.TokenUsage)
	}
}

// TestChatProvider_UsageFallback 验证 DeepSeek 风格 usage 变体回退：
// prompt_tokens=0 时用 cache hit+miss；completion_tokens=0 时用 reasoning 明细。
func TestChatProvider_UsageFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200,"completion_tokens_details":{"reasoning_tokens":60}}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "deepseek-v4-flash", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	u := resp.Message.TokenUsage
	if u.InputTokens != 1000 || u.OutputTokens != 60 || u.TotalTokens != 1060 {
		t.Errorf("usage fallback wrong: %+v", u)
	}
}

// TestChatProvider_ContentArrayForm 验证 content 数组形态兼容（部分多模态/代理端点）。
func TestChatProvider_ContentArrayForm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"你好"},{"type":"text","text":"世界"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "你好世界" {
		t.Errorf("array content = %q", resp.Message.Text())
	}
}

// TestChatProvider_ReasoningFieldFallback 验证响应侧 reasoning 字段（无 reasoning_content 时）捕获。
func TestChatProvider_ReasoningFieldFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"答","reasoning":"想"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Metadata["reasoning_content"] != "想" {
		t.Errorf("reasoning fallback = %v", resp.Message.Metadata)
	}
}

// TestChatProvider_Streaming 验证流式累积：文本/推理增量、tool_calls 分片合并、末块 usage。
func TestChatProvider_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"好\",\"tool_calls\":[{\"index\":0,\"id\":\"call_9\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":1}\"}}]},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	stream := p.NewStreaming(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	var final *blades.ModelResponse
	for resp, err := range stream {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		final = resp
	}
	if final == nil || final.Message.Text() != "你好" {
		t.Fatalf("final text = %v", final)
	}
	if final.Message.Metadata["reasoning_content"] != "思" {
		t.Errorf("reasoning = %v", final.Message.Metadata)
	}
	var toolPart *blades.ToolPart
	for _, part := range final.Message.Parts {
		if tp, ok := part.(blades.ToolPart); ok {
			toolPart = &tp
		}
	}
	if toolPart == nil || toolPart.ID != "call_9" || toolPart.Name != "f" || toolPart.Request != `{"a":1}` {
		t.Errorf("merged tool call = %+v", toolPart)
	}
	if final.Message.TokenUsage.TotalTokens != 7 {
		t.Errorf("usage = %+v", final.Message.TokenUsage)
	}
}

// TestChatProvider_TruncatedToolCallDropped 验证非流式 finish_reason=length 时
// tool_calls 被整轮丢弃：半截 arguments（端点自动补齐引号括号后"合法"）执行会
// 把文件写残（实证 monster.js 730 行被截断写入 64 行）。
func TestChatProvider_TruncatedToolCallDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"WriteFile","arguments":"{\"path\":\"js/monster.js\",\"content\":\""}}]},"finish_reason":"length"}],"usage":{}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Provider: "openai-chat", Model: "m", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), chatTestRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, part := range resp.Message.Parts {
		if _, ok := part.(blades.ToolPart); ok {
			t.Fatal("truncated tool_call should be dropped, got ToolPart in parts")
		}
	}
	if resp.Message.FinishReason != "length" {
		t.Errorf("finish reason = %q, want length", resp.Message.FinishReason)
	}
}

// TestChatProvider_StreamingTruncatedToolCallDropped 验证流式 finish_reason=length
// 时末块不携带累积的半截 tool_call。
func TestChatProvider_StreamingTruncatedToolCallDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"WriteFile\",\"arguments\":\"{\\\"path\\\":\\\"a.js\\\",\\\"content\\\":\\\"\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"partial\"}}]},\"finish_reason\":\"length\"}],\"usage\":{}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Provider: "openai-chat", Model: "m", BaseURL: srv.URL})
	var final *blades.ModelResponse
	for resp, err := range p.NewStreaming(context.Background(), chatTestRequest()) {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		final = resp
	}
	if final == nil {
		t.Fatal("no final message")
	}
	for _, part := range final.Message.Parts {
		if _, ok := part.(blades.ToolPart); ok {
			t.Fatal("truncated tool_call should be dropped in streaming final")
		}
	}
}

// TestResponsesProvider_IncompleteToolCallDropped 验证非流式 status=incomplete +
// max_output_tokens 时 function_call 被丢弃（半截 arguments 写残文件）。
func TestResponsesProvider_IncompleteToolCallDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"r1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"c1","name":"WriteFile","arguments":"{\"path\":\"a.js\",\"content\":\""}],"usage":{}}`)
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Provider: "openai-responses", Model: "m", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), chatTestRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, part := range resp.Message.Parts {
		if _, ok := part.(blades.ToolPart); ok {
			t.Fatal("incomplete function_call should be dropped, got ToolPart")
		}
	}
	if resp.Message.FinishReason != "length" {
		t.Errorf("finish reason = %q, want length", resp.Message.FinishReason)
	}
}

// TestResponsesProvider_StreamingIncompleteDropsToolCalls 验证流式 response.incomplete
// 事件（max_output_tokens）同样丢弃 function_call。
func TestResponsesProvider_StreamingIncompleteDropsToolCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"WriteFile\",\"arguments\":\"{\\\"path\\\":\\\"a.js\\\"\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{}}}\n\n")
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Provider: "openai-responses", Model: "m", BaseURL: srv.URL})
	var final *blades.ModelResponse
	for resp, err := range p.NewStreaming(context.Background(), chatTestRequest()) {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		final = resp
	}
	if final == nil {
		t.Fatal("no final message")
	}
	for _, part := range final.Message.Parts {
		if _, ok := part.(blades.ToolPart); ok {
			t.Fatal("incomplete function_call should be dropped in streaming final")
		}
	}
}

// TestChatProvider_HTTPError 验证非 200 错误体透传（4xx 标记供 retry 层判定不重试）。
func TestChatProvider_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	_, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_request_error") {
		t.Fatalf("err = %v", err)
	}
	if !isNonRetryableErr(err) {
		t.Errorf("400 with invalid_request_error should be non-retryable")
	}
}

// ---------- openai-responses ----------

// TestResponsesProvider_RequestShape 验证 Responses API 请求体形態：
// instructions 顶层字段；input 项类型（input_text/output_text/function_call/function_call_output）；
// tools 扁平化；max_output_tokens 下限钳制到 16。
func TestResponsesProvider_RequestShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"晴天"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`)
	}))
	defer srv.Close()

	// MaxTokens=1 模拟启动连通性探测：应被钳制到 16 而非直接发送 1。
	p := newOpenAIResponsesProvider(types.AgentModelConfig{
		Provider: "openai-responses", Model: "gpt-5", APIKey: "k", BaseURL: srv.URL + "/v1", MaxTokens: 1,
	})
	resp, err := p.Generate(context.Background(), chatTestRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Message.Text() != "晴天" {
		t.Fatalf("text = %q", resp.Message.Text())
	}

	if gotBody["instructions"] != "你是助手" {
		t.Errorf("instructions = %v", gotBody["instructions"])
	}
	if gotBody["max_output_tokens"].(float64) != responsesMinMaxOutputTokens {
		t.Errorf("max_output_tokens = %v, want clamp to 16", gotBody["max_output_tokens"])
	}
	if _, ok := gotBody["max_tokens"]; ok {
		t.Errorf("max_tokens must not appear in responses format")
	}
	if _, ok := gotBody["temperature"]; ok {
		t.Errorf("temperature should be omitted when 0")
	}
	if _, ok := gotBody["stream"]; ok {
		t.Errorf("stream must not appear in non-stream request")
	}

	input := gotBody["input"].([]any)
	// user 消息 → input_text
	user := input[0].(map[string]any)
	if user["role"] != "user" || user["content"].([]any)[0].(map[string]any)["type"] != "input_text" {
		t.Errorf("user item malformed: %v", user)
	}
	// assistant → output_text 消息项 + function_call 项
	assistantMsg := input[1].(map[string]any)
	if assistantMsg["role"] != "assistant" || assistantMsg["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Errorf("assistant item malformed: %v", assistantMsg)
	}
	fc := input[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" || fc["arguments"] != `{"city":"bj"}` {
		t.Errorf("function_call item malformed: %v", fc)
	}
	fco := input[3].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "晴" {
		t.Errorf("function_call_output item malformed: %v", fco)
	}
	// tools 扁平格式（无嵌套 function 键）
	tool0 := gotBody["tools"].([]any)[0].(map[string]any)
	if tool0["type"] != "function" || tool0["name"] != "get_weather" {
		t.Errorf("responses tools should be flat: %v", tool0)
	}
	if _, nested := tool0["function"]; nested {
		t.Errorf("responses tools must not nest under function key: %v", tool0)
	}
	// usage 透传
	if resp.Message.TokenUsage.InputTokens != 10 || resp.Message.TokenUsage.OutputTokens != 5 || resp.Message.TokenUsage.TotalTokens != 15 {
		t.Errorf("usage = %+v", resp.Message.TokenUsage)
	}
	if resp.Message.FinishReason != "stop" {
		t.Errorf("finish = %q", resp.Message.FinishReason)
	}
}

// TestResponsesProvider_ParseOutputItems 验证 output[] 多项解析：
// reasoning summary 入 Metadata；incomplete（max_output_tokens）时 function_call
// 被丢弃（截断的 arguments 不可信，执行会写残文件），finish_reason=length。
func TestResponsesProvider_ParseOutputItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"resp_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"想了"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"部分"}]},{"type":"function_call","call_id":"call_7","name":"f","arguments":"{\"x\":1}"}],"usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	m := resp.Message
	if m.Text() != "部分" {
		t.Errorf("text = %q", m.Text())
	}
	if m.Metadata["reasoning_content"] != "想了" {
		t.Errorf("reasoning = %v", m.Metadata)
	}
	// incomplete + max_output_tokens：function_call 参数不可信，整轮丢弃并判 length。
	if m.FinishReason != "length" {
		t.Errorf("finish = %q", m.FinishReason)
	}
	for _, part := range m.Parts {
		if _, ok := part.(blades.ToolPart); ok {
			t.Errorf("incomplete function_call should be dropped, got ToolPart")
		}
	}
	// total 缺失时 input+output 合计。
	if m.TokenUsage.TotalTokens != 7 {
		t.Errorf("usage = %+v", m.TokenUsage)
	}
}

// TestResponsesProvider_UsageChatNamingFallback 验证代理端点沿用 chat 命名（prompt/completion）的回退。
func TestResponsesProvider_UsageChatNamingFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"prompt_tokens":11,"completion_tokens":22}}`)
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	resp, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	u := resp.Message.TokenUsage
	if u.InputTokens != 11 || u.OutputTokens != 22 || u.TotalTokens != 33 {
		t.Errorf("usage = %+v", u)
	}
}

// TestResponsesProvider_Streaming 验证事件流：文本增量累积、function_call 项完成捕获、
// completed 事件 usage；未知事件跳过。
func TestResponsesProvider_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_9\",\"status\":\"in_progress\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"思\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"好\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":\"call_5\",\"name\":\"g\",\"arguments\":\"{\\\"q\\\":2}\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_9\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":6,\"total_tokens\":11}}}\n\n")
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	stream := p.NewStreaming(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	var final *blades.ModelResponse
	for resp, err := range stream {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		final = resp
	}
	if final == nil || final.Message.Text() != "你好" {
		t.Fatalf("final = %v", final)
	}
	if final.Message.Metadata["reasoning_content"] != "思" {
		t.Errorf("reasoning = %v", final.Message.Metadata)
	}
	if final.Message.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", final.Message.FinishReason)
	}
	var tp *blades.ToolPart
	for _, part := range final.Message.Parts {
		if p2, ok := part.(blades.ToolPart); ok {
			tp = &p2
		}
	}
	if tp == nil || tp.ID != "call_5" || tp.Name != "g" || tp.Request != `{"q":2}` {
		t.Errorf("tool call = %+v", tp)
	}
	if final.Message.TokenUsage.TotalTokens != 11 {
		t.Errorf("usage = %+v", final.Message.TokenUsage)
	}
}

// TestResponsesProvider_StreamingFallback 验证 function_call 兜底：
// 端点漏发 output_item.done 时，从 response.completed.output[] 提取 function_call。
func TestResponsesProvider_StreamingFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		// 故意不发 response.output_item.done；function_call 仅出现在 completed.output[]。
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_fb\",\"name\":\"tool\",\"arguments\":\"{\\\"a\\\":1}\"}],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n")
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	stream := p.NewStreaming(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	var final *blades.ModelResponse
	for resp, err := range stream {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		final = resp
	}
	if final == nil || final.Message.Text() != "hi" {
		t.Fatalf("final = %v", final)
	}
	if final.Message.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", final.Message.FinishReason)
	}
	var tp *blades.ToolPart
	for _, part := range final.Message.Parts {
		if p2, ok := part.(blades.ToolPart); ok {
			tp = &p2
		}
	}
	if tp == nil || tp.ID != "call_fb" || tp.Name != "tool" || tp.Request != `{"a":1}` {
		t.Errorf("fallback tool call = %+v", tp)
	}
	if final.Message.TokenUsage.TotalTokens != 5 {
		t.Errorf("usage = %+v", final.Message.TokenUsage)
	}
}

// TestResponsesProvider_FailedStatus 验证 status=failed 返回错误。
func TestResponsesProvider_FailedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"r","status":"failed","error":{"code":"server_error","message":"boom"},"output":[]}`)
	}))
	defer srv.Close()
	p := newOpenAIResponsesProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})
	_, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("hi")}})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// ---------- 工厂分发 ----------

// TestCreateBladesProvider_Formats 验证 openai 系 provider 名分发与别名：
// openai/openai-chat/openai-deepseek/空 → chat；openai-responses → responses。
func TestCreateBladesProvider_Formats(t *testing.T) {
	chatNames := []string{"openai", "openai-chat", "openai-deepseek", ""}
	for _, name := range chatNames {
		p, err := createBladesProvider(types.AgentModelConfig{Provider: name, Model: "m", APIKey: "k"})
		if err != nil {
			t.Fatalf("provider %q: %v", name, err)
		}
		rp, ok := p.(*retryProvider)
		if !ok {
			t.Fatalf("provider %q not wrapped with retry", name)
		}
		if _, ok := rp.inner.(*openAIChatProvider); !ok {
			t.Errorf("provider %q inner = %T, want *openAIChatProvider", name, rp.inner)
		}
		if p.Name() != "m" {
			t.Errorf("provider %q Name = %q", name, p.Name())
		}
	}
	p, err := createBladesProvider(types.AgentModelConfig{Provider: "openai-responses", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatalf("openai-responses: %v", err)
	}
	rp := p.(*retryProvider)
	if _, ok := rp.inner.(*openAIResponsesProvider); !ok {
		t.Errorf("openai-responses inner = %T, want *openAIResponsesProvider", rp.inner)
	}
}

// TestChatProvider_UserImageContentArray 验证 user 消息携带图片 DataPart
//（Alt+V 粘贴）时请求 content 用数组形态 [{type:text},{type:image_url,data URL}]；
// 无图消息仍为 string content（兼容回归）。
func TestChatProvider_UserImageContentArray(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})

	raw := []byte{0x89, 0x50, 0x4E, 0x47}
	msg := &blades.Message{Role: blades.RoleUser, Parts: []blades.Part{
		blades.TextPart{Text: "按 [image:1] 实现"},
		blades.DataPart{MIMEType: blades.MIMEImagePNG, Bytes: raw},
	}}
	if _, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{msg}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	arr, ok := gotBody["messages"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("messages 应为 1 条数组, got %v", gotBody["messages"])
	}
	first, _ := arr[0].(map[string]any)
	content, ok := first["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("带图 user content 应为数组 2 项, got %v", first["content"])
	}
	txt, _ := content[0].(map[string]any)
	if txt["type"] != "text" || txt["text"] != "按 [image:1] 实现" {
		t.Fatalf("text 项不符: %v", txt)
	}
	iu, _ := content[1].(map[string]any)
	if iu["type"] != "image_url" {
		t.Fatalf("image_url 项 type 不符: %v", iu)
	}
	inner, _ := iu["image_url"].(map[string]any)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	if inner["url"] != want {
		t.Fatalf("data URL 不符: %v", inner["url"])
	}

	// 无图消息：content 仍为 string。
	if _, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{blades.UserMessage("纯文本")}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	arr2, _ := gotBody["messages"].([]any)
	second, _ := arr2[0].(map[string]any)
	if _, isArray := second["content"].([]any); isArray {
		t.Fatalf("无图 user content 不应为数组: %v", second["content"])
	}
	if second["content"] != "纯文本" {
		t.Fatalf("无图 content 不符: %v", second["content"])
	}
}

// TestChatProvider_UserVideoContentArray 验证 user 消息携带 video/* DataPart
//（native 模式视频直传）时请求 content 数组项映射为 video_url（Ark/GLM 视频
// 理解的 OpenAI 兼容格式），而非误标 image_url；audio/* 等未支持模态被跳过。
func TestChatProvider_UserVideoContentArray(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer srv.Close()
	p := newOpenAIChatProvider(types.AgentModelConfig{Model: "m", APIKey: "k", BaseURL: srv.URL})

	raw := []byte{0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70} // 假 mp4 头
	audio := []byte{0x01, 0x02, 0x03}
	msg := &blades.Message{Role: blades.RoleUser, Parts: []blades.Part{
		blades.TextPart{Text: "描述 [video:1]"},
		blades.DataPart{MIMEType: blades.MIMEType("video/mp4"), Bytes: raw},
		blades.DataPart{MIMEType: blades.MIMEType("audio/wav"), Bytes: audio},
	}}
	if _, err := p.Generate(context.Background(), &blades.ModelRequest{Messages: []*blades.Message{msg}}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	arr, _ := gotBody["messages"].([]any)
	first, _ := arr[0].(map[string]any)
	content, ok := first["content"].([]any)
	if !ok || len(content) != 2 { // text + video_url（audio 被跳过）
		t.Fatalf("content 应为 2 项（text+video_url，audio 跳过）, got %v", first["content"])
	}
	vu, _ := content[1].(map[string]any)
	if vu["type"] != "video_url" {
		t.Fatalf("video DataPart 应映射 video_url, got: %v", vu)
	}
	inner, _ := vu["video_url"].(map[string]any)
	want := "data:video/mp4;base64," + base64.StdEncoding.EncodeToString(raw)
	if inner["url"] != want {
		t.Fatalf("video data URL 不符: %v", inner["url"])
	}
}
