package model

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
)

// anthropicProvider 基于 Anthropic Go SDK 原生 Messages API 的 provider 封装。
type anthropicProvider struct {
	client      anthropic.Client
	modelName   string
	maxTokens   int64
	temperature float64
}

// newAnthropicProvider 构造一个 Anthropic 原生 provider。
func newAnthropicProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	maxTokens := int64(cfg.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 4096
	}

	return &anthropicProvider{
		client:      anthropic.NewClient(option.WithAPIKey(cfg.APIKey), option.WithBaseURL(baseURL)),
		modelName:   cfg.Model,
		maxTokens:   maxTokens,
		temperature: cfg.Temperature,
	}
}

func (p *anthropicProvider) Name() string { return p.modelName }

func (p *anthropicProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	messages, err := p.convertMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("convert messages: %w", err)
	}

	params := anthropic.MessageNewParams{
		Model:       anthropic.Model(p.modelName),
		MaxTokens:   p.maxTokens,
		Messages:    messages,
		System:      p.convertSystem(req.Instruction),
		Tools:       p.convertTools(req.Tools),
		Temperature: anthropic.Float(p.temperature),
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages: %w", err)
	}

	msg := p.convertResponse(resp)
	return &blades.ModelResponse{Message: msg}, nil
}

func (p *anthropicProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		resp, err := p.Generate(ctx, req)
		if !yield(resp, err) {
			return
		}
	}
}

func (p *anthropicProvider) convertSystem(inst *blades.Message) []anthropic.TextBlockParam {
	if inst == nil {
		return nil
	}
	var blocks []anthropic.TextBlockParam
	for _, part := range inst.Parts {
		text, ok := part.(blades.TextPart)
		if !ok {
			continue
		}
		blocks = append(blocks, anthropic.TextBlockParam{Text: text.Text})
	}
	return blocks
}

func (p *anthropicProvider) convertMessages(messages []*blades.Message) ([]anthropic.MessageParam, error) {
	out := make([]anthropic.MessageParam, 0, len(messages))
	for _, m := range messages {
		if m == nil {
			continue
		}
		param, err := p.convertMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, param)
	}
	return out, nil
}

func (p *anthropicProvider) convertMessage(m *blades.Message) (anthropic.MessageParam, error) {
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

	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Parts))
	for _, part := range m.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			blocks = append(blocks, anthropic.ContentBlockParamUnion{
				OfText: &anthropic.TextBlockParam{Text: v.Text},
			})
		case blades.ToolPart:
			if v.Response != "" {
				// 工具结果块，必须以 user message 形式返回给模型。
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: v.ID,
						Content: []anthropic.ToolResultBlockParamContentUnion{
							{OfText: &anthropic.TextBlockParam{Text: v.Response}},
						},
					},
				})
			} else if m.Role == blades.RoleAssistant {
				// assistant 发起的 tool_use 调用。
				input := json.RawMessage(v.Request)
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
			// 忽略不支持的 part 类型。
		}
	}

	return anthropic.MessageParam{Role: role, Content: blocks}, nil
}

func (p *anthropicProvider) convertTools(tools []bladestools.Tool) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schemaMap, _ := schemaToMap(t.InputSchema())
		if schemaMap == nil {
			schemaMap = map[string]any{"type": "object"}
		}
		out = append(out, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name(),
				Description: anthropic.String(t.Description()),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: schemaMap,
					Required:   t.InputSchema().Required,
				},
			},
		})
	}
	return out
}

func (p *anthropicProvider) convertResponse(resp *anthropic.Message) *blades.Message {
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	parts := make([]blades.Part, 0, len(resp.Content))

	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			parts = append(parts, blades.TextPart{Text: v.Text})
		case anthropic.ToolUseBlock:
			input := string(v.Input)
			parts = append(parts, blades.NewToolPart(v.ID, v.Name, input))
		}
	}

	msg.Parts = parts
	msg.TokenUsage = blades.TokenUsage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
	}
	msg.FinishReason = string(resp.StopReason)
	return msg
}

func schemaToMap(s *jsonschema.Schema) (map[string]any, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
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
