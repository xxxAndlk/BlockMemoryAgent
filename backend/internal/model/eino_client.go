package model

import (
	"context"
	"fmt"
	"math"

	"github.com/cloudwego/eino-ext/components/model/openai"
	einoModel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/blockmemory/agent/backend/pkg/types"
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

// GenerateWithOptions 允许在不重建客户端的前提下覆盖 temperature
//
// v3 §6.3 要求按任务类型动态调节温度（路由 0；代码 0.15；创意 0.8）。
// Eino BaseChatModel 没有 per-call 温度选项，因此当温度变化超过阈值
// 时，我们临时构造一个新 ChatModel 完成本次调用，避免污染缓存。
func (c *EinoClient) GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error) {
	if math.Abs(temperature-c.cfg.Temperature) < 1e-6 {
		return c.Generate(ctx, prompt)
	}

	tmp := c.cfg
	tmp.Temperature = temperature
	override, err := createChatModel(ctx, tmp)
	if err != nil {
		return c.Generate(ctx, prompt)
	}

	resp, err := override.Generate(ctx, []*schema.Message{schema.UserMessage(prompt)})
	if err != nil {
		return "", fmt.Errorf("override generate: %w", err)
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
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}

	ocfg := &openai.ChatModelConfig{
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
		BaseURL: baseURL,
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
