package model

// 本文件定义 ModelFactory：按角色缓存 LLMClient 实例，
// 提供 LLMClient/TemperatureAware 接口，
// 同时暴露底层 blades.ModelProvider 供工具循环路径使用。

import (
	"context" // 上下文传递
	"fmt"     // 错误格式化
	"strings" // 失败角色列表拼接
	"sync"    // 读写锁，保护 models 缓存
	"time"    // 探测超时

	"github.com/blockmemory/agent/backend/pkg/config" // RoleConfigFile 角色配置
	"github.com/blockmemory/agent/backend/pkg/types"  // AgentModelConfig 类型
	"github.com/go-kratos/blades"                     // blades.ModelProvider 类型引用
)

// LLMClient 大模型客户端接口（与 graph 包兼容）。
// 设计意图：解耦 graph 层与具体 provider 实现，便于测试与替换。
type LLMClient interface {
	Generate(ctx context.Context, prompt string) (string, error) // 单轮文本生成
}

// TemperatureAware 可选接口：支持 per-call temperature 覆盖。
// BladesClient 实现此接口。
type TemperatureAware interface {
	GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error)
}

// UsageAware 可选接口：返回 token 用量的生成调用。
// BladesClient 实现此接口；仅需要真实 token 计量的路径（blades 工具循环的 mock 退化路径）使用。
// 设计意图（P0-4）：不破坏 LLMClient.Generate 的两返回值签名，需要用量的调用方类型断言到本接口。
type UsageAware interface {
	GenerateWithUsage(ctx context.Context, prompt string) (text string, usage blades.TokenUsage, err error)
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
//   - LLMClient: 该角色的客户端
//   - error: provider 构造失败（含 API Key 缺失）
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

	// API Key 缺失直接报错，不再回退 mock
	if modelCfg.APIKey == "" {
		return nil, fmt.Errorf("missing API key for role %s (provider=%s model=%s)", roleDefID, modelCfg.Provider, modelCfg.Model)
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
// 参数：
//   - ctx: 上下文
//   - roleDefID: 角色ID
//
// 返回：
//   - blades.ModelProvider: 底层 provider
//   - error: 构造失败或客户端非 BladesClient
//
// 副作用：首次调用会触发 GetModel 的缓存填充。
// 并发安全：底层 provider 线程安全。
func (f *ModelFactory) GetBladesProvider(ctx context.Context, roleDefID string) (blades.ModelProvider, error) {
	// 先取 LLMClient（带缓存）
	client, err := f.GetModel(ctx, roleDefID)
	if err != nil {
		return nil, err
	}
	// 类型断言为 BladesClient
	bc, ok := client.(*BladesClient)
	if !ok {
		return nil, fmt.Errorf("blades provider unavailable for %s", roleDefID)
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

// GetLightweightModel 获取轻量模型，用于历史总结/检索 query 改写等低开销任务。
// 设计意图：与主对话模型解耦，可指向更便宜更快的模型，降低高频小任务的成本与延迟。
// 未配置 lightweight_model 时回退到 DomainAgent 模型（见 resolveConfig）。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetLightweightModel(ctx context.Context) (LLMClient, error) {
	return f.GetModel(ctx, "lightweight") // "lightweight" 为轻量模型固定缓存键
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
	case "lightweight":
		// 轻量模型：用于历史总结/检索 query 改写等低开销任务。
		// 未配置时回退到 DomainAgent 配置，保证启动不中断。
		if f.cfg.LightweightModel.Model != "" {
			return f.cfg.LightweightModel
		}
		return f.cfg.DomainAgent.ModelConfig
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
	// 预热轻量模型（历史总结/检索改写用）；未配置时回退 domain 已预热，此处可容错
	if _, err := f.GetLightweightModel(ctx); err != nil {
		return fmt.Errorf("warmup lightweight model: %w", err)
	}
	return nil
}

// probePrompt 连通性探测用的最小化提示词（廉价，约 1 token）。
const probePrompt = "ping"

// VerifyConnectivity 启动期对每个已配置模型角色发起一次最小化真实 LLM 调用，验证可连通性。
//
// 职责（P0-1）：
//   - 候选角色 = meta/domain/lightweight + 每个 fixed_roles[].ID
//   - 按 (provider,model,apikey,baseURL) 去重：roles.yaml 多角色常共享同一后端，去重后仅探测一次
//   - APIKey 为空（Mock/无 key 模式）跳过，不阻塞启动
//   - 每个唯一后端用 probeLLM 探测（3 次重试，短超时）
//
// 返回：
//   - error: 任一角色不可达时返回聚合错误（列出全部失败角色），全部可达/跳过则返回 nil
//
// 副作用：可能触发 GetModel 缓存填充（与 WarmUp 重叠，幂等）。
// 并发安全：GetModel 内部锁保护。
func (f *ModelFactory) VerifyConnectivity(ctx context.Context) error {
	// 候选角色列表：三个内置角色 + 全部固定角色
	roles := []string{"meta", "domain", "lightweight"}
	for _, fr := range f.cfg.FixedRoles {
		if fr.ID != "" {
			roles = append(roles, fr.ID)
		}
	}

	seen := make(map[string]bool) // 按 (provider,model,key,baseURL) 去重
	var failed []string
	for _, roleID := range roles {
		cfg := f.resolveConfig(roleID)
		// Mock/无 key 模式：跳过，视为 OK（保证服务可在无 LLM 环境启动）
		if cfg.APIKey == "" {
			continue
		}
		key := cfg.Provider + "\x00" + cfg.Model + "\x00" + cfg.APIKey + "\x00" + cfg.BaseURL
		if seen[key] {
			continue // 同一后端已探测，跳过
		}
		seen[key] = true

		// 构造一个 MaxTokens=1 的临时探测客户端：连通性探测只需模型"能应答"，
		// 用极小输出预算避免推理类模型（如 deepseek-v4-flash）为 "ping" 生成大段
		// reasoning 而拖慢/超时启动校验。正常 LLM 在 ~1s 内返回；错误配置（key/
		// 端点/模型名）通常在 <1s 内返回 4xx，仍能快速失败。
		probeCfg := cfg
		probeCfg.MaxTokens = 1
		probeClient, err := NewBladesClient(ctx, probeCfg)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (构造失败: %v)", roleID, err))
			continue
		}
		// 探测：整体 60s（容纳 2 次重试 + 退避 + 冷启动），单次 30s
		// deepseek-v4-flash 等推理类模型首包冷启动可能 >12s，拉长单次超时避免误杀
		probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := probeLLM(probeCtx, probeClient); err != nil {
			failed = append(failed, fmt.Sprintf("%s (model=%s: %v)", roleID, cfg.Model, err))
		}
		cancel()
	}

	if len(failed) > 0 {
		return fmt.Errorf("LLM 连通性校验未通过: %s", strings.Join(failed, "; "))
	}
	return nil
}

// probeLLM 对已构造的探测客户端发起一次最小化调用（2 次重试，单次 30s），验证可连通性。
// 注：传入的 client 应已用 MaxTokens=1 构造，确保正常 LLM 快速应答。
// 返回 nil 表示连通。
func probeLLM(ctx context.Context, client LLMClient) error {
	_, err, _ := retryGenerate(ctx, client, probePrompt, 30*time.Second)
	return err
}

// CallLightweightWithRetry 用轻量模型生成（带 3 次重试）。
//
// 设计意图（P0-1）：reflectOnResult / summarizeHistoryForGoal 等轻量直连 callers
// 原本绕过 tracker 直调 llm.Generate 无重试；统一收敛到本方法，落实"轻量级总结模型优化"。
// per-attempt 超时 30s，整体取消由 ctx 控制。
func (f *ModelFactory) CallLightweightWithRetry(ctx context.Context, prompt string) (string, error) {
	llm, err := f.GetLightweightModel(ctx)
	if err != nil {
		return "", err
	}
	resp, err, _ := retryGenerate(ctx, llm, prompt, 30*time.Second)
	return resp, err
}
