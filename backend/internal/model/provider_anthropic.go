package model

import (
	"context"       // 上下文传递
	"encoding/json" // JSON 序列化
	"fmt"           // 错误格式化
	"strings"       // 字符串拼接

	"github.com/anthropics/anthropic-sdk-go"         // Anthropic Go SDK
	"github.com/anthropics/anthropic-sdk-go/option"  // Anthropic 客户端选项
	"github.com/blockmemory/agent/backend/pkg/types" // 共享配置类型
	"github.com/go-kratos/blades"                    // ModelProvider 抽象
	bladestools "github.com/go-kratos/blades/tools"  // blades 工具定义
	"github.com/google/jsonschema-go/jsonschema"     // JSON Schema 处理
)

// anthropicProvider 基于 Anthropic Go SDK 原生 Messages API 的 provider 封装。
type anthropicProvider struct {
	client      anthropic.Client // Anthropic SDK 客户端
	modelName   string           // 模型名称
	maxTokens   int64            // 最大输出 token 数
	temperature float64          // 采样温度
	baseURL     string           // .env 配置的完整端点（实际请求 URL）
}

// newAnthropicProvider 构造一个 Anthropic 原生 provider。
//
// 参数：
//   - cfg: 模型配置
//
// 返回：blades.ModelProvider 实例。
func newAnthropicProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	// 若未配置 baseURL，使用 Anthropic 官方默认端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	// 解析 MaxTokens，未配置或非法时回退到 4096
	maxTokens := int64(cfg.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// 创建 Anthropic 客户端并封装为 provider
	return &anthropicProvider{
		client: anthropic.NewClient(
			option.WithAPIKey(cfg.APIKey),
			option.WithBaseURL(baseURL),
		),
		modelName:   cfg.Model,
		maxTokens:   maxTokens,
		temperature: cfg.Temperature,
		baseURL:     baseURL,
	}
}

// Name 返回 provider 使用的模型名称。
func (p *anthropicProvider) Name() string { return p.modelName }

// Generate 调用 Anthropic Messages API 完成生成。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：
//   - *blades.ModelResponse: 模型响应
//   - error: 生成或转换错误
func (p *anthropicProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 转换 messages
	messages, err := p.convertMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}

	// 构造 Anthropic 请求参数
	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(p.modelName),
		MaxTokens:   p.maxTokens,
		Messages:    messages,
		System:      p.convertSystem(req.Instruction),
		Tools:       p.convertTools(req.Tools),
		Temperature: anthropic.Float(p.temperature),
	}

	// 调用 Anthropic Messages API
	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages POST %s: %w", p.baseURL, err)
	}

	// 转换响应为 blades 格式
	msg := p.convertResponse(resp)
	return &blades.ModelResponse{Message: msg}, nil
}

// NewStreaming 创建真正的 SSE 流式生成器：
// 中间产出增量文本块（驱动 UI 逐 token 渲染），最后一个产出值是累积完整的响应。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：blades.Generator 流式生成器。
func (p *anthropicProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		// 转换 messages
		messages, err := p.convertMessages(req.Messages)
		if err != nil {
			yield(nil, fmt.Errorf("convert messages: %w", err))
			return
		}

		// 构造与 Generate 一致的请求参数
		params := anthropic.MessageNewParams{
			Model:       anthropic.Model(p.modelName),
			MaxTokens:   p.maxTokens,
			Messages:    messages,
			System:      p.convertSystem(req.Instruction),
			Tools:       p.convertTools(req.Tools),
			Temperature: anthropic.Float(p.temperature),
		}

		stream := p.client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		// 累积状态：全文文本、思考过程文本、按块序累积的工具调用、token 用量与结束原因。
		var text strings.Builder
		var thinking strings.Builder
		type toolAcc struct {
			id, name, input string
		}
		var tools []toolAcc
		var inputTokens, outputTokens int64
		stopReason := ""

		for stream.Next() {
			event := stream.Current()
			switch ev := event.AsAny().(type) {
			case anthropic.MessageStartEvent:
				inputTokens = ev.Message.Usage.InputTokens
			case anthropic.ContentBlockStartEvent:
				// 工具调用块开始：记录 id/name，后续 input_json_delta 累积入参。
				if ev.ContentBlock.Type == "tool_use" {
					tools = append(tools, toolAcc{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name})
				}
			case anthropic.ContentBlockDeltaEvent:
				switch ev.Delta.Type {
				case "text_delta":
					// 文本增量：累积并产出增量块（消费方停止则终止流）。
					text.WriteString(ev.Delta.Text)
					msg := blades.NewAssistantMessage(blades.StatusCompleted)
					msg.Parts = []blades.Part{blades.TextPart{Text: ev.Delta.Text}}
					if !yield(&blades.ModelResponse{Message: msg}, nil) {
						return
					}
				case "thinking_delta":
					// 思考过程增量：累积并产出携带累积思考文本的中间块（无文本 part），
					// 消费方据此实时展示思考过程；思考内容不进入答复文本。
					thinking.WriteString(ev.Delta.Thinking)
					msg := blades.NewAssistantMessage(blades.StatusInProgress)
					msg.Metadata = map[string]any{"thinking": thinking.String()}
					if !yield(&blades.ModelResponse{Message: msg}, nil) {
						return
					}
				case "input_json_delta":
					// 工具入参增量：追加到最近开始的工具调用块。
					if len(tools) > 0 {
						tools[len(tools)-1].input += ev.Delta.PartialJSON
					}
				}
			case anthropic.MessageDeltaEvent:
				stopReason = string(ev.Delta.StopReason)
				outputTokens = ev.Usage.OutputTokens
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, fmt.Errorf("anthropic messages stream POST %s: %w", p.baseURL, err))
			return
		}

		// 产出累积完整的最终响应（文本 + 工具调用 + 用量 + 结束原因）。
		msg := blades.NewAssistantMessage(blades.StatusCompleted)
		parts := make([]blades.Part, 0, len(tools)+1)
		if text.Len() > 0 {
			parts = append(parts, blades.TextPart{Text: text.String()})
		}
		for _, t := range tools {
			parts = append(parts, blades.NewToolPart(t.id, t.name, t.input))
		}
		msg.Parts = parts
		msg.TokenUsage = blades.TokenUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		}
		msg.FinishReason = stopReason
		// 思考过程经 Metadata 传递（blades 无对应 Part 类型），截断防止超长。
		if thinking.Len() > 0 {
			msg.Metadata = map[string]any{"thinking": truncateThinking(thinking.String())}
		}
		yield(&blades.ModelResponse{Message: msg}, nil)
	}
}

// thinkingMaxRunes 限制单次响应携带的思考过程文本长度，防止超长思考挤占展示与存储。
const thinkingMaxRunes = 4000

// truncateThinking 按 rune 数截断思考过程文本并追加省略提示。
func truncateThinking(s string) string {
	runes := []rune(s)
	if len(runes) <= thinkingMaxRunes {
		return s
	}
	return string(runes[:thinkingMaxRunes]) + "...(truncated)"
}

// convertSystem 将 blades 的 Instruction 消息转换为 Anthropic system blocks。
//
// 参数：
//   - inst: blades system 消息
//
// 返回：Anthropic system text blocks 切片。
func (p *anthropicProvider) convertSystem(inst *blades.Message) []anthropic.TextBlockParam {
	// 空 system 消息直接返回 nil
	if inst == nil {
		return nil
	}
	// 收集所有文本 part
	var blocks []anthropic.TextBlockParam
	for _, part := range inst.Parts {
		// 仅处理文本 part
		text, ok := part.(blades.TextPart)
		if !ok {
			continue
		}
		blocks = append(blocks, anthropic.TextBlockParam{Text: text.Text})
	}
	return blocks
}

// convertMessages 将 blades 消息列表转换为 Anthropic 消息参数列表。
//
// 参数：
//   - messages: blades 消息切片
//
// 返回：
//   - []anthropic.MessageParam: Anthropic 消息参数
//   - error: 转换错误
func (p *anthropicProvider) convertMessages(messages []*blades.Message) ([]anthropic.MessageParam, error) {
	// 预分配等长切片
	out := make([]anthropic.MessageParam, 0, len(messages))
	// 逐条转换
	for i := 0; i < len(messages); i++ {
		// 跳过 nil 消息
		if messages[i] == nil {
			continue
		}
		// Anthropic 协议要求：若上一条 assistant 含 N 个 tool_use，下一条 user 消息
		// 必须包含全部 N 个 tool_result。我们的 ReactMessage 把每个 tool 结果拆成
		// 独立 RoleTool 消息；不合并会导致 Ark/Anthropic 端点 400 InvalidParameter。
		// 这里把连续的 RoleTool 消息合并为单条 user 消息，content 含全部 ToolResultBlock。
		if messages[i].Role == blades.RoleTool {
			merged := anthropic.MessageParam{Role: anthropic.MessageParamRoleUser}
			for i < len(messages) && messages[i] != nil && messages[i].Role == blades.RoleTool {
				for _, part := range messages[i].Parts {
					tp, ok := part.(blades.ToolPart)
					if !ok {
						continue
					}
					if tp.Response == "" {
						continue
					}
					merged.Content = append(merged.Content, anthropic.ContentBlockParamUnion{
						OfToolResult: &anthropic.ToolResultBlockParam{
							ToolUseID: tp.ID,
							Content: []anthropic.ToolResultBlockParamContentUnion{
								{OfText: &anthropic.TextBlockParam{Text: tp.Response}},
							},
						},
					})
				}
				i++
			}
			// 回退一位，外层 for 会再 +1，确保下一条非 RoleTool 消息正常处理。
			i--
			if len(merged.Content) > 0 {
				out = append(out, merged)
			}
			continue
		}
		param, err := p.convertMessage(messages[i])
		if err != nil {
			return nil, err
		}
		out = append(out, param)
	}
	return out, nil
}

// convertMessage 将单条 blades.Message 转换为 Anthropic MessageParam。
//
// 参数：
//   - m: blades 消息
//
// 返回：
//   - anthropic.MessageParam: Anthropic 消息参数
//   - error: 转换错误（当前不会返回错误，保留签名以兼容未来扩展）
func (p *anthropicProvider) convertMessage(m *blades.Message) (anthropic.MessageParam, error) {
	// 根据 blades 角色映射到 Anthropic 角色
	var role anthropic.MessageParamRole
	switch m.Role {
	case blades.RoleUser, blades.RoleTool:
		role = anthropic.MessageParamRoleUser
	case blades.RoleAssistant:
		role = anthropic.MessageParamRoleAssistant
	case blades.RoleSystem:
		role = anthropic.MessageParamRoleUser
	default:
		role = anthropic.MessageParamRoleUser
	}

	// 预分配 content blocks
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts))
	// 逐个 part 转换
	for _, part := range m.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			// 文本块直接追加
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: v.Text},
			})
		case blades.ToolPart:
			if v.Response != "" {
				// 工具结果块，必须以 user message 形式返回给模型
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: v.ID,
						Content: []anthropic.ToolResultBlockParamContentUnion{
							{OfText: &anthropic.TextBlockParam{Text: v.Response}},
						},
					},
				})
			} else if m.Role == blades.RoleAssistant {
				// assistant 发起的 tool_use 调用
				input := json.RawMessage(v.Request)
				// 若请求为空，使用空对象占位
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    v.ID,
						Name:  v.Name,
						Input: input,
					},
				})
			}
		default:
			// 忽略不支持的 part 类型
		}
	}

	return anthropic.MessageParam{Role: role, Content: blocks}, nil
}

// convertTools 将 blades 工具列表转换为 Anthropic 工具参数列表。
//
// 参数：
//   - tools: blades 工具切片
//
// 返回：Anthropic 工具参数切片。
func (p *anthropicProvider) convertTools(tools []bladestools.Tool) []anthropic.ToolUnionParam {
	// 无工具时返回 nil，避免序列化为 "tools": [] 触发端点 InvalidParameter
	if len(tools) == 0 {
		return nil
	}
	// 预分配等长切片
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	// 逐个工具转换
	for _, t := range tools {
		// 将 JSON Schema 转为 map
		schemaMap, _ := schemaToMap(t.InputSchema())
		// ToolInputSchemaParam.Properties 只接受内部 properties 映射，
		// 传入整个 schema 会导致 input_schema 结构错乱（嵌套一层 type/properties/required），
		// 触发 Ark 端点 400 InvalidParameter。
		var properties any
		if schemaMap != nil {
			if props, ok := schemaMap["properties"]; ok {
				properties = props
			} else {
				properties = map[string]any{}
			}
		} else {
			properties = map[string]any{}
		}
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name(),
				Description: anthropic.String(t.Description()),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: properties,
					Required:   t.InputSchema().Required,
				},
			},
		})
	}
	return out
}

// convertResponse 将 Anthropic 响应转换为 blades.Message。
//
// 参数：
//   - resp: Anthropic Messages API 响应
//
// 返回：blades 消息。
func (p *anthropicProvider) convertResponse(resp *anthropic.Message) *blades.Message {
	// 创建 assistant 完成消息
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	// 预分配 parts
	parts := make([]blades.Part, 0, len(resp.Content))

	// 遍历响应内容块
	var thinking strings.Builder
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			// 文本块
			parts = append(parts, blades.TextPart{Text: v.Text})
		case anthropic.ToolUseBlock:
			// 工具调用块
			input := string(v.Input)
			parts = append(parts, blades.NewToolPart(v.ID, v.Name, input))
		case anthropic.ThinkingBlock:
			// 思考过程块：累积后经 Metadata 传递（blades 无对应 Part 类型）
			thinking.WriteString(v.Thinking)
		}
	}

	// 设置消息 parts
	msg.Parts = parts
	// 思考过程截断后放入 Metadata，供上层作为"思考过程"事件展示。
	if thinking.Len() > 0 {
		msg.Metadata = map[string]any{"thinking": truncateThinking(thinking.String())}
	}
	// 设置 token 用量
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}
	// 设置完成原因
	msg.FinishReason = string(resp.StopReason)
	return msg
}

// schemaToMap 将 jsonschema.Schema 序列化为 map[string]any。
//
// 参数：
//   - s: JSON Schema 指针
//
// 返回：
//   - map[string]any: 转换后的 map
//   - error: 序列化/反序列化错误
func schemaToMap(s *jsonschema.Schema) (map[string]any, error) {
	// nil schema 直接返回 nil
	if s == nil {
		return nil, nil
	}
	// 序列化为 JSON
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	// 反序列化为 map
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	// jsonschema-go 在序列化时去掉了 Type/Types 字段（json:"-"），
	// 手动补回顶层 type，方便 Anthropic SDK 生成合法的 input_schema。
	if s.Type != "" {
		m["type"] = s.Type
	} else if len(s.Types) > 0 {
		m["type"] = s.Types[0]
	}
	return m, nil
}
