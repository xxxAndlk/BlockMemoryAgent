package model

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einoModel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/blockmemory/agent/pkg/types"
)

// EinoClient 包装 Eino ChatModel，实现 LLMClient 接口
type EinoClient struct {
	chatModel einoModel.BaseChatModel
	cfg       types.AgentModelConfig
}

// NewEinoClient 从配置创建 Eino 客户端
func NewEinoClient(ctx context.Context, cfg types.AgentModelConfig) (*EinoClient, error) {
	chatModel, err := createChatModel(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create chat model: %w", err)
	}

	return &EinoClient{
		chatModel: chatModel,
		cfg:       cfg,
	}, nil
}

// Generate 实现 LLMClient 接口
func (c *EinoClient) Generate(ctx context.Context, prompt string) (string, error) {
	messages := []*schema.Message{
		schema.UserMessage(prompt),
	}

	resp, err := c.chatModel.Generate(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("model generate: %w", err)
	}

	if resp == nil {
		return "", fmt.Errorf("empty model response")
	}

	return resp.Content, nil
}

// GenerateWithSystem 带 system prompt 生成（用于角色定义生成）
func (c *EinoClient) GenerateWithSystem(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	messages := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(userPrompt),
	}

	resp, err := c.chatModel.Generate(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("model generate: %w", err)
	}

	if resp == nil {
		return "", fmt.Errorf("empty model response")
	}

	return resp.Content, nil
}

// createChatModel 根据配置创建对应的 ChatModel
func createChatModel(ctx context.Context, cfg types.AgentModelConfig) (einoModel.BaseChatModel, error) {
	switch cfg.Provider {
	case "openai", "":
		return createOpenAIModel(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

// createOpenAIModel 创建 OpenAI 兼容模型
func createOpenAIModel(ctx context.Context, cfg types.AgentModelConfig) (einoModel.BaseChatModel, error) {
	ocfg := &openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		BaseURL: cfg.BaseURL,
	}

	if cfg.Temperature != 0 {
		t := float32(cfg.Temperature)
		ocfg.Temperature = &t
	}
	if cfg.MaxTokens > 0 {
		mt := cfg.MaxTokens
		ocfg.MaxCompletionTokens = &mt
	}

	return openai.NewChatModel(ctx, ocfg)
}
