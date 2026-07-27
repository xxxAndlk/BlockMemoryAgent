package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/google/jsonschema-go/jsonschema"
)

// deepseekProvider 直连 DeepSeek OpenAI 兼容端点的 provider。
//
// 为什么不用 blades contrib/openai：
//   - DeepSeek V4 思考模型（deepseek-v4-pro/flash）在 assistant 响应里返回 reasoning_content；
//   - DeepSeek V4 API 要求后续请求把 reasoning_content 回传到对应 assistant 消息，
//     否则 400 "reasoning_content must be passed back"；
//   - blades contrib/openai@v0.3.0 既不捕获 reasoning_content（choiceToResponse 丢弃），
//     也不在请求序列化时回传，且把 assistant 消息错误地当 user 消息发送。
//
// 本 provider 用 net/http 直连，自行序列化/反序列化，完整支持 reasoning_content 双向流转：
//   - 请求侧：从 blades.Message.Metadata["reasoning_content"] 取值，写入 assistant 消息 JSON；
//   - 响应侧：从 choices[].message.reasoning_content 取值，写入 blades.Message.Metadata。
//
// ReactMessage.ReasoningContent <-> blades.Message.Metadata["reasoning_content"] 的映射
// 在 agent 包的 ToBladesMessages / AssistantMessageFromBlades 中完成。
type deepseekProvider struct {
	baseURL     string
	apiKey      string
	modelName   string
	temperature float64
	maxTokens   int
	httpClient  *http.Client
}

// newDeepSeekProvider 构造一个直连 DeepSeek 的 provider。
func newDeepSeekProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	return &deepseekProvider{
		baseURL:     strings.TrimRight(baseURL, "/"),
		apiKey:      cfg.APIKey,
		modelName:   cfg.Model,
		temperature: cfg.Temperature,
		maxTokens:   cfg.MaxTokens,
		httpClient: &http.Client{
			Timeout: 600 * time.Second,
		},
	}
}

// Name 返回模型名。
func (p *deepseekProvider) Name() string { return p.modelName }

// Generate 执行非流式 chat completion，捕获 reasoning_content 写入 Metadata。
func (p *deepseekProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	body, err := p.buildRequestJSON(req, false)
	if err != nil {
		return nil, fmt.Errorf("deepseek build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("deepseek new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("deepseek do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("deepseek read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("deepseek %d: %s", resp.StatusCode, string(raw))
	}

	return parseDeepSeekResponse(raw)
}

// NewStreaming 执行流式请求，逐块产出；最后一块为完整累积响应。
// SSE 解析 delta.content / delta.reasoning_content / delta.tool_calls。
func (p *deepseekProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		body, err := p.buildRequestJSON(req, true)
		if err != nil {
			yield(nil, fmt.Errorf("deepseek build request: %w", err))
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, fmt.Errorf("deepseek new request: %w", err))
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("deepseek do: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			yield(nil, fmt.Errorf("deepseek stream %d: %s", resp.StatusCode, string(raw)))
			return
		}

		// 累积器：content / reasoning_content / tool_calls / finish_reason / usage
		var contentBuf strings.Builder
		var reasoningBuf strings.Builder
		var toolCalls []map[string]any
		var finishReason string
		var usage promptUsage

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content          string         `json:"content"`
						ReasoningContent string         `json:"reasoning_content"`
						ToolCalls        []deepseekTool `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *promptUsage `json:"usage,omitempty"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			for _, c := range chunk.Choices {
				if c.Delta.Content != "" {
					contentBuf.WriteString(c.Delta.Content)
				}
				if c.Delta.ReasoningContent != "" {
					reasoningBuf.WriteString(c.Delta.ReasoningContent)
				}
				for _, tc := range c.Delta.ToolCalls {
					toolCalls = mergeToolCall(toolCalls, tc)
				}
				if c.FinishReason != "" {
					finishReason = c.FinishReason
				}
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			// 推送增量响应（文本 + reasoning），UI 可据此流式渲染。
			msg := blades.NewAssistantMessage(blades.StatusIncomplete)
			if contentBuf.Len() > 0 {
				msg.Parts = append(msg.Parts, blades.TextPart{Text: contentBuf.String()})
			}
			if reasoningBuf.Len() > 0 {
				if msg.Metadata == nil {
					msg.Metadata = make(map[string]any)
				}
				msg.Metadata["reasoning_content"] = reasoningBuf.String()
			}
			msg.FinishReason = finishReason
			if !yield(&blades.ModelResponse{Message: msg}, nil) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			yield(nil, fmt.Errorf("deepseek scan: %w", err))
			return
		}

		// 最终完整响应。
		final := blades.NewAssistantMessage(blades.StatusCompleted)
		if contentBuf.Len() > 0 {
			final.Parts = append(final.Parts, blades.TextPart{Text: contentBuf.String()})
		}
		for _, tc := range toolCalls {
			final.Role = blades.RoleTool
			final.Parts = append(final.Parts, blades.ToolPart{
				ID:      fmt.Sprintf("%v", tc["id"]),
				Name:    toolCallName(tc),
				Request: toolCallArgs(tc),
			})
		}
		if reasoningBuf.Len() > 0 {
			if final.Metadata == nil {
				final.Metadata = make(map[string]any)
			}
			final.Metadata["reasoning_content"] = reasoningBuf.String()
		}
		final.FinishReason = finishReason
		final.TokenUsage = blades.TokenUsage{
			InputTokens:  int64(usage.PromptTokens),
			OutputTokens: int64(usage.CompletionTokens),
			TotalTokens:  int64(usage.TotalTokens),
		}
		yield(&blades.ModelResponse{Message: final}, nil)
	}
}

// promptUsage 是 OpenAI 兼容的 usage 字段。
type promptUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// deepseekTool 是 OpenAI 兼容的 tool_call 增量结构。
type deepseekTool struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// mergeToolCall 把增量 tool_call 合并到累积切片（按 index 聚合 id/name/arguments）。
func mergeToolCall(acc []map[string]any, tc deepseekTool) []map[string]any {
	idx := tc.Index
	for idx >= len(acc) {
		acc = append(acc, map[string]any{})
	}
	entry := acc[idx]
	if tc.ID != "" {
		entry["id"] = tc.ID
	}
	if tc.Function.Name != "" {
		entry["name"] = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		prev, _ := entry["arguments"].(string)
		entry["arguments"] = prev + tc.Function.Arguments
	}
	return acc
}

func toolCallName(tc map[string]any) string {
	if v, ok := tc["name"].(string); ok {
		return v
	}
	return ""
}

func toolCallArgs(tc map[string]any) string {
	if v, ok := tc["arguments"].(string); ok {
		return v
	}
	return ""
}

// buildRequestJSON 构造 DeepSeek/OpenAI 兼容的 chat/completions 请求体。
// stream=true 时附加 stream:true。
func (p *deepseekProvider) buildRequestJSON(req *blades.ModelRequest, stream bool) ([]byte, error) {
	body := map[string]any{
		"model": p.modelName,
	}
	if p.temperature > 0 {
		body["temperature"] = p.temperature
	}
	if p.maxTokens > 0 {
		body["max_tokens"] = p.maxTokens
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	// Instruction 作为 system 前缀。
	if req.Instruction != nil {
		msgs = append(msgs, map[string]any{"role": "system", "content": textOf(req.Instruction)})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, bladesMessageToDeepSeek(m))
	}
	body["messages"] = msgs

	// Tools 转换为 OpenAI function 格式。
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{
				"name": t.Name(),
			}
			if d := t.Description(); d != "" {
				fn["description"] = d
			}
			if schema := t.InputSchema(); schema != nil {
				fn["parameters"] = schemaAsMap(schema)
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		body["tools"] = tools
	}

	return json.Marshal(body)
}

// bladesMessageToDeepSeek 把 blades.Message 转为 DeepSeek/OpenAI 消息 JSON。
// 关键：assistant 消息从 Metadata["reasoning_content"] 取值回传，避免 400。
func bladesMessageToDeepSeek(m *blades.Message) map[string]any {
	if m == nil {
		return nil
	}
	switch m.Role {
	case blades.RoleUser:
		return map[string]any{"role": "user", "content": textOf(m)}
	case blades.RoleSystem:
		return map[string]any{"role": "system", "content": textOf(m)}
	case blades.RoleAssistant:
		out := map[string]any{"role": "assistant"}
		if t := textOf(m); t != "" {
			out["content"] = t
		}
		// 工具调用
		toolCalls := make([]map[string]any, 0)
		for _, part := range m.Parts {
			if tp, ok := part.(blades.ToolPart); ok && tp.Name != "" {
				toolCalls = append(toolCalls, map[string]any{
					"id":   tp.ID,
					"type": "function",
					"function": map[string]any{
						"name":      tp.Name,
						"arguments": tp.Request,
					},
				})
			}
		}
		if len(toolCalls) > 0 {
			out["tool_calls"] = toolCalls
			if _, ok := out["content"]; !ok {
				out["content"] = nil
			}
		}
		// 回传 reasoning_content（DeepSeek V4 思考模型要求）。
		if m.Metadata != nil {
			if rc, ok := m.Metadata["reasoning_content"].(string); ok && rc != "" {
				out["reasoning_content"] = rc
			}
		}
		return out
	case blades.RoleTool:
		// blades 的 RoleTool 消息含 ToolPart（带 Response），转为 OpenAI tool 结果消息。
		for _, part := range m.Parts {
			if tp, ok := part.(blades.ToolPart); ok {
				return map[string]any{
					"role":         "tool",
					"tool_call_id": tp.ID,
					"content":      tp.Response,
				}
			}
		}
		return map[string]any{"role": "tool", "content": textOf(m)}
	default:
		return map[string]any{"role": "user", "content": textOf(m)}
	}
}

// textOf 提取 blades.Message 中所有 TextPart 拼接为字符串。
func textOf(m *blades.Message) string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range m.Parts {
		if tp, ok := part.(blades.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}

// schemaAsMap 把 jsonschema.Schema 转为 map[string]any，便于 JSON 序列化。
func schemaAsMap(s *jsonschema.Schema) map[string]any {
	if s == nil {
		return nil
	}
	// 先 marshal 再 unmarshal，避免类型字段歧义。
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// parseDeepSeekResponse 解析非流式响应，提取 content/tool_calls/reasoning_content/usage。
func parseDeepSeekResponse(raw []byte) (*blades.ModelResponse, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Role             string         `json:"role"`
				Content          string         `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				ToolCalls        []deepseekTool `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage promptUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("deepseek unmarshal: %w", err)
	}

	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  int64(resp.Usage.PromptTokens),
		OutputTokens: int64(resp.Usage.CompletionTokens),
		TotalTokens:  int64(resp.Usage.TotalTokens),
	}
	for _, choice := range resp.Choices {
		if choice.Message.Content != "" {
			msg.Parts = append(msg.Parts, blades.TextPart{Text: choice.Message.Content})
		}
		if choice.Message.ReasoningContent != "" {
			if msg.Metadata == nil {
				msg.Metadata = make(map[string]any)
			}
			msg.Metadata["reasoning_content"] = choice.Message.ReasoningContent
		}
		if choice.FinishReason != "" {
			msg.FinishReason = choice.FinishReason
		}
		for _, call := range choice.Message.ToolCalls {
			msg.Role = blades.RoleTool
			msg.Parts = append(msg.Parts, blades.ToolPart{
				ID:      call.ID,
				Name:    call.Function.Name,
				Request: call.Function.Arguments,
			})
		}
	}
	return &blades.ModelResponse{Message: msg}, nil
}
