package model

import (
	"context" // 上下文传递
	"strings" // 字符串判断

	"github.com/blockmemory/agent/backend/pkg/types" // AgentModelConfig 类型
	"github.com/go-kratos/blades"                    // ModelProvider 抽象
	"github.com/go-kratos/blades/contrib/openai"     // OpenAI 兼容 provider
)

// openAIProvider 基于 blades contrib/openai 的 OpenAI 兼容 provider 封装。
type openAIProvider struct {
	model blades.ModelProvider // 底层 blades OpenAI provider
	name  string               // 模型名称
}

// newOpenAIProvider 构造一个 OpenAI 兼容 provider。
//
// 参数：
//   - cfg: 模型配置，需包含 BaseURL/APIKey/Temperature/MaxTokens/Model
//
// 返回：blades.ModelProvider 实例。
func newOpenAIProvider(cfg types.AgentModelConfig) blades.ModelProvider {
	// 若未配置 baseURL，使用官方默认端点
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	// 构造 openai.Config
	ocfg := openai.Config{
		BaseURL:         baseURL,
		APIKey:          cfg.APIKey,
		Temperature:     cfg.Temperature,
		MaxOutputTokens: int64(cfg.MaxTokens),
	}
	// DeepSeek 思考模型（deepseek-v4-flash 等）默认开启 thinking，返回 reasoning_content；
	// 后续请求须把 reasoning_content 回传，否则 400 "reasoning_content must be passed back"。
	// blades contrib openai v0.3.0 不捕获也不回传 reasoning_content，直接关闭 thinking 最简。
	// 通过 ExtraFields 注入 enable_thinking=false；DeepSeek 端点识别此参数。
	if strings.Contains(strings.ToLower(baseURL), "deepseek.com") {
		if ocfg.ExtraFields == nil {
			ocfg.ExtraFields = map[string]any{}
		}
		ocfg.ExtraFields["enable_thinking"] = false
	}
	// 返回封装后的 provider
	return &openAIProvider{
		model: openai.NewModel(cfg.Model, ocfg),
		name:  cfg.Model,
	}
}

// Name 返回 provider 使用的模型名称。
func (p *openAIProvider) Name() string { return p.name }

// Generate 调用底层 OpenAI 兼容 provider 完成生成。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：
//   - *blades.ModelResponse: 模型响应
//   - error: 生成错误
func (p *openAIProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 直接委托底层 provider
	return p.model.Generate(ctx, req)
}

// NewStreaming 创建流式生成器。
//
// 参数：
//   - ctx: 上下文
//   - req: blades 模型请求
//
// 返回：blades.Generator 流式生成器。
func (p *openAIProvider) NewStreaming(ctx context.Context, req *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error] {
	// 直接委托底层 provider 的流式实现
	return p.model.NewStreaming(ctx, req)
}
