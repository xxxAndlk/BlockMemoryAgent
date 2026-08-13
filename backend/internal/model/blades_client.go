package model

// 本文件实现 BladesClient：对 blades.ModelProvider 的薄包装，
// 提供 LLMClient / TemperatureAware 接口所需的 Generate 系列方法，
// 供 ModelFactory 按角色缓存并复用。

import (
	"context" // 上下文，用于超时与取消传递
	"fmt"     // 错误格式化
	"math"    // 浮点比较，判断温度是否变化

	"github.com/blockmemory/agent/backend/pkg/types" // AgentModelConfig 等共享类型
	"github.com/go-kratos/blades"                    // 上游 ModelProvider 抽象
)

// BladesClient 包装 blades.ModelProvider，实现 LLMClient 接口。
// 设计意图：屏蔽底层 provider 细节，对 graph 层暴露统一 Generate 入口；
// 同时保留原始 Provider() 以供工具循环（tool-calling）路径直接调用。
type BladesClient struct {
	provider blades.ModelProvider   // 底层 blades provider，真正承担请求
	cfg      types.AgentModelConfig // 构造时的模型配置（含温度/MaxTokens 等），用于按需重建 provider
}

// NewBladesClient 根据给定配置构造 BladesClient。
//
// 职责：解析配置 → 创建底层 blades.ModelProvider → 装入 BladesClient。
// 参数：
//   - ctx: 上下文（当前实现未透传给 provider 构造，保留以备未来扩展）
//   - cfg: 模型配置（Provider/BaseURL/APIKey/Model/Temperature/MaxTokens）
//
// 返回：
//   - *BladesClient: 可用的客户端
//   - error: provider 创建失败时返回（如 provider 类型不支持）
//
// 副作用：无外部状态变更；provider 内部可能持有 HTTP 连接池。
// 并发安全：返回的实例可被多协程并发使用（底层 provider 自身线程安全）。
func NewBladesClient(ctx context.Context, cfg types.AgentModelConfig) (*BladesClient, error) {
	// 委托给 createBladesProvider 按 Provider 字段选择实现
	provider, err := createBladesProvider(cfg)
	if err != nil {
		// 包装错误便于上层定位
		return nil, fmt.Errorf("create blades provider: %w", err)
	}
	// 缓存 provider 与原始配置，供后续 Generate/GenerateWithOptions 使用
	return &BladesClient{provider: provider, cfg: cfg}, nil
}

// Provider 暴露底层 blades.ModelProvider，供工具调用循环路径直接使用。
//
// 设计意图：工具循环需要访问 provider 的 Chat/ToolCall 等扩展能力，
// 而 LLMClient 接口只暴露 Generate；通过此方法把底层 provider 透出去。
//
// 返回：底层 blades.ModelProvider 实例。
// 并发安全：只读返回字段，本身线程安全。
func (c *BladesClient) Provider() blades.ModelProvider {
	// 直接返回底层 provider 引用
	return c.provider
}

// ModelName 返回当前配置的模型名，供 react_agent 日志记录 model 字段。
// 实现 react_agent.llmModelName 期望的 interface{ ModelName() string } 接口。
func (c *BladesClient) ModelName() string {
	return c.cfg.Model
}

// doGenerate 执行 blades 生成请求并做通用校验。
// 四个 Generate* 方法共享此 helper，避免重复构造请求与空响应检查。
//
// 一律流式：ark /api/coding 对 thinking 模型拒绝非流式长任务请求
// （"streaming is required for operations that may take longer than 10 minutes"），
// 2026-08-13 实证非流式 Generate 全天 400（skill 选择/策略决策等辅助调用静默失效）。
//
// 参数：
//   - ctx: 上下文
//   - provider: 实际使用的 blades.ModelProvider
//   - req: 已构造好的请求
//
// 返回：
//   - *blades.ModelResponse: 成功响应（最后一次有效产出）
//   - error: 生成失败或响应为空时返回错误
func (c *BladesClient) doGenerate(ctx context.Context, provider blades.ModelProvider, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	var final *blades.ModelResponse
	for resp, err := range provider.NewStreaming(ctx, req) {
		if err != nil {
			return nil, fmt.Errorf("model generate: %w", err)
		}
		if resp != nil && resp.Message != nil {
			final = resp
		}
	}
	if final == nil {
		return nil, fmt.Errorf("empty model response")
	}
	return final, nil
}

// Generate 实现 LLMClient 接口的单轮文本生成。
//
// 职责：把 prompt 包装成单条 user message 调用底层 provider。
// 参数：
//   - ctx: 上下文，用于超时/取消
//   - prompt: 用户提示词
//
// 返回：
//   - string: 模型回复文本
//   - error: 调用失败或响应为空时返回
//
// 副作用：无（除底层 HTTP 调用外）。
// 并发安全：底层 provider 线程安全，可并发调用。
func (c *BladesClient) Generate(ctx context.Context, prompt string) (string, error) {
	// 构造仅含一条 user 消息的请求
	req := &blades.ModelRequest{
		Messages: []*blades.Message{blades.UserMessage(prompt)},
	}
	// 调用通用生成 helper
	resp, err := c.doGenerate(ctx, c.provider, req)
	if err != nil {
		// 出错直接返回
		return "", err
	}
	// 提取并返回消息文本
	return resp.Message.Text(), nil
}

// GenerateWithUsage 实现 UsageAware 接口：单轮文本生成并返回 token 用量。
//
// 设计意图（P0-4）：blades 工具循环的 mock 退化路径（provider==nil）需要真实 token 计量；
// 本方法复用 Generate 逻辑，额外返回 resp.Message.TokenUsage（OpenAI 兼容响应标准字段）。
// provider 未填充用量时返回零值 TokenUsage，调用方应回退到 EstimateTokens。
//
// 参数：
//   - ctx: 上下文
//   - prompt: 用户提示词
//
// 返回：
//   - string: 模型回复文本
//   - blades.TokenUsage: token 用量（InputTokens/OutputTokens/TotalTokens）
//   - error: 调用失败或响应为空
func (c *BladesClient) GenerateWithUsage(ctx context.Context, prompt string) (string, blades.TokenUsage, error) {
	// 构造仅含一条 user 消息的请求
	req := &blades.ModelRequest{
		Messages: []*blades.Message{blades.UserMessage(prompt)},
	}
	// 调用通用生成 helper
	resp, err := c.doGenerate(ctx, c.provider, req)
	if err != nil {
		// 出错时返回空字符串与零值 TokenUsage
		return "", blades.TokenUsage{}, err
	}
	// 返回文本与底层 provider 提供的 token 用量
	return resp.Message.Text(), resp.Message.TokenUsage, nil
}

// GenerateWithSystem 带 system prompt 生成（用于角色定义生成等需要设定人设的场景）。
//
// 职责：在 user 消息之外额外传入 system instruction，引导模型行为。
// 参数：
//   - ctx: 上下文
//   - systemPrompt: 系统提示词（人设/规则）
//   - userPrompt: 用户输入
//
// 返回：
//   - string: 模型回复文本
//   - error: 调用失败或响应为空
//
// 副作用：无。
// 并发安全：底层 provider 线程安全。
func (c *BladesClient) GenerateWithSystem(ctx context.Context, systemPrompt, userPrompt string) (string, error) {
	// 构造包含 system instruction 与 user message 的请求
	req := &blades.ModelRequest{
		Instruction: blades.SystemMessage(systemPrompt),
		Messages:    []*blades.Message{blades.UserMessage(userPrompt)},
	}
	// 调用通用生成 helper
	resp, err := c.doGenerate(ctx, c.provider, req)
	if err != nil {
		return "", err
	}
	// 返回模型回复文本
	return resp.Message.Text(), nil
}

// GenerateWithOptions 允许覆盖 temperature 完成单次调用。
//
// 设计意图：v3 §6.3 要求按任务类型动态调节温度（路由 0；代码 0.15；创意 0.8）。
// blades openai.Config 的 Temperature 在构造时固定，无 per-call 选项，
// 因此当温度变化超过阈值时，临时构造一个新 provider 完成本次调用，
// 避免污染缓存。openai.NewModel 构造廉价，无需缓存温度变体。
//
// 参数：
//   - ctx: 上下文
//   - prompt: 用户提示词
//   - temperature: 本次调用所需温度
//
// 返回：
//   - string: 模型回复文本
//   - error: 调用失败
//
// 副作用：温度变化时临时创建一个 provider，调用后丢弃（不影响缓存）。
// 并发安全：临时 provider 仅本次调用可见，无共享状态。
func (c *BladesClient) GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error) {
	// 温度与缓存配置一致时，直接走快速路径，避免无谓重建
	if math.Abs(temperature-c.cfg.Temperature) < 1e-6 {
		return c.Generate(ctx, prompt)
	}

	// 复制配置并覆盖温度，构造临时 provider
	tmp := c.cfg
	tmp.Temperature = temperature
	override, err := createBladesProvider(tmp)
	if err != nil {
		// 临时 provider 创建失败时退回默认 Generate，保证可用性
		return c.Generate(ctx, prompt)
	}

	// 构造仅含一条 user 消息的请求
	req := &blades.ModelRequest{
		Messages: []*blades.Message{blades.UserMessage(prompt)},
	}
	// 使用临时 provider 生成
	resp, err := c.doGenerate(ctx, override, req)
	if err != nil {
		return "", err
	}
	// 返回模型回复文本
	return resp.Message.Text(), nil
}

// createBladesProvider 根据配置中的 Provider 字段选择对应的 blades.ModelProvider 实现。
//
// 职责：provider 类型分发 + 统一包装 3 次重试。
//   - openai-chat：OpenAI Chat Completions 兼容端点（/chat/completions），自研实现，
//     兼容 DeepSeek/Kimi/GLM/豆包等三方端点的字段变体（reasoning_content 回传、
//     max_tokens vs max_completion_tokens、usage cache 字段等）。
//     openai / openai-deepseek / 空串 均为其别名（向后兼容旧配置）。
//   - openai-responses：OpenAI Responses API 端点（/responses），自研实现，
//     适用于 gpt-4.1/o 系/gpt-5 等新一代接口及其实现该协议的第三方端点。
//   - anthropic：使用 Anthropic Go SDK 原生 Messages API。
//   - ollama：使用 Ollama Go SDK 原生 /api/chat。
//
// 所有 provider 构造后经 wrapWithRetry 包装，Generate 调用失败时自动重试 3 次，
// 覆盖 429/503/超时/空响应等可重试状态；3 次后仍失败返回错误给上级。
//
// 参数：
//   - cfg: 模型配置
//
// 返回：
//   - blades.ModelProvider: 构造好的 provider（已包装重试）
//   - error: 不支持的 provider 类型
//
// 副作用：无外部状态变更。
// 并发安全：纯函数式构造，可并发调用。
func createBladesProvider(cfg types.AgentModelConfig) (blades.ModelProvider, error) {
	// 根据 provider 名称分发
	var inner blades.ModelProvider
	var err error
	switch cfg.Provider {
	case "openai", "openai-chat", "openai-deepseek", "":
		// openai / openai-deepseek / 空串 是 openai-chat 的别名：
		// openai-chat 原生支持 reasoning_content 捕获与回传（DeepSeek V4 要求），
		// 并按模型名自动选择 max_tokens / max_completion_tokens 字段。
		inner = newOpenAIChatProvider(cfg)
	case "openai-responses":
		// OpenAI Responses API（/responses 端点）。
		inner = newOpenAIResponsesProvider(cfg)
	case "anthropic":
		// Anthropic 原生 Messages API
		inner = newAnthropicProvider(cfg)
	case "ollama":
		// Ollama 本地 /api/chat
		inner, err = newOllamaProvider(cfg)
		if err != nil {
			return nil, err
		}
	default:
		// 后续可在此扩展 azure/bedrock 等分支
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	// 统一包装 3 次重试：覆盖所有 provider 的 Generate 调用，
	// 非成功状态（含 402 余额不足、429 限速、5xx 服务端错误、超时、空响应）自动重试。
	return wrapWithRetry(inner, cfg.Model), nil
}
