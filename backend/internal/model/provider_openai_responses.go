package model

// provider_openai_responses.go 实现 OpenAI Responses API 端点（POST {base}/responses）
// 的自研 provider，provider 名：openai-responses。
//
// Responses API 是 OpenAI 新一代接口（gpt-4.1/o 系/gpt-5 主推），与 Chat Completions 的差异：
//   - 请求：input[] 项列表（message/function_call/function_call_output）替代 messages[]；
//     system 指令走 instructions 字段；max token 字段为 max_output_tokens；
//     tools 扁平化（function 字段直接平铺，不再嵌套 function 键）；
//   - 响应：output[] 项列表（message.content[].output_text / function_call / reasoning），
//     usage 字段为 input_tokens/output_tokens（非 prompt_tokens/completion_tokens）；
//   - 流式：事件流（response.output_text.delta / response.output_item.done /
//     response.completed 等），非 chat 的 choices[].delta 增量。
//
// 字段兼容策略：
//   - max_output_tokens 下限 16：OpenAI 要求 >=16，启动连通性探测用 MaxTokens=1 会 400，
//     这里自动钳制到 16；
//   - temperature 仅在 >0 时发送（推理模型拒绝非默认温度）；
//   - usage：total_tokens 缺失时用 input+output 合计；reasoning tokens 在
//     output_tokens_details.reasoning_tokens，cached 在 input_tokens_details.cached_tokens；
//   - reasoning：reasoning 项的 summary 文本捕获进 Metadata["reasoning_content"] 供展示；
//     不回传 reasoning 项（跨轮状态管理超出本层职责；推理模型 + 多轮 function calling
//     在严格端点上可能要求回传，遇此场景请改用 openai-chat 或官方 SDK）；
//   - 流式事件宽松解析：只识别关心的事件类型，未知事件跳过，兼容第三方实现的扩展事件。
//
// DeepSeek Responses API 兼容性（https://api-docs.deepseek.com/zh-cn/guides/responses_api）：
//   - base_url https://api.deepseek.com，endpoint {base}/responses，与 OpenAI 同形；
//   - 已兼容：model/input/instructions/stream/temperature/max_output_tokens/tools/tool_choice；
//   - DeepSeek 不支持且本 provider 也不发送：previous_response_id/store/metadata/include/
//     truncation/service_tier/stream_options/prompt_cache_key/context_management/background；
//   - DeepSeek 不发 data: [DONE]，流以 response.completed/incomplete/failed 结束；
//     本 provider 的 [DONE] break 对 DeepSeek 无害（永不触发），靠 scanner EOF 收尾；
//   - DeepSeek reasoning 输入项仅支持明文 content（summary/encrypted_content 不支持），
//     本 provider 不回传 reasoning 项，与 DeepSeek 兼容；
//   - 流式 function_call 兜底：DeepSeek 在 response.completed 事件的 response.output[]
//     携带完整 function_call 项；若 output_item.done 事件漏发，从 completed.output[] 提取兜底。

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
)

// responsesMinMaxOutputTokens 是 OpenAI Responses API 的 max_output_tokens 下限。
// 低于 16 会 400 "max_output_tokens must be at least 16"；探测调用（MaxTokens=1）依赖此钳制。
const responsesMinMaxOutputTokens = 16

// openAIResponsesProvider 直连 OpenAI Responses API 端点的 provider。
type openAIResponsesProvider struct {
	baseURL     string
	apiKey      string
	modelName   string
	temperature float64
	maxTokens   int
	httpClient  *http.Client
}

// newOpenAIResponsesProvider 构造一个 /responses 格式的 provider。
// baseURL 为空时默认官方端点 https://api.openai.com/v1。
func newOpenAIResponsesProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &openAIResponsesProvider{
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
func (p *openAIResponsesProvider) Name() string { return p.modelName }

// Generate 执行非流式 Responses 请求。
func (p *openAIResponsesProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	body, err := p.buildRequestJSON(req, false)
	if err != nil {
		return nil, fmt.Errorf("openai-responses build request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai-responses new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-responses do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai-responses read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-responses %d: %s", resp.StatusCode, string(raw))
	}
	return parseResponsesAPIResponse(raw)
}

// NewStreaming 执行流式 Responses 请求，逐事件产出累积响应；最后一块为完整响应。
func (p *openAIResponsesProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		body, err := p.buildRequestJSON(req, true)
		if err != nil {
			yield(nil, fmt.Errorf("openai-responses build request: %w", err))
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			yield(nil, fmt.Errorf("openai-responses new request: %w", err))
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		resp, err := p.httpClient.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("openai-responses do: %w", err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			yield(nil, fmt.Errorf("openai-responses stream %d: %s", resp.StatusCode, string(raw)))
			return
		}

		// 累积器：文本 / reasoning / function_call / usage / 完成状态
		var contentBuf strings.Builder
		var reasoningBuf strings.Builder
		var toolCalls []responsesFunctionCall
		var usage responsesUsage
		var finishReason string

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
			var ev responsesStreamEvent
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "response.output_text.delta":
				contentBuf.WriteString(ev.Delta)
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				reasoningBuf.WriteString(ev.Delta)
			case "response.output_item.done":
				// function_call 项完成时携带完整 call_id/name/arguments。
				if ev.Item != nil && ev.Item.Type == "function_call" {
					toolCalls = append(toolCalls, responsesFunctionCall{
						CallID:    ev.Item.CallID,
						Name:      ev.Item.Name,
						Arguments: ev.Item.Arguments,
					})
				}
			case "response.completed", "response.incomplete":
				if ev.Response != nil {
					usage = ev.Response.Usage
					// 兜底：若 output_item.done 事件漏发，从 completed 事件的 output[] 提取 function_call。
					if len(toolCalls) == 0 {
						for _, item := range ev.Response.Output {
							if item.Type == "function_call" {
								toolCalls = append(toolCalls, responsesFunctionCall{
									CallID:    item.CallID,
									Name:      item.Name,
									Arguments: item.Arguments,
								})
							}
						}
					}
					finishReason = responsesFinishReason(ev.Response, len(toolCalls) > 0)
				}
			case "response.failed":
				if ev.Response != nil && ev.Response.Error != nil {
					yield(nil, fmt.Errorf("openai-responses stream failed: %s: %s", ev.Response.Error.Code, ev.Response.Error.Message))
					return
				}
				yield(nil, fmt.Errorf("openai-responses stream failed"))
				return
			case "error":
				if ev.Error != nil {
					yield(nil, fmt.Errorf("openai-responses stream error: %s: %s", ev.Error.Code, ev.Error.Message))
				} else {
					yield(nil, fmt.Errorf("openai-responses stream error"))
				}
				return
			}
			// 其余事件（response.created/output_item.added/content_part.delta 等）忽略。

			// 推送累积响应，UI 据此流式渲染。
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
			yield(nil, fmt.Errorf("openai-responses scan: %w", err))
			return
		}

		yield(buildResponsesFinalMessage(contentBuf.String(), reasoningBuf.String(), toolCalls, usage, finishReason), nil)
	}
}

// responsesFunctionCall 是 Responses API function_call 输出项的关键字段。
type responsesFunctionCall struct {
	CallID    string
	Name      string
	Arguments string
}

// responsesUsage 是 Responses API 的 usage 字段（与 chat 的 prompt/completion 命名不同）。
type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	// 兼容个别代理端点沿用 chat 命名。
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// input 返回真实输入 token 数：input_tokens 优先，回退 prompt_tokens（代理端点）。
func (u responsesUsage) input() int {
	if u.InputTokens > 0 {
		return u.InputTokens
	}
	return u.PromptTokens
}

// output 返回真实输出 token 数：output_tokens 优先，回退 completion_tokens / reasoning 明细。
func (u responsesUsage) output() int {
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	if u.OutputTokensDetails != nil {
		return u.OutputTokensDetails.ReasoningTokens
	}
	return 0
}

// total 返回总 token 数：total_tokens 优先，为 0 时用 input+output 合计。
func (u responsesUsage) total() int {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.input() + u.output()
}

// responsesOutputItem 是 Responses API output[] 项的联合结构（按 type 判别）。
type responsesOutputItem struct {
	Type string `json:"type"`
	// message 项
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// function_call 项
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// reasoning 项
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

// responsesAPIError 是 Responses API 的错误对象。
type responsesAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responsesAPIResponseBody 是 Responses API 非流式响应体（也是 response.completed 事件的 response 字段）。
type responsesAPIResponseBody struct {
	ID     string                `json:"id"`
	Status string                `json:"status"`
	Error  *responsesAPIError    `json:"error"`
	Output []responsesOutputItem `json:"output"`
	Usage  responsesUsage        `json:"usage"`
	// IncompleteDetails 非完成态原因（如 max_output_tokens）。
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// responsesStreamEvent 是 Responses API 流式事件的宽松结构：
// 只声明关心的事件字段，未知字段/事件类型忽略，兼容第三方扩展。
type responsesStreamEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"` // *.delta 事件的增量文本
	// output_item.done 等事件的完整项。
	Item *responsesOutputItem `json:"item"`
	// completed/incomplete/failed 事件的完整响应对象。
	Response *responsesAPIResponseBody `json:"response"`
	// error 事件的错误对象。
	Error *responsesAPIError `json:"error"`
}

// responsesFinishReason 由响应状态推导 finish_reason（仅用于日志展示与截断判定）：
// incomplete（max_output_tokens）优先判为 length —— 即使 output 里带 function_call，
// 其参数也可能是截断的半截 JSON（实证 monster.js 被写残）；其次 tool_calls / stop。
func responsesFinishReason(r *responsesAPIResponseBody, hasToolCalls bool) string {
	if r == nil {
		if hasToolCalls {
			return "tool_calls"
		}
		return ""
	}
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
		if r.IncompleteDetails.Reason == "max_output_tokens" {
			return "length"
		}
		return r.IncompleteDetails.Reason
	}
	if hasToolCalls {
		return "tool_calls"
	}
	if r.Status == "completed" {
		return "stop"
	}
	return r.Status
}

// buildResponsesFinalMessage 组装完整响应消息（流式末块与非流式解析共用）。
// finishReason="length"（incomplete + max_output_tokens）时丢弃 function_call：
// 参数为流中断时累积的半截 JSON，执行会把文件写残（与 openai-chat 同规则）。
func buildResponsesFinalMessage(content, reasoning string, toolCalls []responsesFunctionCall, usage responsesUsage, finishReason string) *blades.ModelResponse {
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	if content != "" {
		msg.Parts = append(msg.Parts, blades.TextPart{Text: content})
	}
	if finishReason != "length" {
		for _, tc := range toolCalls {
			msg.Role = blades.RoleTool
			msg.Parts = append(msg.Parts, blades.ToolPart{
				ID:      tc.CallID,
				Name:    tc.Name,
				Request: tc.Arguments,
			})
		}
	}
	if reasoning != "" {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["reasoning_content"] = reasoning
	}
	msg.FinishReason = finishReason
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  int64(usage.input()),
		OutputTokens: int64(usage.output()),
		TotalTokens:  int64(usage.total()),
	}
	// TODO #40 缓存可观测：cached_tokens（input_tokens_details）作 hit，其余输入作 miss。
	if usage.InputTokensDetails != nil {
		cached := int64(usage.InputTokensDetails.CachedTokens)
		setCacheUsageMeta(msg, cached, int64(usage.input())-cached)
	}
	return &blades.ModelResponse{Message: msg}
}

// parseResponsesAPIResponse 解析非流式响应：output[] 项提取文本/function_call/reasoning。
func parseResponsesAPIResponse(raw []byte) (*blades.ModelResponse, error) {
	var body responsesAPIResponseBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("openai-responses unmarshal: %w", err)
	}
	if body.Status == "failed" {
		if body.Error != nil {
			return nil, fmt.Errorf("openai-responses failed: %s: %s", body.Error.Code, body.Error.Message)
		}
		return nil, fmt.Errorf("openai-responses failed")
	}

	var contentBuf strings.Builder
	var reasoningBuf strings.Builder
	var toolCalls []responsesFunctionCall
	for _, item := range body.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" || c.Type == "text" {
					contentBuf.WriteString(c.Text)
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, responsesFunctionCall{
				CallID:    item.CallID,
				Name:      item.Name,
				Arguments: item.Arguments,
			})
		case "reasoning":
			for _, s := range item.Summary {
				reasoningBuf.WriteString(s.Text)
			}
		}
	}
	return buildResponsesFinalMessage(contentBuf.String(), reasoningBuf.String(), toolCalls, body.Usage, responsesFinishReason(&body, len(toolCalls) > 0)), nil
}

// buildRequestJSON 构造 Responses API 请求体。
// input[] 项转换规则：
//   - Instruction → instructions 字段（顶层 system 指令）；
//   - user/system 消息 → {type:"message", role, content:[input_text]}；
//   - assistant 文本 → {type:"message", role:"assistant", content:[output_text]}；
//   - assistant 工具调用 → {type:"function_call", call_id, name, arguments}；
//   - tool 结果 → {type:"function_call_output", call_id, output}。
func (p *openAIResponsesProvider) buildRequestJSON(req *blades.ModelRequest, stream bool) ([]byte, error) {
	body := map[string]any{
		"model": p.modelName,
	}
	// temperature 仅 >0 时发送：推理模型拒绝非默认温度字段。
	if p.temperature > 0 {
		body["temperature"] = p.temperature
	}
	if p.maxTokens > 0 {
		mt := p.maxTokens
		if mt < responsesMinMaxOutputTokens {
			mt = responsesMinMaxOutputTokens
		}
		body["max_output_tokens"] = mt
	}
	if stream {
		body["stream"] = true
	}

	if req.Instruction != nil {
		if s := chatTextOf(req.Instruction); s != "" {
			body["instructions"] = s
		}
	}

	input := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		input = append(input, bladesMessageToResponsesInput(m)...)
	}
	body["input"] = input

	// Tools 转换为 Responses 扁平 function 格式（区别于 chat 的嵌套 function 键）。
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"type": "function",
				"name": t.Name(),
			}
			if d := t.Description(); d != "" {
				tool["description"] = d
			}
			if schema := t.InputSchema(); schema != nil {
				tool["parameters"] = chatSchemaAsMap(schema)
			}
			tools = append(tools, tool)
		}
		body["tools"] = tools
	}

	return json.Marshal(body)
}

// bladesMessageToResponsesInput 把一条 blades.Message 转为 Responses API input 项（可能多项：
// assistant 的文本与每个 function_call 各为一项）。
func bladesMessageToResponsesInput(m *blades.Message) []map[string]any {
	if m == nil {
		return nil
	}
	textPart := func(typ, text string) map[string]any {
		return map[string]any{"type": typ, "text": text}
	}
	switch m.Role {
	case blades.RoleUser:
		return []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{textPart("input_text", chatTextOf(m))},
		}}
	case blades.RoleSystem:
		// Responses API 接受 system/developer 角色消息（内部按 developer 处理）。
		return []map[string]any{{
			"type": "message", "role": "system",
			"content": []map[string]any{textPart("input_text", chatTextOf(m))},
		}}
	case blades.RoleAssistant:
		var out []map[string]any
		if t := chatTextOf(m); t != "" {
			out = append(out, map[string]any{
				"type": "message", "role": "assistant",
				"content": []map[string]any{textPart("output_text", t)},
			})
		}
		for _, part := range m.Parts {
			if tp, ok := part.(blades.ToolPart); ok && tp.Name != "" {
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   tp.ID,
					"name":      tp.Name,
					"arguments": tp.Request,
					"status":    "completed",
				})
			}
		}
		return out
	case blades.RoleTool:
		// tool 结果 → function_call_output。
		for _, part := range m.Parts {
			if tp, ok := part.(blades.ToolPart); ok {
				return []map[string]any{{
					"type":    "function_call_output",
					"call_id": tp.ID,
					"output":  tp.Response,
				}}
			}
		}
		// 无 ToolPart 的兜底：降级为 user 文本，避免静默丢内容。
		return []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{textPart("input_text", chatTextOf(m))},
		}}
	default:
		return []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{textPart("input_text", chatTextOf(m))},
		}}
	}
}
