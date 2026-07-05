package model

import (
	"context"

	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/contrib/openai"
)

// openAIProvider 基于 blades contrib/openai 的 OpenAI 兼容 provider 封装。
type openAIProvider struct {
	model blades.ModelProvider
	name  string
}

func newOpenAIProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	ocfg := openai.Config{
		BaseURL:         baseURL,
		APIKey:          cfg.APIKey,
		Temperature:     cfg.Temperature,
		MaxOutputTokens: int64(cfg.MaxTokens),
	}
	return &openAIProvider{
		model: openai.NewModel(cfg.Model, ocfg),
		name:  cfg.Model,
	}
}

func (p *openAIProvider) Name() string { return p.name }

func (p *openAIProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return p.model.Generate(ctx, req)
}

func (p *openAIProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	return p.model.NewStreaming(ctx, req)
}
