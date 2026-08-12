package model

import (
	"context"       // 上下文传递
	"encoding/json" // JSON 序列化/反序列化
	"fmt"           // 错误格式化
	"net/http"      // HTTP 客户端
	"net/url"       // URL 解析

	"github.com/blockmemory/agent/backend/pkg/types" // 共享配置类型
	"github.com/go-kratos/blades"                    // ModelProvider 抽象
	bladestools "github.com/go-kratos/blades/tools"  // blades 工具定义
	"github.com/google/jsonschema-go/jsonschema"     // JSON Schema 处理
	"github.com/ollama/ollama/api"                   // Ollama Go SDK
)

// ollamaProvider 基于 Ollama Go SDK 原生 /api/chat 的 provider 封装。
type ollamaProvider struct {
	client      *api.Client // Ollama SDK 客户端
	modelName   string      // 模型名称
	baseURL     string      // 服务端基础 URL
	temperature float64     // 采样温度
	maxTokens   int         // 最大输出 token 数
}

// defaultOllamaBaseURL 是 Ollama 本地默认端点。
// 推荐在 roles.yaml 的 model_config.base_url 中显式配置，避免隐式依赖。
const defaultOllamaBaseURL = "http://localhost:11434"

// newOllamaProvider 构造一个 Ollama 原生 provider。
//
// 参数：
//   - cfg: 模型配置
//
// 返回：
//   - blades.ModelProvider: 构造好的 provider
//   - error: URL 解析失败等构造错误
//
// 说明：当 cfg.BaseURL 为空时回退到 defaultOllamaBaseURL；URL 解析失败返回错误，避免静默回退导致配置错误难以排查。
func newOllamaProvider(cfg types.AgentModelConfig) (blades.ModelProvider, error) {
	// 若未配置 baseURL，使用本地默认端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}

	// 解析 URL，失败则返回错误
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse ollama base_url %q: %w", baseURL, err)
	}

	// 返回封装后的 Ollama provider
	return &ollamaProvider{
		client:      api.NewClient(u, http.DefaultClient),
		modelName:   cfg.Model,
		baseURL:     baseURL,
		temperature: cfg.Temperature,
		maxTokens:   cfg.MaxTokens,
	}, nil
}

// Name 返回 provider 使用的模型名称。
func (p *ollamaProvider) Name() string { return p.modelName }

// Generate 调用 Ollama /api/chat 完成生成。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：
//   - *blades.ModelResponse: 模型响应
//   - error: 生成或转换错误
func (p *ollamaProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 转换 blades 消息为 Ollama 消息格式
	messages := p.convertMessages(req)
	// 转换 blades 工具为 Ollama 工具格式
	tools := p.convertTools(req.Tools)

	// 关闭流式响应
	stream := false
	// 构造 Ollama Chat 请求
	chatReq := &api.ChatRequest{
		Model:    p.modelName,
		Messages: messages,
		Tools:    tools,
		Stream:   &stream,
		Options: map[string]any{
			"temperature": p.temperature,
			"num_predict": p.maxTokens,
		},
	}

	// 保存最后一条消息、结束原因与 token 用量
	var lastMsg api.Message
	var doneReason string
	var usage blades.TokenUsage
	// 调用 Ollama Chat API
	err := p.client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
		// 当响应完成时提取消息与用量
		if resp.Done {
			lastMsg = resp.Message
			doneReason = resp.DoneReason
			usage = blades.TokenUsage{
				InputTokens:  int64(resp.Metrics.PromptEvalCount),
				OutputTokens: int64(resp.Metrics.EvalCount),
				TotalTokens:  int64(resp.Metrics.PromptEvalCount + resp.Metrics.EvalCount),
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ollama chat: %w", err)
	}

	// 转换最后一条消息为 blades 格式并附加用量
	msg := p.convertMessage(lastMsg)
	// done_reason=length 表示 num_predict 截断：tool_calls 参数可能是半截 JSON，
	// 执行会把文件写残（与 openai-chat 同规则）。丢弃工具调用，文本保留。
	if doneReason == "length" {
		kept := make([]blades.Part, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if _, isTool := part.(blades.ToolPart); !isTool {
				kept = append(kept, part)
			}
		}
		msg.Parts = kept
	}
	msg.TokenUsage = usage
	return &blades.ModelResponse{Message: msg}, nil
}

// NewStreaming 创建流式生成器（当前为简化实现，非真正流式）。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：blades.Generator 流式生成器。
func (p *ollamaProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	// 返回一个只产生一次完整响应的 generator
	return func(yield func(*blades.ModelResponse, error) bool) {
		// 调用普通生成
		resp, err := p.Generate(ctx, req)
		// 将结果交给消费者，若消费者不继续则直接返回
		if !yield(resp, err) {
			return
		}
	}
}

// convertMessages 将 blades 请求转换为 Ollama 消息列表。
//
// 参数：
//   - req: blades 模型请求
//
// 返回：Ollama 消息切片。
func (p *ollamaProvider) convertMessages(req *blades.ModelRequest) []api.Message {
	// 初始化空消息切片
	var messages []api.Message

	// 若存在 system instruction，先追加 system 消息
	if req.Instruction != nil {
		text := messageText(req.Instruction)
		if text != "" {
			messages = append(messages, api.Message{Role: "system", Content: text})
		}
	}

	// 追加普通消息
	for _, m := range req.Messages {
		// 跳过 nil 消息
		if m == nil {
			continue
		}
		messages = append(messages, p.convertBladesMessage(m))
	}

	return messages
}

// convertBladesMessage 将单条 blades.Message 转换为 Ollama api.Message。
//
// 参数：
//   - m: blades 消息
//
// 返回：Ollama 消息。
func (p *ollamaProvider) convertBladesMessage(m *blades.Message) api.Message {
	// 根据 blades 角色映射到 Ollama 角色
	var role string
	switch m.Role {
	case blades.RoleUser, blades.RoleTool:
		role = "user"
	case blades.RoleAssistant:
		role = "assistant"
	case blades.RoleSystem:
		role = "system"
	default:
		role = "user"
	}

	// 聚合文本内容与工具调用
	var content string
	var toolCalls []api.ToolCall

	// 遍历所有 part
	for _, part := range m.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			// 多个文本 part 用换行拼接
			if content != "" {
				content += "\n"
			}
			content += v.Text
		case blades.ToolPart:
			if v.Response != "" {
				// 工具结果，以文本形式拼接到 user message
				if content != "" {
					content += "\n"
				}
				content += fmt.Sprintf("[%s result]: %s", v.Name, v.Response)
			} else if m.Role == blades.RoleAssistant {
				// assistant 发起的工具调用
				args, err := toolArgsToMap(v.Request)
				if err != nil {
					// 解析失败时使用空对象
					args = map[string]any{}
				}
				toolCalls = append(toolCalls, api.ToolCall{
					Function: api.ToolCallFunction{
						Name:      v.Name,
						Arguments: args,
					},
				})
			}
		}
	}

	return api.Message{
		Role:      role,
		Content:   content,
		ToolCalls: toolCalls,
	}
}

// convertMessage 将 Ollama api.Message 转换为 blades.Message。
//
// 参数：
//   - m: Ollama 消息
//
// 返回：blades 消息。
func (p *ollamaProvider) convertMessage(m api.Message) *blades.Message {
	// 创建 assistant 完成消息
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	// 初始化空 parts
	var parts []blades.Part

	// 若文本内容非空，追加文本 part
	if m.Content != "" {
		parts = append(parts, blades.TextPart{Text: m.Content})
	}

	// 追加工具调用 part
	for _, tc := range m.ToolCalls {
		// 将参数 map 序列化为 JSON 字符串
		reqJSON, _ := json.Marshal(tc.Function.Arguments)
		parts = append(parts, blades.NewToolPart("", tc.Function.Name, string(reqJSON)))
	}

	// 设置 parts
	msg.Parts = parts
	return msg
}

// convertTools 将 blades 工具列表转换为 Ollama 工具列表。
//
// 参数：
//   - tools: blades 工具切片
//
// 返回：Ollama 工具切片。
func (p *ollamaProvider) convertTools(tools []bladestools.Tool) []api.Tool {
	// 预分配等长切片
	out := make([]api.Tool, 0, len(tools))
	// 逐个转换
	for _, t := range tools {
		tool, err := toolToOllamaTool(t)
		if err != nil {
			// 单个工具转换失败则跳过，避免整体失败
			continue
		}
		out = append(out, tool)
	}
	return out
}

// toolToOllamaTool 将单个 blades 工具转换为 Ollama api.Tool。
//
// 参数：
//   - t: blades 工具
//
// 返回：
//   - api.Tool: Ollama 工具
//   - error: JSON 序列化/反序列化错误
func toolToOllamaTool(t bladestools.Tool) (api.Tool, error) {
	// 获取工具输入 schema
	schema := t.InputSchema()
	// 构造 properties map
	properties := make(map[string]any)
	for name, prop := range schema.Properties {
		propMap := map[string]any{
			"type":        schemaType(prop),
			"description": prop.Description,
		}
		// 若存在枚举值，加入 schema
		if enum := anySliceToStrings(prop.Enum); len(enum) > 0 {
			propMap["enum"] = enum
		}
		properties[name] = propMap
	}

	// 序列化为 JSON 再反序列化到 api.Tool
	raw, err := json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters": map[string]any{
				"type":       schemaType(schema),
				"required":   schema.Required,
				"properties": properties,
			},
		},
	})
	if err != nil {
		return api.Tool{}, err
	}

	var tool api.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		return api.Tool{}, err
	}
	return tool, nil
}

// schemaType 从 JSON Schema 中提取类型字符串。
//
// 参数：
//   - s: JSON Schema 指针
//
// 返回：类型字符串，默认 "object"。
func schemaType(s *jsonschema.Schema) string {
	// nil schema 默认返回 object
	if s == nil {
		return "object"
	}
	// 优先使用 Type 字段
	if s.Type != "" {
		return s.Type
	}
	// 其次使用 Types 切片首个元素
	if len(s.Types) > 0 {
		return s.Types[0]
	}
	return "object"
}

// messageText 从 blades.Message 中提取所有文本内容并拼接。
//
// 参数：
//   - m: blades 消息
//
// 返回：拼接后的文本字符串。
func messageText(m *blades.Message) string {
	var text string
	// 遍历所有 part
	for _, part := range m.Parts {
		// 仅处理文本 part
		if t, ok := part.(blades.TextPart); ok {
			// 多个文本 part 用换行拼接
			if text != "" {
				text += "\n"
			}
			text += t.Text
		}
	}
	return text
}

// toolArgsToMap 将工具参数字符串解析为 map[string]any。
//
// 参数：
//   - raw: JSON 格式的参数字符串
//
// 返回：
//   - map[string]any: 解析后的参数
//   - error: JSON 解析错误
func toolArgsToMap(raw string) (map[string]any, error) {
	// 空字符串返回空对象
	if raw == "" {
		return map[string]any{}, nil
	}
	// 解析 JSON
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// anySliceToStrings 将 []any 中的字符串元素提取为 []string。
//
// 参数：
//   - v: 原始 []any 切片
//
// 返回：仅包含字符串元素的新切片。
func anySliceToStrings(v []any) []string {
	// 预分配等长切片
	out := make([]string, 0, len(v))
	// 遍历并过滤字符串
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
