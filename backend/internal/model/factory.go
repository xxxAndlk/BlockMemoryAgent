package model

// 本文件定义 ModelFactory：按角色缓存 LLMClient 实例，
// 提供 LLMClient/TemperatureAware 接口与 mock 回退，
// 同时暴露底层 blades.ModelProvider 供工具循环路径使用。

import (
	"context" // 上下文传递
	"fmt"     // 错误格式化
	"strings" // 字符串裁剪（判断 API Key 是否为空）
	"sync"    // 读写锁，保护 models 缓存

	"github.com/go-kratos/blades"                     // blades.ModelProvider 类型引用
	"github.com/blockmemory/agent/backend/pkg/config" // RoleConfigFile 角色配置
	"github.com/blockmemory/agent/backend/pkg/types"  // AgentModelConfig 类型
)

// LLMClient 大模型客户端接口（与 graph 包兼容）。
// 设计意图：解耦 graph 层与具体 provider 实现，便于测试与替换。
type LLMClient interface {
	Generate(ctx context.Context, prompt string) (string, error) // 单轮文本生成
}

// TemperatureAware 可选接口：支持 per-call temperature 覆盖。
// BladesClient 实现此接口；mockClient 不实现，调用方需走 type assertion。
type TemperatureAware interface {
	GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error)
}

// GenerateWithTemperature 工具函数：若客户端实现 TemperatureAware
// 则用 per-call 温度，否则退回到 Generate。
//
// 设计意图：让调用方无需关心底层是否支持温度覆盖，统一入口。
// 参数：
//   - ctx: 上下文
//   - c: LLMClient 实例
//   - prompt: 提示词
//   - temperature: 期望温度
//
// 返回：
//   - string: 模型回复
//   - error: 调用错误
//
// 并发安全：无共享状态。
func GenerateWithTemperature(ctx context.Context, c LLMClient, prompt string, temperature float64) (string, error) {
	// 类型断言：判断是否支持温度覆盖
	if t, ok := c.(TemperatureAware); ok {
		return t.GenerateWithOptions(ctx, prompt, temperature)
	}
	// 不支持则忽略温度，走默认 Generate
	return c.Generate(ctx, prompt)
}

// mockClient 无API Key时的回退客户端。
// 设计意图：让服务器在缺少 LLM 凭证时仍能启动，便于本地调试与测试。
type mockClient struct{}

// Generate 实现 LLMClient 接口，返回模拟响应。
// 并发安全：无状态。
func (m *mockClient) Generate(ctx context.Context, prompt string) (string, error) {
	// 返回简单提示，表明这是模拟输出
	return fmt.Sprintf("[模拟响应] 收到请求长度: %d 字符", len(prompt)), nil
}

// ModelFactory 模型工厂，按角色缓存模型实例。
// 设计意图：避免重复构造 provider（连接池/鉴权开销），按角色复用。
type ModelFactory struct {
	mu     sync.RWMutex           // 读写锁保护 models 并发访问
	models map[string]LLMClient   // key: roleDefID or "meta" or "domain"
	cfg    *config.RoleConfigFile // 角色配置，用于解析每个角色的 ModelConfig
}

// NewModelFactory 创建模型工厂。
//
// 参数：
//   - cfg: 角色配置文件（meta/domain/fixed_roles）
//
// 返回：
//   - *ModelFactory: 工厂实例（models 缓存为空，按需填充）
//
// 副作用：无（不做网络请求）。
// 并发安全：返回实例可被多协程共享调用。
func NewModelFactory(cfg *config.RoleConfigFile) *ModelFactory {
	return &ModelFactory{
		models: make(map[string]LLMClient), // 初始化空缓存
		cfg:    cfg,
	}
}

// GetModel 获取指定角色的 LLMClient（带缓存）。
//
// 职责：先查缓存（读锁），未命中再构造（写锁 + 双重检查）。
// 参数：
//   - ctx: 上下文（透传给 NewBladesClient）
//   - roleDefID: 角色ID（"meta"/"domain"/固定角色ID/动态角色ID）
//
// 返回：
//   - LLMClient: 该角色的客户端（可能是真实或 mock）
//   - error: provider 构造失败
//
// 副作用：首次调用会构造并缓存客户端。
// 并发安全：读写锁 + 双重检查，保证同角色只构造一次。
func (f *ModelFactory) GetModel(ctx context.Context, roleDefID string) (LLMClient, error) {
	// 快路径：读锁查缓存
	f.mu.RLock()
	if m, ok := f.models[roleDefID]; ok {
		f.mu.RUnlock()
		return m, nil
	}
	f.mu.RUnlock()

	// 慢路径：升级为写锁构造
	f.mu.Lock()
	defer f.mu.Unlock()

	// 双重检查：防止等待锁期间已被其他协程构造
	if m, ok := f.models[roleDefID]; ok {
		return m, nil
	}

	// 根据角色ID解析对应的模型配置
	modelCfg := f.resolveConfig(roleDefID)

	// 无 API Key 时回退到 Mock，保证服务器可启动
	if strings.TrimSpace(modelCfg.APIKey) == "" {
		f.models[roleDefID] = &mockClient{}
		return f.models[roleDefID], nil
	}

	// 构造真实 BladesClient
	client, err := NewBladesClient(ctx, modelCfg)
	if err != nil {
		return nil, fmt.Errorf("create blades client for %s: %w", roleDefID, err)
	}

	// 写入缓存并返回
	f.models[roleDefID] = client
	return client, nil
}

// GetBladesProvider 获取指定角色的底层 blades.ModelProvider（用于工具循环路径）。
//
// 设计意图：工具调用循环需要访问 provider 的扩展能力（如 ToolCall），
// 而非仅 Generate；此方法把底层 provider 暴露出去。
// 无 API Key（mock 路径）时返回错误，调用方应退回单次 Generate。
// 参数：
//   - ctx: 上下文
//   - roleDefID: 角色ID
//
// 返回：
//   - blades.ModelProvider: 底层 provider
//   - error: 客户端非 BladesClient（即 mock）或构造失败
//
// 副作用：首次调用会触发 GetModel 的缓存填充。
// 并发安全：底层 provider 线程安全。
func (f *ModelFactory) GetBladesProvider(ctx context.Context, roleDefID string) (blades.ModelProvider, error) {
	// 先取 LLMClient（带缓存）
	client, err := f.GetModel(ctx, roleDefID)
	if err != nil {
		return nil, err
	}
	// 类型断言为 BladesClient；mock 路径会失败
	bc, ok := client.(*BladesClient)
	if !ok {
		return nil, fmt.Errorf("blades provider unavailable for %s (mock client)", roleDefID)
	}
	// 返回底层 provider
	return bc.Provider(), nil
}

// GetMetaModel 获取 MetaAgent 的模型。
// 设计意图：MetaAgent 是图入口节点，使用独立（通常更轻量）的模型配置。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetMetaModel(ctx context.Context) (LLMClient, error) {
	return f.GetModel(ctx, "meta") // "meta" 为 MetaAgent 的固定缓存键
}

// GetDomainModel 获取 DomainAgent 的模型（所有 DomainAgent 共用）。
// 设计意图：DomainAgent 数量可变，统一复用同一模型配置以减少开销。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetDomainModel(ctx context.Context) (LLMClient, error) {
	return f.GetModel(ctx, "domain") // "domain" 为 Domain/SubDomain 共用缓存键
}

// resolveConfig 根据角色ID解析模型配置。
//
// 职责：把 roleDefID 映射到 AgentModelConfig。
//   - "meta" → MetaAgent 配置
//   - "domain" → DomainAgent 配置
//   - 其他 → 先查 fixed_roles，未命中则回退 DomainAgent 配置（动态助手）
//
// 参数：
//   - roleDefID: 角色 ID
//
// 返回：
//   - types.AgentModelConfig: 该角色的模型配置
//
// 副作用：无。
// 并发安全：只读 cfg，无锁。
func (f *ModelFactory) resolveConfig(roleDefID string) types.AgentModelConfig {
	switch roleDefID {
	case "meta":
		return f.cfg.MetaAgent.ModelConfig // MetaAgent 专用配置
	case "domain":
		return f.cfg.DomainAgent.ModelConfig // DomainAgent 共用配置
	default:
		// 查找固定角色配置（roles.yaml 中显式定义的角色）
		if role := f.cfg.GetFixedRole(roleDefID); role != nil {
			return role.ModelConfig
		}
		// 回退到 DomainAgent 配置：动态生成的助手角色默认沿用 Domain 模型
		return f.cfg.DomainAgent.ModelConfig
	}
}

// WarmUp 预热常用模型（避免首次调用延迟）。
//
// 职责：在启动阶段提前构造 Meta/Domain 模型，避免首个请求承担构造开销。
// 参数：
//   - ctx: 上下文
//
// 返回：
//   - error: 任一预热失败时返回（含定位信息）
//
// 副作用：填充 models 缓存；可能触发底层 HTTP 连接池初始化。
// 并发安全：通过 GetModel 内部锁保证。
func (f *ModelFactory) WarmUp(ctx context.Context) error {
	// 预热 MetaAgent 模型
	if _, err := f.GetMetaModel(ctx); err != nil {
		return fmt.Errorf("warmup meta model: %w", err)
	}
	// 预热 DomainAgent 模型
	if _, err := f.GetDomainModel(ctx); err != nil {
		return fmt.Errorf("warmup domain model: %w", err)
	}
	return nil
}
