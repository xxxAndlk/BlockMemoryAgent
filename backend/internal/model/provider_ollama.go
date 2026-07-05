package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/ollama/ollama/api"
)

// ollamaProvider 基于 Ollama Go SDK 原生 /api/chat 的 provider 封装。
type ollamaProvider struct {
	client      *api.Client
	modelName   string
	baseURL     string
	temperature float64
	maxTokens   int
}

// newOllamaProvider 构造一个 Ollama 原生 provider。
func newOllamaProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		// 解析失败时回退到默认 URL，避免构造阶段崩溃。
		u, _ = url.Parse("http://localhost:11434")
	}

	return &ollamaProvider{
		client:      api.NewClient(u, http.DefaultClient),
		modelName:   cfg.Model,
		baseURL:     baseURL,
		temperature: cfg.Temperature,
		maxTokens:   cfg.MaxTokens,
	}
}

func (p *ollamaProvider) Name() string { return p.modelName }

func (p *ollamaProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	messages := p.convertMessages(req)
	tools := p.convertTools(req.Tools)

	stream := false
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

	var lastMsg api.Message
	var usage blades.TokenUsage
	err := p.client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
		if resp.Done {
			lastMsg = resp.Message
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

	msg := p.convertMessage(lastMsg)
	msg.TokenUsage = usage
	return &blades.ModelResponse{Message: msg}, nil
}

func (p *ollamaProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return func(yield func(*blades.ModelResponse, error) bool) {
		resp, err := p.Generate(ctx, req)
		if !yield(resp, err) {
			return
		}
	}
}

func (p *ollamaProvider) convertMessages(req *blades.ModelRequest) []api.Message {
	var messages []api.Message

	if req.Instruction != nil {
		text := messageText(req.Instruction)
		if text != "" {
			messages = append(messages, api.Message{Role: "system", Content: text})
		}
	}

	for _, m := range req.Messages {
		if m == nil {
			continue
		}
		messages = append(messages, p.convertBladesMessage(m))
	}

	return messages
}

func (p *ollamaProvider) convertBladesMessage(m *blades.Message) api.Message {
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

	var content string
	var toolCalls []api.ToolCall

	for _, part := range m.Parts {
		switch v := part.(type) {
		case blades.TextPart:
			if content != "" {
				content += "\n"
			}
			content += v.Text
		case blades.ToolPart:
			if v.Response != "" {
				// 工具结果，以文本形式拼接到 user message。
				if content != "" {
					content += "\n"
				}
				content += fmt.Sprintf("[%s result]: %s", v.Name, v.Response)
			} else if m.Role == blades.RoleAssistant {
				args, err := toolArgsToMap(v.Request)
				if err != nil {
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

func (p *ollamaProvider) convertMessage(m api.Message) *blades.Message {
	msg := blades.NewAssistantMessage(blades.StatusCompleted)
	var parts []blades.Part

	if m.Content != "" {
		parts = append(parts, blades.TextPart{Text: m.Content})
	}

	for _, tc := range m.ToolCalls {
		reqJSON, _ := json.Marshal(tc.Function.Arguments)
		parts = append(parts, blades.NewToolPart("", tc.Function.Name, string(reqJSON)))
	}

	msg.Parts = parts
	return msg
}

func (p *ollamaProvider) convertTools(tools []bladestools.Tool) []api.Tool {
	out := make([]api.Tool, 0, len(tools))
	for _, t := range tools {
		tool, err := toolToOllamaTool(t)
		if err != nil {
			continue
		}
		out = append(out, tool)
	}
	return out
}

func toolToOllamaTool(t bladestools.Tool) (api.Tool, error) {
	schema := t.InputSchema()
	properties := make(map[string]any)
	for name, prop := range schema.Properties {
		propMap := map[string]any{
			"type":        schemaType(prop),
			"description": prop.Description,
		}
		if enum := anySliceToStrings(prop.Enum); len(enum) > 0 {
			propMap["enum"] = enum
		}
		properties[name] = propMap
	}

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

func schemaType(s *jsonschema.Schema) string {
	if s == nil {
		return "object"
	}
	if s.Type != "" {
		return s.Type
	}
	if len(s.Types) > 0 {
		return s.Types[0]
	}
	return "object"
}

func messageText(m *blades.Message) string {
	var text string
	for _, part := range m.Parts {
		if t, ok := part.(blades.TextPart); ok {
			if text != "" {
				text += "\n"
			}
			text += t.Text
		}
	}
	return text
}

func toolArgsToMap(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	return m, nil
}

func anySliceToStrings(v []any) []string {
	out := make([]string, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
