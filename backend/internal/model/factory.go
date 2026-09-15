package model

// 本文件定义 ModelFactory：按角色缓存 LLMClient 实例，
// 提供 LLMClient/TemperatureAware 接口，
// 同时暴露底层 blades.ModelProvider 供工具循环路径使用。

import (
	"context" // 上下文传递
	"fmt"     // 错误格式化
	"sort"    // 实例覆盖清单排序（展示稳定）
	"strings" // 失败角色列表拼接
	"sync"    // 读写锁，保护 models 缓存
	"time"    // 探测超时

	"github.com/blockmemory/agent/backend/internal/logger" // logger 提供 ctx 携带的会话级日志器（轻量调用写 session_logs）
	"github.com/blockmemory/agent/backend/pkg/config"      // RoleConfigFile 角色配置
	"github.com/blockmemory/agent/backend/pkg/types"       // AgentModelConfig 类型
	"github.com/go-kratos/blades"                          // blades.ModelProvider 类型引用
)

// LLMClient 大模型客户端接口（与 graph 包兼容）。
// 设计意图：解耦 graph 层与具体 provider 实现，便于测试与替换。
type LLMClient interface {
	// Generate 执行单轮文本生成。
	Generate(ctx context.Context, prompt string) (string, error)
}

// TemperatureAware 可选接口：支持 per-call temperature 覆盖。
// BladesClient 实现此接口。
type TemperatureAware interface {
	// GenerateWithOptions 按指定温度生成文本。
	GenerateWithOptions(ctx context.Context, prompt string, temperature float64) (string, error)
}

// UsageAware 可选接口：返回 token 用量的生成调用。
// BladesClient 实现此接口；仅需要真实 token 计量的路径（blades 工具循环的 mock 退化路径）使用。
// 设计意图（P0-4）：不破坏 LLMClient.Generate 的两返回值签名，需要用量的调用方类型断言到本接口。
type UsageAware interface {
	// GenerateWithUsage 执行生成并返回 token 用量。
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
		// 支持则按指定温度调用
		return t.GenerateWithOptions(ctx, prompt, temperature)
	}
	// 不支持则忽略温度，走默认 Generate
	return c.Generate(ctx, prompt)
}

// cachedClient 缓存条目：客户端 + 构造时使用的生效配置。
// cfg 用于热更新比对：models.json 变更后重算生效配置，变化才失效重建。
type cachedClient struct {
	client LLMClient
	cfg    types.AgentModelConfig
}

// agentOverride 实例级模型覆盖条目：只活在本进程内存，实例终结即回收，永不落盘。
type agentOverride struct {
	roleID  string                 // 设置时的角色 ID（展示/审计用）
	modelID string                 // 注册表条目 ID
	cfg     types.AgentModelConfig // 构造客户端时使用的生效配置
	client  LLMClient              // 覆盖专用客户端（不参与角色缓存）
}

// AgentOverrideInfo 实例级覆盖摘要（展示用，不含 api_key）。
type AgentOverrideInfo struct {
	AgentID  string // 被覆盖的 Agent 实例 ID
	RoleID   string // 设置时的角色 ID
	ModelID  string // 注册表条目 ID
	Model    string // 生效模型名
	Thinking string // 生效思考档位
}

// ModelFactory 模型工厂，按角色缓存模型实例。
// 设计意图：避免重复构造 provider（连接池/鉴权开销），按角色复用。
type ModelFactory struct {
	mu             sync.RWMutex                      // 读写锁保护 models 并发访问
	models         map[string]cachedClient           // key: roleDefID or "meta" or "domain"
	cfg            *config.RoleConfigFile            // 角色配置，用于解析每个角色的 ModelConfig（只读，运行期不修改）
	dynMu          sync.RWMutex                      // 独立锁保护 dynamicConfigs（resolveConfig 会在 f.mu 写锁内被调用，不可重入）
	dynamicConfigs map[string]types.AgentModelConfig // 运行时动态角色的模型配置（P3-4）

	// registry 模型注册表（config/models.json）：连接参数来源 + 角色绑定 +
	// mtime 热更新。bootstrap 启动期注入；nil 时走纯内联配置（测试桩场景）。
	registry *config.RegistryStore
	// switchMu 串行化 SwitchModel（TUI 与 server 双入口共享同进程工厂）。
	switchMu sync.Mutex
	// probeHook 切换前的连通性探测钩子；nil 用默认实现（MaxTokens=1 临时客户端
	// + probeLLM，60s 超时）。测试注入 fake 以免真实网络调用。
	probeHook func(ctx context.Context, cfg types.AgentModelConfig) error

	// agentOvMu / agentOverrides：实例级模型覆盖（key: agentID）。独立于角色缓存 models——
	// role_bindings 变更 / invalidateChangedClients / checkRegistryReload 均不触碰覆盖
	// （角色换绑定不影响已覆盖实例）；反向亦隔离（覆盖不改同角色其他实例）。
	agentOvMu       sync.RWMutex
	agentOverrides  map[string]agentOverride
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
	// 初始化工厂，创建空缓存与动态配置 map
	return &ModelFactory{
		models:         make(map[string]cachedClient), // 初始化空缓存
		cfg:            cfg,
		dynamicConfigs: make(map[string]types.AgentModelConfig), // 动态角色模型配置
		agentOverrides: make(map[string]agentOverride),          // 实例级覆盖（默认空）
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
	// 热更新检查：models.json 变更则失效受影响的缓存条目（含缓存命中场景）。
	f.checkRegistryReload()

	// 快路径：读锁查缓存
	f.mu.RLock()
	if m, ok := f.models[roleDefID]; ok {
		// 缓存命中，释放读锁并返回
		f.mu.RUnlock()
		return m.client, nil
	}
	// 缓存未命中，释放读锁准备升级写锁
	f.mu.RUnlock()

	// 慢路径：升级为写锁构造
	f.mu.Lock()
	// 函数退出时释放写锁
	defer f.mu.Unlock()

	// 双重检查：防止等待锁期间已被其他协程构造
	if m, ok := f.models[roleDefID]; ok {
		return m.client, nil
	}

	// 根据角色ID解析生效的模型配置（含注册表合并）
	modelCfg, err := f.resolveConfig(roleDefID)
	if err != nil {
		return nil, err
	}

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
	f.models[roleDefID] = cachedClient{client: client, cfg: modelCfg}
	return client, nil
}

// SetRegistry 注入模型注册表（config/models.json）。由 bootstrap 启动期调用一次；
// 注入前解析走内联配置（注册表未就绪时 model_ref 无法解析）。
func (f *ModelFactory) SetRegistry(store *config.RegistryStore) {
	f.registry = store
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
		// 构造失败直接返回错误
		return nil, err
	}
	// 类型断言为 BladesClient
	bc, ok := client.(*BladesClient)
	if !ok {
		// 非 BladesClient 无法提供底层 provider
		return nil, fmt.Errorf("blades provider unavailable for %s", roleDefID)
	}
	// 返回底层 provider
	return bc.Provider(), nil
}

// GetBladesProviderForAgent 实例级模型解析：agentID 有覆盖时用覆盖客户端，否则回落
// 角色级 GetBladesProvider。agentID 为空（无实例语义的调用点）直接回落。
//
// 覆盖客户端在 SetAgentModel 时一并构造（探测已通过）；此处只读，无构造开销。
func (f *ModelFactory) GetBladesProviderForAgent(ctx context.Context, roleID, agentID string) (blades.ModelProvider, error) {
	if agentID != "" {
		f.agentOvMu.RLock()
		ov, ok := f.agentOverrides[agentID]
		f.agentOvMu.RUnlock()
		if ok {
			if bc, isBC := ov.client.(*BladesClient); isBC {
				return bc.Provider(), nil
			}
			return nil, fmt.Errorf("blades provider unavailable for agent %s", agentID)
		}
	}
	return f.GetBladesProvider(ctx, roleID)
}

// GetBladesProviderWithFallback 带备胎链的 provider 解析（TODO 第15项 T17）。
// 主模型 = GetBladesProvider 现有解析（role_bindings 覆写 > model_ref）；备胎链 =
// models.json role_bindings[roleID].fallback 依序列出的条目 ID。主模型调用出错
// （非 ctx 取消）时降级语义见 fallbackProvider。observe 可空（agent 层注入会话
// 作用域观察者）；绑定未配置 fallback 时原样返回主 provider（零行为变化）。
func (f *ModelFactory) GetBladesProviderWithFallback(ctx context.Context, roleID string, observe FallbackObserver) (blades.ModelProvider, error) {
	primary, err := f.GetBladesProvider(ctx, roleID)
	if err != nil {
		return nil, err
	}
	ids := f.fallbackChainFor(roleID)
	if len(ids) == 0 {
		return primary, nil
	}
	return newFallbackProvider(roleID, primary, ids, f.buildFallbackProvider, observe), nil
}

// fallbackChainFor 读 models.json role_bindings[roleID].fallback（热更新感知），
// 过滤空项。注册表未注入或绑定不存在时返回 nil。
func (f *ModelFactory) fallbackChainFor(roleID string) []string {
	f.checkRegistryReload()
	if f.registry == nil {
		return nil
	}
	b, ok := f.registry.Binding(roleID)
	if !ok || len(b.Fallback) == 0 {
		return nil
	}
	out := make([]string, 0, len(b.Fallback))
	for _, id := range b.Fallback {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// buildFallbackProvider 构造单个备胎条目的 provider（惰性调用，构造失败由备胎层
// 永久标记本档不可用）。生效配置 = 角色基础配置 + 条目连接参数（同 SetAgentModel
// 的构造口径，但不做连通性探测——备胎启用时机在故障现场，预探测会拖慢每次解析）。
func (f *ModelFactory) buildFallbackProvider(roleID, entryID string) (blades.ModelProvider, error) {
	entry, ok := f.entryByID(entryID)
	if !ok {
		return nil, fmt.Errorf("条目 %q 不在模型注册表", entryID)
	}
	cfg := applyModelEntry(f.resolveBaseConfig(roleID), entry)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("条目 %q 未配置 api_key", entryID)
	}
	client, err := NewBladesClient(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	return client.Provider(), nil
}

// SetAgentModel 为单个 Agent 实例覆盖模型（set_agent_model 工具核心）。
//
// 与 SwitchModel 的区别：只改本实例、不写 role_bindings、不落盘、不影响同角色其他实例
// 与其他角色；覆盖随实例存续，实例终结由调用方 ClearAgentModel 回收（进程重启天然清空）。
//
// 校验口径同 SwitchModel：角色可切换（meta/lightweight 由工具层拦截，此处仅查清单）、
// 条目存在、api_key 非空、连通性探测 fail-closed（失败不落覆盖保持原模型）。
// thinking 空 = 沿用该角色当前生效思考档；max_tokens 从角色侧重算（不继承旧模型条目的
// 输出上限，避免换到小上限模型时带过去超限值触发 400）。
func (f *ModelFactory) SetAgentModel(ctx context.Context, agentID, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	if strings.TrimSpace(agentID) == "" {
		return types.AgentModelConfig{}, fmt.Errorf("agent_id 不能为空")
	}
	if !f.isSwitchableRole(roleID) {
		return types.AgentModelConfig{}, fmt.Errorf("role %q 不支持动态切换模型", roleID)
	}
	// 热更新检查（与 SwitchModel 同：防设置到刚被删掉的条目）
	f.checkRegistryReload()
	entry, ok := f.entryByID(modelID)
	if !ok {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 不在模型注册表中", modelID)
	}
	// 同覆盖短路（同条目同思考档则无操作，避免重复探测）
	if cur, exists := f.agentOverrideOf(agentID); exists && cur.modelID == modelID && cur.cfg.Thinking == resolveThinking(f, roleID, thinking) {
		return cur.cfg, nil
	}
	// 角色不在条目允许表内则拒绝（selectable_roles 白名单）。
	if !entry.SwitchableFor(roleID) {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 仅限角色 %v 切换使用，角色 %q 不可选用", modelID, entry.SelectableRoles, roleID)
	}

	// 生效配置 = 角色侧行为参数 + 目标条目连接参数；thinking 沿用当前生效档（未显式指定）
	newCfg := applyModelEntry(f.resolveBaseConfig(roleID), entry)
	if t := resolveThinking(f, roleID, thinking); t != "" {
		newCfg.Thinking = t
	}
	if newCfg.APIKey == "" {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 未配置 api_key", modelID)
	}
	// 连通性探测（锁外网络 IO；MaxTokens=1 最小化开销，同 SwitchModel）
	probeCfg := newCfg
	probeCfg.MaxTokens = 1
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := f.probe(probeCtx, probeCfg); err != nil {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 连通性探测失败，未设置实例覆盖: %w", modelID, err)
	}
	client, err := NewBladesClient(ctx, newCfg)
	if err != nil {
		return types.AgentModelConfig{}, fmt.Errorf("create blades client for agent %s: %w", agentID, err)
	}

	f.agentOvMu.Lock()
	if f.agentOverrides == nil {
		f.agentOverrides = make(map[string]agentOverride)
	}
	f.agentOverrides[agentID] = agentOverride{roleID: roleID, modelID: modelID, cfg: newCfg, client: client}
	f.agentOvMu.Unlock()
	return newCfg, nil
}

// resolveThinking 计算实例覆盖的思考档：显式 thinking 优先，空则沿用角色当前生效档
// （含 role_bindings 覆盖），避免静默回退到 roles.yaml 配置。
func resolveThinking(f *ModelFactory, roleID, thinking string) string {
	if strings.TrimSpace(thinking) != "" {
		return thinking
	}
	if cur, err := f.resolveConfig(roleID); err == nil {
		return cur.Thinking
	}
	return ""
}

// ClearAgentModel 回收实例级模型覆盖（节点死亡/取消/槽销毁时调用；无覆盖为 no-op）。
func (f *ModelFactory) ClearAgentModel(agentID string) {
	if agentID == "" {
		return
	}
	f.agentOvMu.Lock()
	delete(f.agentOverrides, agentID)
	f.agentOvMu.Unlock()
}

// ClearAgentModelsByPrefix 按前缀回收实例级覆盖（会话清理兜底：话题切换/会话删除）。
func (f *ModelFactory) ClearAgentModelsByPrefix(prefix string) {
	if prefix == "" {
		return
	}
	f.agentOvMu.Lock()
	for id := range f.agentOverrides {
		if strings.HasPrefix(id, prefix) {
			delete(f.agentOverrides, id)
		}
	}
	f.agentOvMu.Unlock()
}

// agentOverrideOf 读取单个实例覆盖（无则 exists=false）。
func (f *ModelFactory) agentOverrideOf(agentID string) (agentOverride, bool) {
	if agentID == "" {
		return agentOverride{}, false
	}
	f.agentOvMu.RLock()
	ov, ok := f.agentOverrides[agentID]
	f.agentOvMu.RUnlock()
	return ov, ok
}

// AgentOverrides 返回当前实例级覆盖清单（展示用；按 AgentID 排序保证输出稳定）。
func (f *ModelFactory) AgentOverrides() []AgentOverrideInfo {
	f.agentOvMu.RLock()
	out := make([]AgentOverrideInfo, 0, len(f.agentOverrides))
	for id, ov := range f.agentOverrides {
		out = append(out, AgentOverrideInfo{
			AgentID: id, RoleID: ov.roleID, ModelID: ov.modelID,
			Model: ov.cfg.Model, Thinking: ov.cfg.Thinking,
		})
	}
	f.agentOvMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// isSwitchableRole 判断角色是否在可动态切换清单内（meta/domain/lightweight + 固定角色）。
func (f *ModelFactory) isSwitchableRole(roleID string) bool {
	for _, r := range f.SwitchableRoles() {
		if r == roleID {
			return true
		}
	}
	return false
}

// RegisterDynamicModelConfig 注册运行时动态角色的模型配置（P3-4）。
// 由 RoleFactory 在创建动态 Domain/Assistant/SubDomain 角色后调用；
// 当 GetModel 遇到非 meta/domain/lightweight/FixedRole 的角色 ID 时，优先使用此处注册的配置，
// 未注册再回退到 DomainAgent 模型。
//
// 参数：
//   - roleDefID: 动态角色 ID
//   - cfg: 该角色的模型配置
//
// 并发安全：内部持锁。
func (f *ModelFactory) RegisterDynamicModelConfig(roleDefID string, cfg types.AgentModelConfig) {
	// 加独立写锁保护 dynamicConfigs（与 f.mu 分离，resolveConfig 在 f.mu 写锁内读取此 map）
	f.dynMu.Lock()
	// 退出时释放写锁
	defer f.dynMu.Unlock()
	// 防御性初始化 map
	if f.dynamicConfigs == nil {
		f.dynamicConfigs = make(map[string]types.AgentModelConfig)
	}
	// 注册动态角色配置
	f.dynamicConfigs[roleDefID] = cfg
}

// GetMetaModel 获取 MetaAgent 的模型。
// 设计意图：MetaAgent 是图入口节点，使用独立（通常更轻量）的模型配置。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetMetaModel(ctx context.Context) (LLMClient, error) {
	// "meta" 为 MetaAgent 的固定缓存键
	return f.GetModel(ctx, "meta")
}

// GetDomainModel 获取 DomainAgent 的模型（所有 DomainAgent 共用）。
// 设计意图：DomainAgent 数量可变，统一复用同一模型配置以减少开销。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetDomainModel(ctx context.Context) (LLMClient, error) {
	// "domain" 为 Domain/SubDomain 共用缓存键
	return f.GetModel(ctx, "domain")
}

// GetLightweightModel 获取轻量模型，用于历史总结/检索 query 改写等低开销任务。
// 设计意图：与主对话模型解耦，可指向更便宜更快的模型，降低高频小任务的成本与延迟。
// 未配置 lightweight_model 时回退到 DomainAgent 模型（见 resolveConfig）。
// 并发安全：委托 GetModel。
func (f *ModelFactory) GetLightweightModel(ctx context.Context) (LLMClient, error) {
	// "lightweight" 为轻量模型固定缓存键
	return f.GetModel(ctx, "lightweight")
}

// resolveConfig 根据角色ID解析生效模型配置（含注册表合并）。
//
// 连接参数（provider/model/api_key/base_url）合并序：
//  1. role_bindings[roleID] 存在 → 取注册表 models[binding.model_id]
//  2. 否则角色配置 model_ref 非空 → 取注册表 models[model_ref]
//  3. 否则用角色配置内联连接字段（roles.yaml 直填，测试夹具场景）
//
// 行为参数：temperature/max_tokens 恒取角色配置；thinking = binding.thinking（非空）
// > 角色 thinking。base 为 meta/domain/lightweight/dynamic/fixed/回退各分支的
// 角色侧配置；连接字段缺失（model_ref 解析不到）返回 error。
//
// 并发安全：registry 内部持锁；f.cfg 只读。
func (f *ModelFactory) resolveConfig(roleDefID string) (types.AgentModelConfig, error) {
	base := f.resolveBaseConfig(roleDefID)
	// 1. 运行时绑定优先（动态切换角色模型，持久化于 models.json role_bindings）。
	// 绑定目标缺失时忽略绑定回落角色自身配置（fail-open，对齐旧 overrides 缺预设
	// 跳过语义；启动期由 bootstrap 记警告）。
	if b, ok := f.bindingFor(roleDefID); ok {
		if entry, found := f.entryByID(b.ModelID); found {
			base = applyModelEntry(base, entry)
			if b.Thinking != "" {
				base.Thinking = b.Thinking
			}
			return base, nil
		}
	}
	// 2. 角色引用注册表条目（model_ref）
	if base.ModelRef != "" {
		entry, found := f.entryByID(base.ModelRef)
		if !found {
			return types.AgentModelConfig{}, fmt.Errorf("role %s 的 model_ref %q 不在模型注册表中", roleDefID, base.ModelRef)
		}
		base = applyModelEntry(base, entry)
	}
	// 3. 内联连接字段已在 base 中，直接生效
	return base, nil
}

// resolveBaseConfig 解析角色侧基础配置（未合并注册表）：含 model_ref/内联连接字段 +
// 行为参数。registry 为 nil 或无绑定时不做任何注册表访问。
func (f *ModelFactory) resolveBaseConfig(roleDefID string) types.AgentModelConfig {
	switch roleDefID {
	case "meta":
		// MetaAgent 专用配置
		return f.cfg.MetaAgent.ModelConfig
	case "domain":
		// DomainAgent 共用配置
		return f.cfg.DomainAgent.ModelConfig
	case "lightweight":
		// 轻量模型：用于历史总结/检索 query 改写等低开销任务。
		// 未配置时回退到 DomainAgent 配置，保证启动不中断。
		if f.cfg.LightweightModel.Model != "" || f.cfg.LightweightModel.ModelRef != "" {
			return f.cfg.LightweightModel
		}
		return f.cfg.DomainAgent.ModelConfig
	default:
		// 1. 运行时动态角色配置（P3-4）：RoleFactory 创建动态角色后注册到 ModelFactory
		f.dynMu.RLock()
		cfg, ok := f.dynamicConfigs[roleDefID]
		f.dynMu.RUnlock()
		if ok {
			return cfg
		}
		// 2. 查找固定角色配置（roles.yaml 中显式定义的角色）
		if role := f.cfg.GetFixedRole(roleDefID); role != nil {
			return role.ModelConfig
		}
		// 3. 回退到 DomainAgent 配置：动态生成的助手角色默认沿用 Domain 模型
		return f.cfg.DomainAgent.ModelConfig
	}
}

// applyModelEntry 用注册表条目的连接参数覆盖 base 的连接字段（行为参数保留角色侧）。
// 输出上限：角色侧 max_tokens 显式配置（>0）优先；未配置时取条目 max_output_tokens，
// 两者皆无则保持 0（provider 省略/回退端点默认）。调用方（探活）在合并后覆写
// MaxTokens=1，勿把本函数移到探活赋值之后。
func applyModelEntry(base types.AgentModelConfig, e types.ModelEntry) types.AgentModelConfig {
	base.Provider = e.Provider
	base.Model = e.Model
	base.APIKey = e.APIKey
	base.BaseURL = e.BaseURL
	if base.MaxTokens <= 0 && e.MaxOutputTokens > 0 {
		base.MaxTokens = e.MaxOutputTokens
	}
	return base
}

// bindingFor / entryByID：registry nil 安全的读取封装。
func (f *ModelFactory) bindingFor(roleDefID string) (config.RoleBinding, bool) {
	if f.registry == nil {
		return config.RoleBinding{}, false
	}
	return f.registry.Binding(roleDefID)
}

func (f *ModelFactory) entryByID(id string) (types.ModelEntry, bool) {
	if f.registry == nil {
		return types.ModelEntry{}, false
	}
	return f.registry.ModelByID(id)
}

// checkRegistryReload 热更新检查：models.json mtime 变化则重载，并对缓存中每个角色
// 重算生效配置，配置变化（或解析失败）才删除缓存条目。文件损坏时保留旧数据继续服务。
// 轻量操作：文件未变时仅一次 stat。
func (f *ModelFactory) checkRegistryReload() {
	if f.registry == nil {
		return
	}
	changed, err := f.registry.MaybeReload()
	if err != nil || !changed {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for roleID, c := range f.models {
		newCfg, err := f.resolveConfig(roleID)
		if err != nil || newCfg != c.cfg {
			delete(f.models, roleID)
		}
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
	// 全部预热成功
	return nil
}

// probePrompt 连通性探测用的最小化提示词（廉价，约 1 token）。
const probePrompt = "ping"

// VerifyConnectivity 启动期对每个已配置模型角色发起一次最小化真实 LLM 调用，验证可连通性。
//
// 职责（P0-1）：
//   - 候选角色 = meta/domain/lightweight + 每个 fixed_roles[].ID
//   - 按 (provider,model,apikey,baseURL) 去重：roles.yaml 多角色常共享同一后端，去重后仅探测一次
//   - APIKey 为空视为未配置，加入失败列表，不允许 Mock/无 key 跳过启动
//   - 每个唯一后端用 probeLLM 探测（3 次重试，短超时）
//
// 参数：
//   - ctx: 上下文
//
// 返回：
//   - error: 任一角色不可达或未配置时返回聚合错误（列出全部失败角色），全部可达则返回 nil
//
// 副作用：可能触发 GetModel 缓存填充（与 WarmUp 重叠，幂等）。
// 并发安全：GetModel 内部锁保护。
func (f *ModelFactory) VerifyConnectivity(ctx context.Context) error {
	// 候选角色列表：三个内置角色 + 全部固定角色
	roles := []string{"meta", "domain", "lightweight"}
	// 遍历固定角色配置，追加非空 ID
	for _, fr := range f.cfg.FixedRoles {
		if fr.ID != "" {
			roles = append(roles, fr.ID)
		}
	}

	// 按 (provider,model,key,baseURL) 去重，避免同一后端重复探测
	seen := make(map[string]bool)
	// 收集所有失败角色的描述
	var failed []string
	// 遍历每个候选角色
	for _, roleID := range roles {
		// 解析该角色的模型配置（含注册表合并；model_ref 缺失即失败）
		cfg, err := f.resolveConfig(roleID)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", roleID, err))
			continue
		}
		// 严格启动：未配置 API Key 视为不可达，不跳过。
		if cfg.APIKey == "" {
			failed = append(failed, fmt.Sprintf("%s (api_key not configured)", roleID))
			// 跳过本次循环，继续检查下一个角色
			continue
		}
		// 用 \x00 拼接配置字段作为去重 key
		key := cfg.Provider + "\x00" + cfg.Model + "\x00" + cfg.APIKey + "\x00" + cfg.BaseURL
		// 若同一后端已探测，则跳过
		if seen[key] {
			continue
		}
		// 标记该后端已探测
		seen[key] = true

		// 构造一个 MaxTokens=1 的临时探测客户端：连通性探测只需模型"能应答"，
		// 用极小输出预算避免推理类模型（如 deepseek-v4-flash）为 "ping" 生成大段
		// reasoning 而拖慢/超时启动校验。正常 LLM 在 ~1s 内返回；错误配置（key/
		// 端点/模型名）通常在 <1s 内返回 4xx，仍能快速失败。
		probeCfg := cfg
		probeCfg.MaxTokens = 1
		// 创建临时探测客户端
		probeClient, err := NewBladesClient(ctx, probeCfg)
		if err != nil {
			// 构造失败计入失败列表
			failed = append(failed, fmt.Sprintf("%s (构造失败: %v)", roleID, err))
			// 继续探测下一个角色
			continue
		}
		// 探测：整体 150s（容纳 3 次重试 ×30s 单次 + 退避 + 慢网关冷启动），单次 30s
		// deepseek-v4-flash 等推理类模型首包冷启动可能 >12s；gemini-flash 经
		// gemini-webapi 网关单呼实测 ~10s（2026-09-08），原 60s 总窗会截断重试链
		probeCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
		// 发起最小化调用验证连通性
		if err := probeLLM(probeCtx, probeClient); err != nil {
			// 探测失败，记录失败原因
			failed = append(failed, fmt.Sprintf("%s (model=%s: %v)", roleID, cfg.Model, err))
		}
		// 释放探测上下文
		cancel()
	}

	// 若存在失败角色，返回聚合错误
	if len(failed) > 0 {
		return fmt.Errorf("LLM 连通性校验未通过: %s", strings.Join(failed, "; "))
	}
	// 全部通过
	return nil
}

// probeLLM 对已构造的探测客户端发起一次最小化调用（2 次重试，单次 30s），验证可连通性。
// 注：传入的 client 应已用 MaxTokens=1 构造，确保正常 LLM 快速应答。
//
// 参数：
//   - ctx: 上下文
//   - client: LLM 客户端
//
// 返回：nil 表示连通，非 nil 表示失败。
func probeLLM(ctx context.Context, client LLMClient) error {
	// 调用 retryGenerate 进行最多 3 次尝试（含退避）
	_, err, _ := retryGenerate(ctx, client, probePrompt, 30*time.Second)
	return err
}

// CallLightweightWithRetry 用轻量模型生成（带 3 次重试）。
//
// 设计意图（P0-1）：reflectOnResult / summarizeHistoryForGoal 等轻量直连 callers
// 原本绕过 tracker 直调 llm.Generate 无重试；统一收敛到本方法，落实"轻量级总结模型优化"。
// per-attempt 超时 30s，整体取消由 ctx 控制。
//
// 决策固化（TODO #33）：本方法走流式累积（retryStreamGenerate）——方舟 coding 端点
// 对"可能超过 10 分钟的操作"拒绝非流式 POST（2026-08-10 事故：5 组 Generate
// exhausted retries，打捞/摘要/事实提取全挂降级），流式是长任务端点的事实要求；
// 非流式仅作为客户端无流式实现（测试 fake）时的回退。
//
// 参数：
//   - ctx: 上下文
//   - prompt: 提示词
//
// 返回：
//   - string: 模型回复
//   - error: 调用错误
func (f *ModelFactory) CallLightweightWithRetry(ctx context.Context, prompt string) (string, error) {
	// 获取轻量模型客户端
	llm, err := f.GetLightweightModel(ctx)
	if err != nil {
		// 获取失败直接返回错误
		return "", err
	}
	// 流式累积 + 重试，单次超时 30 秒
	start := time.Now()
	resp, meta, err, _ := retryStreamGenerate(ctx, llm, prompt, 30*time.Second)
	// 写 session_logs（同普通调用 schema，Meta.layer=lightweight 可区分）；
	// 补齐评测耗时归因缺口：轻量调用此前不产生 llm_input/llm_output 记录。
	f.logLightweightCall(ctx, prompt, resp, meta, err, time.Since(start))
	return resp, err
}

// logLightweightCall 把一次轻量 LLM 调用写入 session_logs（复用 Logger.LLMCall 路径）。
// ctx 未携带会话 logger（启动期/无会话场景）时跳过，不影响主流程。
// 模型名取 LightweightResolution 的解析结果（与 GetLightweightModel 同一解析逻辑）；
// token 为估算值（轻量链路走流式累积，blades.TokenUsage 未透传），
// 但缓存命中/未命中 token 经流式末块 Metadata 透传（cache_hit/miss_tokens，TODO #40），
// 使轻量调用（事件摘要/打捞/L2 仲裁）纳入缓存命中率统计。
func (f *ModelFactory) logLightweightCall(ctx context.Context, prompt, resp string, meta map[string]any, callErr error, dur time.Duration) {
	lg := logger.FromContext(ctx)
	if lg == nil {
		return
	}
	if callErr != nil {
		resp = "[ERROR] " + callErr.Error() + "\n" + resp
	}
	var cacheHit, cacheMiss int
	if meta != nil {
		if v, ok := meta["cache_hit_tokens"].(int64); ok {
			cacheHit = int(v)
		}
		if v, ok := meta["cache_miss_tokens"].(int64); ok {
			cacheMiss = int(v)
		}
	}
	cfg, _ := f.LightweightResolution()
	lg.LLMCall(ctx, logger.LLMCallRecord{
		Agent:           "lightweight",
		Model:           cfg.Model,
		Prompt:          prompt,
		Response:        resp,
		InputTokens:     EstimateTokens(prompt),
		OutputTokens:    EstimateTokens(resp),
		CacheHitTokens:  cacheHit,
		CacheMissTokens: cacheMiss,
		LatencyMs:       int(dur.Milliseconds()),
		Meta:            map[string]any{"layer": "lightweight"},
	})
}

// LightweightResolution 返回轻量模型解析结果与来源，供启动日志排查配置加载。
// 2026-08-10 事故：roles.yaml 配 lightweight_model.model=deepseek-v4-flash，
// 但运行时重试日志 provider=glm-5.2（=domain 模型）——生效配置与磁盘现值不一致
// （配置晚于会话加载 / CWD 路径漂移），启动时打印解析结果可当场暴露此类漂移。
// 来源："binding"（models.json role_bindings 覆写）/ "direct" / "fallback-domain"。
func (f *ModelFactory) LightweightResolution() (cfg types.AgentModelConfig, source string) {
	if _, ok := f.bindingFor("lightweight"); ok {
		source = "binding"
	} else if f.cfg.LightweightModel.Model != "" || f.cfg.LightweightModel.ModelRef != "" {
		source = "direct"
	} else {
		source = "fallback-domain"
	}
	cfg, _ = f.resolveConfig("lightweight")
	return cfg, source
}

// ModelStatus 角色当前生效模型的状态摘要（目录展示用，不含 api_key）。
type ModelStatus struct {
	Provider string // 连接供应方
	Model    string // 模型名
	ModelID  string // 生效来源条目 ID（绑定或 model_ref）；内联配置为空
	Thinking string // 生效思考档位（原始值）
	Bound    bool   // 是否存在运行时绑定（role_bindings）
}

// SwitchableRoles 返回可动态切换模型的角色 ID 列表：
// meta / domain / lightweight + 全部 fixed_roles[].id。动态角色不在范围内。
func (f *ModelFactory) SwitchableRoles() []string {
	roles := []string{"meta", "domain", "lightweight"}
	for _, fr := range f.cfg.FixedRoles {
		if fr.ID != "" {
			roles = append(roles, fr.ID)
		}
	}
	return roles
}

// CurrentModelInfo 返回角色当前生效的模型状态（含绑定/model_ref 来源，不含 api_key）。
// 供 StatusHandler 与模型目录展示；unknown roleID 按默认解析链返回（Bound=false）。
func (f *ModelFactory) CurrentModelInfo(roleID string) (ModelStatus, error) {
	_, bound := f.bindingFor(roleID)
	cfg, err := f.resolveConfig(roleID)
	if err != nil {
		return ModelStatus{}, err
	}
	st := ModelStatus{Provider: cfg.Provider, Model: cfg.Model, Thinking: cfg.Thinking, Bound: bound}
	if b, ok := f.bindingFor(roleID); ok {
		st.ModelID = b.ModelID
	} else if cfg.ModelRef != "" {
		st.ModelID = cfg.ModelRef
	}
	return st, nil
}

// SwitchModel 把角色切换到指定模型条目并可选覆盖思考强度（运行时动态切换核心入口）。
//
// 流程：串行锁 → 热更新检查 → 校验 → 同绑定短路 → 连通性探测（fail-closed，
// 失败三处全不动）→ 先持久化 models.json role_bindings 再失效客户端缓存。
//
// 行为参数语义：切换只换连接参数（registry 条目），角色的 temperature/max_tokens
// 保留；thinking 非空时作为该角色的持久思考档覆盖（空 = 回落角色配置）。
//
// 生效语义：下一个 LLM 客户端获取点生效——domain/叶子下一次派发、lightweight/judge
// 下一次调用、meta 下一会话（provider 每会话注入一次）；进行中任务不中断。
//
// 返回生效的模型配置；错误时状态不变。
func (f *ModelFactory) SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	f.switchMu.Lock()
	defer f.switchMu.Unlock()

	// 校验角色可切换
	if !f.isSwitchableRole(roleID) {
		return types.AgentModelConfig{}, fmt.Errorf("role %q 不支持动态切换模型", roleID)
	}
	// 热更新检查（switchMu 下，防切换到刚被删掉的条目）
	f.checkRegistryReload()
	// 校验目标条目存在
	entry, ok := f.entryByID(modelID)
	if !ok {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 不在模型注册表中", modelID)
	}
	// 同绑定短路（目标模型与思考档均已生效则无操作）
	if b, ok := f.bindingFor(roleID); ok && b.ModelID == modelID && b.Thinking == thinking {
		return f.resolveConfig(roleID)
	}
	// 角色不在条目允许表内则拒绝（selectable_roles 白名单；绑定/model_ref 不受限）。
	if !entry.SwitchableFor(roleID) {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 仅限角色 %v 切换使用，角色 %q 不可选用", modelID, entry.SelectableRoles, roleID)
	}

	// 计算切换后的生效配置：角色行为参数 + 目标条目连接参数 + 思考覆盖
	newCfg := applyModelEntry(f.resolveBaseConfig(roleID), entry)
	if thinking != "" {
		newCfg.Thinking = thinking
	}
	if newCfg.APIKey == "" {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 未配置 api_key", modelID)
	}
	// 连通性探测（锁外网络 IO；MaxTokens=1 最小化开销，参照启动期 VerifyConnectivity）
	probeCfg := newCfg
	probeCfg.MaxTokens = 1
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := f.probe(probeCtx, probeCfg); err != nil {
		return types.AgentModelConfig{}, fmt.Errorf("模型 %q 连通性探测失败，保持原模型: %w", modelID, err)
	}

	// 先持久化绑定（失败则内存不动，无半状态）
	if err := f.registry.SetBinding(roleID, modelID, thinking); err != nil {
		return types.AgentModelConfig{}, fmt.Errorf("持久化模型绑定失败，保持原模型: %w", err)
	}

	// 失效缓存：重算所有缓存角色的生效配置，变化才删（覆盖 lightweight-domain 回退边）
	f.invalidateChangedClients()
	return newCfg, nil
}

// AddModel 新增模型条目到注册表并落盘（TUI /model 表单与 Web 新增对话框共用）。
// id 为空时由 model 名 slug 化生成并自动去重（TUI 表单无 id 字段）。
func (f *ModelFactory) AddModel(entry types.ModelEntry) error {
	f.switchMu.Lock()
	defer f.switchMu.Unlock()
	if f.registry == nil {
		return fmt.Errorf("模型注册表未启用")
	}
	if entry.ID == "" {
		entry.ID = f.generateModelID(entry.Model)
	}
	return f.registry.Add(entry)
}

// generateModelID 由 model 名生成条目 ID（小写、非法字符转 -），与注册表已有条目
// 冲突时追加 -2/-3 后缀。调用方需持 switchMu（或单线程路径）。
func (f *ModelFactory) generateModelID(modelName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(modelName)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "model"
	}
	if _, taken := f.registry.ModelByID(base); !taken {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, taken := f.registry.ModelByID(candidate); !taken {
			return candidate
		}
	}
}

// RegistryModels 返回模型注册表清单副本（目录展示用）；registry 未启用返回 nil。
func (f *ModelFactory) RegistryModels() []types.ModelEntry {
	if f.registry == nil {
		return nil
	}
	return f.registry.Models()
}

// invalidateChangedClients 重算缓存中各角色的生效配置，变化（或解析失败）才删除缓存。
// 调用方需已持有 switchMu 或在单线程切换路径中；内部短持 f.mu 写锁。
func (f *ModelFactory) invalidateChangedClients() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for roleID, c := range f.models {
		newCfg, err := f.resolveConfig(roleID)
		if err != nil || newCfg != c.cfg {
			delete(f.models, roleID)
		}
	}
}

// probe 执行切换前连通性探测；probeHook 非空用注入实现，否则默认临时客户端。
func (f *ModelFactory) probe(ctx context.Context, probeCfg types.AgentModelConfig) error {
	if f.probeHook != nil {
		return f.probeHook(ctx, probeCfg)
	}
	client, err := NewBladesClient(ctx, probeCfg)
	if err != nil {
		return err
	}
	return probeLLM(ctx, client)
}
