package model

// provider_openai_chat.go 实现 OpenAI Chat Completions 兼容端点（POST {base}/chat/completions）
// 的自研 provider，provider 名：openai-chat（openai / openai-deepseek 为其别名）。
//
// 为什么不用 blades contrib/openai：
//   - contrib/openai@v0.3.0 底层走官方 openai-go SDK 的 Chat.Completions，字段序列化由 SDK 钉死，
//     无法兼容各三方端点的字段差异（reasoning_content 不捕获不回传、usage 变体字段丢失）；
//   - DeepSeek V4 思考模型要求 assistant 消息回传 reasoning_content，否则 400
//     "reasoning_content must be passed back"；contrib 既不捕获也不回传。
//
// 本 provider 用 net/http 直连，自行序列化/反序列化，字段兼容策略（覆盖主流 OpenAI 兼容端点）：
//   - max_tokens vs max_completion_tokens：o1/o3/o4/gpt-5 系只认 max_completion_tokens，
//     其余（含 gpt-4o/deepseek/kimi/glm）认 max_tokens，按模型名自动选择（chatMaxTokensField）；
//   - temperature 仅在 >0 时发送：推理模型（o 系/deepseek-reasoner）拒绝非默认温度，
//     配置 temperature: 0 即表示不发送该字段；
//   - reasoning_content 双向流转：响应侧捕获 reasoning_content（DeepSeek/Kimi/GLM），
//     无则回退 reasoning（部分端点字段名）；请求侧从 Metadata["reasoning_content"] 回传；
//   - usage 变体：prompt_tokens=0 时回退 prompt_cache_hit/miss_tokens 合计（DeepSeek V3+ 拆分），
//     completion_tokens=0 时回退 reasoning_tokens（顶层或 completion_tokens_details）；
//   - content 双形态：兼容 string 与 [{type:"text",text:...}] 数组两种返回；
//   - 流式：SSE data: 行解析，stream_options.include_usage 请求末块带 usage。
//
// ReactMessage.ReasoningContent <-> blades.Message.Metadata["reasoning_content"] 的映射
// 在 agent 包的 ToBladesMessages / AssistantMessageFromBlades 中完成。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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

// openAIChatProvider 直连 OpenAI Chat Completions 兼容端点的 provider。
type openAIChatProvider struct {
	baseURL     string
	apiKey      string
	modelName   string
	temperature float64
	maxTokens   int
	httpClient  *http.Client
}

// newOpenAIChatProvider 构造一个 chat/completions 格式的 provider。
// baseURL 为空时默认官方端点 https://api.openai.com/v1。
func newOpenAIChatProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &openAIChatProvider{
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
func (p *openAIChatProvider) Name() string { return p.modelName }

// Generate 执行非流式 chat completion，捕获 reasoning_content 写入 Metadata。
func (p *openAIChatProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	body, err := p.buildRequestJSON(req, false)
	if err != nil {
		return nil, fmt.Errorf("openai-chat build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai-chat new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-chat do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai-chat read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 错误体保留原文：OpenAI 风格 {"error":{"type":"invalid_request_error"}} 中的
		// type 标记供 retry_provider 的 4xx 非重试判定匹配。
		return nil, fmt.Errorf("openai-chat %d: %s", resp.StatusCode, string(raw))
	}

	return parseChatResponse(raw)
}

// NewStreaming 执行流式请求，逐块产出累积响应；最后一块为完整响应（含 tool_calls/usage）。
// SSE 解析 delta.content / delta.reasoning_content / delta.reasoning / delta.tool_calls。
func (p *openAIChatProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		body, err := p.buildRequestJSON(req, true)
		if err != nil {
			yield(nil, fmt.Errorf("openai-chat build request: %w", err))
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, fmt.Errorf("openai-chat new request: %w", err))
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("openai-chat do: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			yield(nil, fmt.Errorf("openai-chat stream %d: %s", resp.StatusCode, string(raw)))
			return
		}

		// 累积器：content / reasoning_content / tool_calls / finish_reason / usage
		var contentBuf strings.Builder
		var reasoningBuf strings.Builder
		var toolCalls []map[string]any
		var finishReason string
		var usage chatUsage

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
						Reasoning        string         `json:"reasoning"` // 部分端点（如某些 kimi/glm 代理）用 reasoning 字段
						ToolCalls        []chatToolCall `json:"tool_calls"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason"`
				} `json:"choices"`
				Usage *chatUsage `json:"usage,omitempty"`
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
				} else if c.Delta.Reasoning != "" {
					reasoningBuf.WriteString(c.Delta.Reasoning)
				}
				for _, tc := range c.Delta.ToolCalls {
					toolCalls = mergeChatToolCall(toolCalls, tc)
				}
				if c.FinishReason != "" {
					finishReason = c.FinishReason
				}
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			// 推送累积响应（文本 + reasoning），UI 据此流式渲染。
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
			yield(nil, fmt.Errorf("openai-chat scan: %w", err))
			return
		}

		// 最终完整响应。
		final := blades.NewAssistantMessage(blades.StatusCompleted)
		if contentBuf.Len() > 0 {
			final.Parts = append(final.Parts, blades.TextPart{Text: contentBuf.String()})
		}
		// finish_reason=length 时丢弃工具调用：arguments 为流中断时累积的半截 JSON，
		// 执行会把文件写残（实证 monster.js 被截断到 64 行）。文本保留（模型下轮可续）。
		if finishReason != "length" {
			for _, tc := range toolCalls {
				final.Role = blades.RoleTool
				final.Parts = append(final.Parts, blades.ToolPart{
					ID:      fmt.Sprintf("%v", tc["id"]),
					Name:    chatToolCallName(tc),
					Request: chatToolCallArgs(tc),
				})
			}
		}
		if reasoningBuf.Len() > 0 {
			if final.Metadata == nil {
				final.Metadata = make(map[string]any)
			}
			final.Metadata["reasoning_content"] = reasoningBuf.String()
		}
		final.FinishReason = finishReason
		final.TokenUsage = blades.TokenUsage{
			InputTokens:  int64(usage.inputTokens()),
			OutputTokens: int64(usage.outputTokens()),
			TotalTokens:  int64(usage.totalTokens()),
		}
		// TODO #40 缓存可观测：DeepSeek 原生 prompt_cache_hit/miss_tokens 透传 Metadata。
		setCacheUsageMeta(final, int64(usage.PromptCacheHitTokens), int64(usage.PromptCacheMissTokens))
		yield(&blades.ModelResponse{Message: final}, nil)
	}
}

// chatUsage 是 OpenAI 兼容的 usage 字段，兼容各端点变体。
// DeepSeek V3+ 拆分 prompt_tokens 为 prompt_cache_hit_tokens + prompt_cache_miss_tokens，
// 部分 V4 思考模型 prompt_tokens 返回 0，需用 cache 字段合计还原真实输入 token；
// OpenAI o 系/gpt-5 的 reasoning tokens 在 completion_tokens_details.reasoning_tokens，
// 部分端点（DeepSeek）在顶层 reasoning_tokens。
type chatUsage struct {
	PromptTokens          int `json:"prompt_tokens"`
	CompletionTokens      int `json:"completion_tokens"`
	TotalTokens           int `json:"total_tokens"`
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens"`
	ReasoningTokens       int `json:"reasoning_tokens"`
	PromptTokensDetails   *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// inputTokens 返回真实输入 token 数：优先 prompt_tokens，为 0 时回退 cache 合计。
func (u chatUsage) inputTokens() int {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.PromptCacheHitTokens + u.PromptCacheMissTokens
}

// outputTokens 返回真实输出 token 数：completion_tokens 优先，
// 为 0 时回退 reasoning_tokens（顶层或 completion_tokens_details）。
func (u chatUsage) outputTokens() int {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	if u.ReasoningTokens > 0 {
		return u.ReasoningTokens
	}
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return 0
}

// totalTokens 返回总 token 数：total_tokens 优先，为 0 时用 input+output 合计。
func (u chatUsage) totalTokens() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.inputTokens() + u.outputTokens()
}

// chatToolCall 是 OpenAI 兼容的 tool_call 结构（流式增量与非流式完整共用）。
type chatToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// mergeChatToolCall 把增量 tool_call 合并到累积切片（按 index 聚合 id/name/arguments）。
func mergeChatToolCall(acc []map[string]any, tc chatToolCall) []map[string]any {
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

func chatToolCallName(tc map[string]any) string {
	if v, ok := tc["name"].(string); ok {
		return v
	}
	return ""
}

func chatToolCallArgs(tc map[string]any) string {
	if v, ok := tc["arguments"].(string); ok {
		return v
	}
	return ""
}

// chatMaxTokensField 按模型名选择 max token 请求字段名。
// OpenAI 新一代模型（o1/o3/o4 系、gpt-5 系）只接受 max_completion_tokens，
// 传 max_tokens 会 400；其余端点（gpt-4o/deepseek/kimi/glm/doubao 等）认 max_tokens。
// 兼容 openrouter 风格的 "vendor/model" 前缀（取最后一段判定）。
func chatMaxTokensField(model string) string {
	m := strings.ToLower(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4") ||
		strings.HasPrefix(m, "gpt-5") {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// buildRequestJSON 构造 OpenAI 兼容的 chat/completions 请求体。
// stream=true 时附加 stream:true 与 stream_options.include_usage。
func (p *openAIChatProvider) buildRequestJSON(req *blades.ModelRequest, stream bool) ([]byte, error) {
	body := map[string]any{
		"model": p.modelName,
	}
	// temperature 仅 >0 时发送：推理模型（o 系/deepseek-reasoner）拒绝非默认温度字段。
	if p.temperature > 0 {
		body["temperature"] = p.temperature
	}
	if p.maxTokens > 0 {
		body[chatMaxTokensField(p.modelName)] = p.maxTokens
	}
	if stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	// Instruction 作为 system 前缀。
	if req.Instruction != nil {
		msgs = append(msgs, map[string]any{"role": "system", "content": chatTextOf(req.Instruction)})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, bladesMessageToChat(m))
	}
	body["messages"] = msgs

	// Tools 转换为 OpenAI function 格式（chat 格式：嵌套在 function 键下）。
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
				fn["parameters"] = chatSchemaAsMap(schema)
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

// bladesMessageToChat 把 blades.Message 转为 OpenAI chat 消息 JSON。
// 关键：assistant 消息从 Metadata["reasoning_content"] 取值回传，避免 DeepSeek V4 400。
func bladesMessageToChat(m *blades.Message) map[string]any {
	if m == nil {
		return nil
	}
	switch m.Role {
	case blades.RoleUser:
		// 携带图片 DataPart（Alt+V 粘贴 / 工具透传）时 content 用数组形式
		//（text + image_url data URL）；无图保持 string content（兼容回归）。
		if imgs := chatImageDataParts(m); len(imgs) > 0 {
			content := make([]map[string]any, 0, len(m.Parts)+len(imgs))
			if t := chatTextOf(m); t != "" {
				content = append(content, map[string]any{"type": "text", "text": t})
			}
			content = append(content, imgs...)
			return map[string]any{"role": "user", "content": content}
		}
		return map[string]any{"role": "user", "content": chatTextOf(m)}
	case blades.RoleSystem:
		return map[string]any{"role": "system", "content": chatTextOf(m)}
	case blades.RoleAssistant:
		out := map[string]any{"role": "assistant"}
		if t := chatTextOf(m); t != "" {
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
		return map[string]any{"role": "tool", "content": chatTextOf(m)}
	default:
		return map[string]any{"role": "user", "content": chatTextOf(m)}
	}
}

// chatTextOf 提取 blades.Message 中所有 TextPart 拼接为字符串。
func chatTextOf(m *blades.Message) string {
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

// chatImageDataParts 提取消息中的 DataPart 转为 OpenAI 兼容多模态 content 数组项，
// 按 MIME 主类型分流（Ark/GLM 等 OpenAI 兼容端点的视频理解同此格式）：
//   - image/* → {"type":"image_url","image_url":{"url":"data:<mime>;base64,..."}}
//   - video/* → {"type":"video_url","video_url":{"url":"data:<mime>;base64,..."}}
//     （火山方舟 doubao-seed/GLM 视频理解、智谱 GLM-4V/4.5V 的 OpenAI 兼容格式）
//   - 其他（audio 等）：跳过（宁可降级为纯文本也不误标 image_url 触发 400）。
//
// 无媒体项返回 nil，调用方保持纯文本 content。
func chatImageDataParts(m *blades.Message) []map[string]any {
	if m == nil {
		return nil
	}
	var out []map[string]any
	for _, part := range m.Parts {
		dp, ok := part.(blades.DataPart)
		if !ok || len(dp.Bytes) == 0 || dp.MIMEType == "" {
			continue
		}
		dataURL := "data:" + string(dp.MIMEType) + ";base64," + base64.StdEncoding.EncodeToString(dp.Bytes)
		major := string(dp.MIMEType)
		if i := strings.IndexByte(major, '/'); i >= 0 {
			major = major[:i]
		}
		switch major {
		case "image":
			out = append(out, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": dataURL},
			})
		case "video":
			out = append(out, map[string]any{
				"type":      "video_url",
				"video_url": map[string]any{"url": dataURL},
			})
		default:
			// audio/* 等未支持模态：跳过（历史上会被无条件误标为 image_url）。
			continue
		}
	}
	return out
}

// chatSchemaAsMap 把 jsonschema.Schema 转为 map[string]any，便于 JSON 序列化。
func chatSchemaAsMap(s *jsonschema.Schema) map[string]any {
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

// chatContent 兼容 message.content 的两种形态：
// string（绝大多数端点）与 [{type:"text",text:...}] 数组（部分多模态/代理端点）。
type chatContent struct {
	text string
}

// UnmarshalJSON 实现 string | array | null 三形态解析。
func (c *chatContent) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.text = s
		return nil
	}
	// 数组形态：拼接 text / output_text 类型的片段。
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return nil // 未知形态静默忽略，不让整条响应失败
	}
	for _, p := range parts {
		if p.Type == "text" || p.Type == "output_text" {
			c.text += p.Text
		}
	}
	return nil
}

// parseChatResponse 解析非流式响应，提取 content/tool_calls/reasoning_content/usage。
func parseChatResponse(raw []byte) (*blades.ModelResponse, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Role             string         `json:"role"`
				Content          chatContent    `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				Reasoning        string         `json:"reasoning"` // 部分端点字段名
				ToolCalls        []chatToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("openai-chat unmarshal: %w", err)
	}

	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  int64(resp.Usage.inputTokens()),
		OutputTokens: int64(resp.Usage.outputTokens()),
		TotalTokens:  int64(resp.Usage.totalTokens()),
	}
	// TODO #40 缓存可观测：DeepSeek 原生 prompt_cache_hit/miss_tokens 透传 Metadata。
	setCacheUsageMeta(msg, int64(resp.Usage.PromptCacheHitTokens), int64(resp.Usage.PromptCacheMissTokens))
	for _, choice := range resp.Choices {
		if choice.Message.Content.text != "" {
			msg.Parts = append(msg.Parts, blades.TextPart{Text: choice.Message.Content.text})
		}
		// reasoning_content 优先，空则回退 reasoning 字段。
		reasoning := choice.Message.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Message.Reasoning
		}
		if reasoning != "" {
			if msg.Metadata == nil {
				msg.Metadata = make(map[string]any)
			}
			msg.Metadata["reasoning_content"] = reasoning
		}
		if choice.FinishReason != "" {
			msg.FinishReason = choice.FinishReason
		}
		// finish_reason=length 表示响应被 max_tokens 截断：tool_calls 的 arguments
		// 可能是半截 JSON（端点/网关常自动补齐引号括号使其"合法"但内容缺失）。
		// 执行半截参数会把文件写残（实证：monster.js 730 行被截断写入 64 行）。
		// 整轮丢弃工具调用，由 ReAct 空响应/下一轮让模型重试。
		if choice.FinishReason == "length" {
			continue
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
