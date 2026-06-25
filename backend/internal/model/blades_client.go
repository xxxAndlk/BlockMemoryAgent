package model

import (
	"context"
	"fmt"
	"math"

	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/contrib/openai"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// BladesClient 包装 blades.ModelProvider，实现 LLMClient 接口。
type BladesClient struct {
	provider blades.ModelProvider
	cfg      types.AgentModelConfig
}

// NewBladesClient 从配置创建 Blades 客户端
func NewBladesClient(ctx context.Context, cfg types.AgentModelConfig) (*BladesClient, error) {
	provider, err := createBladesProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("create blades provider: %w", err)
	}
	return &BladesClient{provider: provider, cfg: cfg}, nil
}

// Provider 暴露底层 blades.ModelProvider，供工具循环路径直接使用
func (c *BladesClient) Provider() blades.ModelProvider {
	return c.provider
}

// Generate 实现 LLMClient 接口
func (c *BladesClient) Generate(ctx context.Context, prompt string) (string, error) {
	req := &blades.ModelRequest{
		Messages: []*blades.Message{blades.UserMessage(prompt)},
	}
	resp, err := c.provider.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("model generate: %w", err)
	}
	if resp == nil || resp.Message == nil {
		return "", fmt.Errorf("empty model response")
	}
	return resp.Message.Text(), nil
}

// GenerateWithSystem 带 system prompt 生成（用于角色定义生成）
func (c *BladesClient) GenerateWithSystem(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	req := &blades.ModelRequest{
		Instruction: blades.SystemMessage(systemPrompt),
		Messages:    []*blades.Message{blades.UserMessage(userPrompt)},
	}
	resp, err := c.provider.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("model generate: %w", err)
	}
	if resp == nil || resp.Message == nil {
		return "", fmt.Errorf("empty model response")
	}
	return resp.Message.Text(), nil
}

// GenerateWithOptions 允许覆盖 temperature 完成单次调用。
//
// v3 §6.3 要求按任务类型动态调节温度（路由 0；代码 0.15；创意 0.8）。
// blades openai.Config 的 Temperature 在构造时固定，无 per-call 选项，
// 因此当温度变化超过阈值时，临时构造一个新 provider 完成本次调用，
// 避免污染缓存。openai.NewModel 构造廉价，无需缓存温度变体。
func (c *BladesClient) GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error) {
	if math.Abs(temperature-c.cfg.Temperature) < 1e-6 {
		return c.Generate(ctx, prompt)
	}

	tmp := c.cfg
	tmp.Temperature = temperature
	override, err := createBladesProvider(tmp)
	if err != nil {
		return c.Generate(ctx, prompt)
	}

	req := &blades.ModelRequest{
		Messages: []*blades.Message{blades.UserMessage(prompt)},
	}
	resp, err := override.Generate(ctx, req)
	if err != nil {
		return "", fmt.Errorf("override generate: %w", err)
	}
	if resp == nil || resp.Message == nil {
		return "", fmt.Errorf("empty model response")
	}
	return resp.Message.Text(), nil
}

// createBladesProvider 根据配置创建对应的 blades.ModelProvider
func createBladesProvider(cfg types.AgentModelConfig) (blades.ModelProvider, error) {
	switch cfg.Provider {
	case "openai", "":
		return createOpenAIProvider(cfg), nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
}

// createOpenAIProvider 创建 OpenAI 兼容模型
func createOpenAIProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/v1"
	}
	ocfg := openai.Config{
		BaseURL:         baseURL,
		APIKey:          cfg.APIKey,
		Temperature:     cfg.Temperature,
		MaxOutputTokens: int64(cfg.MaxTokens),
	}
	return openai.NewModel(cfg.Model, ocfg)
}
