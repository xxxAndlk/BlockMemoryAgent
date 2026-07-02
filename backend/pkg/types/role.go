package types

import (
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// RoleType 角色类型：四层 Agent 体系中的角色分类，决定实例化路径与生命周期管理。
// 实际定义见 pkg/enums/enums.go，此处保留 type alias 以向后兼容。
type RoleType = enums.RoleType

const (
	// RoleTypeMeta 主 Agent / MetaAgent：顶层调度者，负责会话级任务分解与升级裁决。
	RoleTypeMeta = enums.RoleTypeMeta
	// RoleTypeDomain 会话块 Agent / DomainAgent：负责单一领域的子任务执行与会话块管理。
	RoleTypeDomain = enums.RoleTypeDomain
	// RoleTypeSubDomain 子领域 Agent / SubDomainAgent：DomainAgent 进一步拆分的子领域执行者。
	RoleTypeSubDomain = enums.RoleTypeSubDomain
	// RoleTypeFixed 固定助手角色：由配置文件（roles.yaml）预定义，长期可复用。
	RoleTypeFixed = enums.RoleTypeFixed
	// RoleTypeDynamic 动态助手角色：由 LLM 在运行时按需创建，随任务结束消亡。
	RoleTypeDynamic = enums.RoleTypeDynamic
)

// RoleLifecycle 角色生命周期：刻画角色实例的存活时长与回收策略。
// 实际定义见 pkg/enums/enums.go，此处保留 type alias 以向后兼容。
type RoleLifecycle = enums.RoleLifecycle

const (
	// RoleLifecyclePermanent 永久型：固定角色，跨会话长期存在。
	RoleLifecyclePermanent = enums.RoleLifecyclePermanent
	// RoleLifecycleSession 会话级：随会话结束自动消亡。
	RoleLifecycleSession = enums.RoleLifecycleSession
	// RoleLifecycleTask 任务级：单次任务完成后即回收。
	RoleLifecycleTask = enums.RoleLifecycleTask
)

// RoleDefinition 角色定义（配置层面）：来自 roles.yaml 的静态角色模板。
// 描述"角色应当是什么样"，运行时由 RoleFactory 据此实例化为 RoleInstance。
type RoleDefinition struct {
	// ID 角色定义唯一标识。
	ID string `json:"id" yaml:"id"`
	// Name 人类可读名称。
	Name string `json:"name" yaml:"name"`
	// Type 角色类型。
	Type RoleType `json:"type" yaml:"type"`
	// Lifecycle 生命周期策略。
	Lifecycle RoleLifecycle `json:"lifecycle" yaml:"lifecycle"`
	// Description 角色职责描述，供 LLM 动态创建时参考。
	Description string `json:"description" yaml:"description"`
	// SystemPrompt 系统提示词，注入 Agent 上下文。
	SystemPrompt string `json:"system_prompt" yaml:"system_prompt"`
	// Keywords 关键字列表，用于领域匹配与角色筛选。
	Keywords []string `json:"keywords" yaml:"keywords"`
	// Skills 技能列表（Skill ID 列表），运行时与 SkillPool 取交集。
	Skills []string `json:"skills" yaml:"skills"`
	// Tools 可用工具 ID 列表，约束该角色可调用的工具集合。
	Tools []string `json:"tools" yaml:"tools"`
	// ModelConfig 模型配置（provider/model/温度等）。
	ModelConfig AgentModelConfig `json:"model_config" yaml:"model_config"`
	// CanBeCalled 是否可被其他 Agent 调用；false 表示纯调度型角色。
	CanBeCalled bool `json:"can_be_called" yaml:"can_be_called"`
	// Parents 可调用此角色的父角色 ID 列表，约束调用关系图。
	Parents []string `json:"parents" yaml:"parents"`
}

// AgentModelConfig Agent 模型配置：单个角色所需的 LLM 接入参数。
type AgentModelConfig struct {
	// Provider 模型供应方（如 openai / anthropic）。
	Provider string `json:"provider" yaml:"provider"`
	// Model 具体模型名（如 gpt-4o-mini）。
	Model string `json:"model" yaml:"model"`
	// APIKey 调用密钥；可空则回落到全局环境变量。
	APIKey string `json:"api_key" yaml:"api_key"`
	// BaseURL 自定义 API 端点（兼容 OpenAI 协议的代理）。
	BaseURL string `json:"base_url" yaml:"base_url"`
	// Temperature 采样温度，影响输出随机性。
	Temperature float64 `json:"temperature" yaml:"temperature"`
	// MaxTokens 单次响应最大 Token 数。
	MaxTokens int `json:"max_tokens" yaml:"max_tokens"`
}

// RoleInstance 角色实例（运行时）：RoleDefinition 的具体化，绑定到会话与领域。
type RoleInstance struct {
	// ID 实例唯一标识（与 RoleDefinition.ID 不同，运行时生成）。
	ID string `json:"id"`
	// RoleDefID 引用 RoleDefinition.ID，标识来源模板。
	RoleDefID string `json:"role_def_id"`
	// Type 角色类型。
	Type RoleType `json:"type"`
	// Lifecycle 生命周期策略。
	Lifecycle RoleLifecycle `json:"lifecycle"`
	// SessionID 所属会话 ID。
	SessionID string `json:"session_id"`
	// Domain 负责领域（如 "商城页面" / "订单服务"）。
	Domain string `json:"domain"`
	// Status 当前运行态。
	Status RoleStatus `json:"status"`
	// CreatedAt 实例创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt 过期时间，可空表示不过期；用于 session/task 生命周期回收。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// ParentID 创建此实例的父 Agent ID，构成调用树。
	ParentID string `json:"parent_id"`
	// Children 子角色实例 ID 列表。
	Children []string `json:"children"`
	// ContextRef 上下文引用（snapshot 存储键），指向私有快照。
	ContextRef string `json:"context_ref"`
}

// RoleStatus 角色状态：实例在运行时状态机中的当前阶段。
// 实际定义见 pkg/enums/enums.go，此处保留 type alias 以向后兼容。
type RoleStatus = enums.RoleStatus

const (
	// RoleStatusIdle 空闲：已创建但未开始执行。
	RoleStatusIdle = enums.RoleStatusIdle
	// RoleStatusActive 活跃：正在执行任务。
	RoleStatusActive = enums.RoleStatusActive
	// RoleStatusWaiting 等待：阻塞等待依赖/外部事件。
	RoleStatusWaiting = enums.RoleStatusWaiting
	// RoleStatusCalling 调用中：正在调用下层助手角色。
	RoleStatusCalling = enums.RoleStatusCalling
	// RoleStatusDone 完成：任务已成功结束。
	RoleStatusDone = enums.RoleStatusDone
	// RoleStatusError 错误：执行失败，需升级或重试。
	RoleStatusError = enums.RoleStatusError
)

// CallRequest 角色间调用请求：父 Agent 向子 Agent 发起的结构化调用。
type CallRequest struct {
	// ID 请求唯一标识。
	ID string `json:"id"`
	// CallerID 调用者角色实例 ID。
	CallerID string `json:"caller_id"`
	// CalleeID 被调用者角色实例 ID。
	CalleeID string `json:"callee_id"`
	// Task 任务描述，作为子 Agent 的目标输入。
	Task string `json:"task"`
	// Context 传递的上下文键值对，用于共享必要的中间状态。
	Context map[string]any `json:"context"`
	// Priority 优先级，影响调度顺序。
	Priority int `json:"priority"`
	// CreatedAt 请求创建时间。
	CreatedAt time.Time `json:"created_at"`
}

// CallResponse 角色间调用响应：被调用者执行完成后返回的结果。
type CallResponse struct {
	// RequestID 对应 CallRequest.ID。
	RequestID string `json:"request_id"`
	// Result 结果文本摘要。
	Result string `json:"result"`
	// Status 状态：CallResultSuccess / CallResultFailed。
	Status enums.CallResult `json:"status"`
	// Output 结构化公开输出，可空。
	Output *AgentOutput `json:"output,omitempty"`
}

// SessionBlock 会话块（DomainAgent 管理）：按领域隔离的上下文容器。
// 每个会话块绑定一个 DomainAgent，包含其子任务、事件与中间产物。
type SessionBlock struct {
	// ID 会话块唯一标识。
	ID string `json:"id"`
	// SessionID 所属会话 ID。
	SessionID string `json:"session_id"`
	// Domain 本块负责的领域名。
	Domain string `json:"domain"`
	// Goal 本块目标描述。
	Goal string `json:"goal"`
	// Status 本块状态（active / completed / failed）。
	Status enums.BlockStatus `json:"status"`
	// Agents 该会话块内的角色实例 ID 列表。
	Agents []string `json:"agents"`
	// Events 本块累积的工作区事件。
	Events []*Event `json:"events"`
	// TaskResults 已完成任务的结果映射（任务名 -> 结果摘要）。
	TaskResults map[string]string `json:"task_results"`
	// Plan 结构化执行计划（Plan-and-Execute，TODO #1）。多任务时由 DomainAgent 生成，按步骤派发助手。
	Plan *ExecutionPlan `json:"plan,omitempty"`
	// archived 块记忆是否已落库（幂等兜底用，TODO #4）。异步归档成功后置 true。
	// 用 atomic.Bool 因为异步归档 goroutine 写、图循环读，需无锁并发安全。不序列化（瞬态标志）。
	archived atomic.Bool `json:"-"`
	// SubDomainSplit 是否已拆分为子领域；true 表示进入第三层。
	SubDomainSplit bool `json:"subdomain_split"`
	// SubDomainList 子领域名称列表，拆分后顺序执行。
	SubDomainList []string `json:"subdomain_list"`
	// SubDomainIndex 当前处理到的子领域索引，支持断点续行。
	SubDomainIndex int `json:"subdomain_index"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt 最近更新时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// ThreeLayerState 三层架构全局状态：贯穿 MetaAgent / DomainAgent / Assistant 的运行时状态。
// 由 ThreeLayerGraph 在循环中读写，是各 Node 间传递的核心载体。
type ThreeLayerState struct {
	// ===== Layer 1: MetaAgent =====
	// SessionID 会话唯一标识。
	SessionID string `json:"session_id"`
	// SessionSummary 会话级信息总结，由 MetaAgent 维护。
	SessionSummary string `json:"session_summary"`
	// ActiveBlocks 当前活跃的会话块映射（ID -> SessionBlock）。
	ActiveBlocks map[string]*SessionBlock `json:"active_blocks"`
	// CompletedBlocks 已完成的会话块 ID 列表。
	CompletedBlocks []string `json:"completed_blocks"`

	// ===== Layer 2: DomainAgent（当前活跃的） =====
	// CurrentBlockID 当前活跃会话块 ID。
	CurrentBlockID string `json:"current_block_id"`
	// CurrentDomain 当前领域名。
	CurrentDomain string `json:"current_domain"`
	// DomainGoal 当前领域目标。
	DomainGoal string `json:"domain_goal"`

	// ===== Layer 3: Assistant（当前调用的） =====
	// CurrentAssistantID 当前正在调用的助手实例 ID。
	CurrentAssistantID string `json:"current_assistant_id"`
	// CallStack 调用栈，支持 DomainAgent→SubDomainAgent→Assistant 的嵌套调用。
	CallStack []*CallRequest `json:"call_stack"`

	// ===== 对话历史 =====
	// Messages 与用户/系统的对话历史，可空。
	Messages []ChatMessage `json:"messages,omitempty"`

	// ===== 全局 =====
	// RoleInstances 所有角色实例映射（ID -> RoleInstance）。
	RoleInstances map[string]*RoleInstance `json:"role_instances"`
	// NextAction 下一步控制信号，驱动状态机。
	NextAction enums.ActionType `json:"next_action"`
	// TargetRoleID Switch/Escalate 动作的目标角色 ID。
	TargetRoleID string `json:"target_role_id"`
	// Reason 本次动作的理由，供审计与调试。
	Reason string `json:"reason"`
	// DirectExecute 标记为"直接执行"模式：MetaAgent 判定为简单查询/搜索/分析类任务时置 true，
	// DomainAgent 见此标志跳过 LLM 子任务拆解，直接把 goal 作为单个子任务交给一个 Assistant。
	DirectExecute bool `json:"direct_execute"`
	// EnableSubdomain 是否允许 DomainAgent 自适应启用 SubDomain（第四层）。
	// 由 MetaAgent 路由判定：仅 RouteFullFourLayer 路径置 true；其余路径 false 以避免不必要的 overhead。
	EnableSubdomain bool `json:"enable_subdomain"`
	// PendingClarify 待处理的人机对话请求：非空表示 Graph 已挂起，等待用户答复后由 server 侧恢复。
	PendingClarify *ClarifyRequest `json:"pending_clarify,omitempty"`
}

// NewThreeLayerState 创建三层状态：初始化会话级映射与调用栈，默认动作 Continue。
// 参数：sessionID 会话 ID。返回：初始化后的 ThreeLayerState 指针。副作用：无。
func NewThreeLayerState(sessionID string) *ThreeLayerState {
	return &ThreeLayerState{
		SessionID:     sessionID,
		ActiveBlocks:  make(map[string]*SessionBlock),
		RoleInstances: make(map[string]*RoleInstance),
		CallStack:     make([]*CallRequest, 0),
		NextAction:    enums.ActionContinue,
	}
}

// GetRoleInstance 按 ID 获取角色实例。
// 参数：roleID 角色 ID。返回：实例指针，不存在则为 nil。副作用：无。
func (s *ThreeLayerState) GetRoleInstance(roleID string) *RoleInstance {
	return s.RoleInstances[roleID]
}

// AddRoleInstance 注册角色实例到全局映射。
// 参数：inst 角色实例。副作用：写入 s.RoleInstances[inst.ID]。
func (s *ThreeLayerState) AddRoleInstance(inst *RoleInstance) {
	s.RoleInstances[inst.ID] = inst
}

// GetActiveAssistants 获取当前可活跃的助手列表：类型为 fixed/dynamic 且状态为 idle/active。
// 返回：符合条件的角色实例切片。副作用：无。
func (s *ThreeLayerState) GetActiveAssistants() []*RoleInstance {
	var result []*RoleInstance
	for _, inst := range s.RoleInstances {
		if (inst.Type == RoleTypeFixed || inst.Type == RoleTypeDynamic) &&
			(inst.Status == RoleStatusIdle || inst.Status == RoleStatusActive) {
			result = append(result, inst)
		}
	}
	return result
}

// PushCallStack 压入调用栈：记录一次向助手的调用，并更新当前助手指针。
// 参数：req 调用请求。副作用：追加到 CallStack，更新 CurrentAssistantID。
func (s *ThreeLayerState) PushCallStack(req *CallRequest) {
	s.CallStack = append(s.CallStack, req)
	s.CurrentAssistantID = req.CalleeID
}

// PopCallStack 弹出调用栈：回收最顶层的调用，并回退当前助手指针到上一层。
// 返回：被弹出的 CallRequest；栈空时返回 nil。副作用：缩减 CallStack，重置 CurrentAssistantID。
func (s *ThreeLayerState) PopCallStack() *CallRequest {
	if len(s.CallStack) == 0 {
		return nil
	}
	req := s.CallStack[len(s.CallStack)-1]
	s.CallStack = s.CallStack[:len(s.CallStack)-1]
	if len(s.CallStack) > 0 {
		s.CurrentAssistantID = s.CallStack[len(s.CallStack)-1].CalleeID
	} else {
		s.CurrentAssistantID = ""
	}
	return req
}

// IsCalling 判断是否处于助手调用中（调用栈非空）。
// 返回：true 表示当前嵌套在助手调用链中。副作用：无。
func (s *ThreeLayerState) IsCalling() bool {
	return len(s.CallStack) > 0
}

// MarkArchived 标记块记忆已落库（异步归档成功后调用，幂等兜底用）。
func (b *SessionBlock) MarkArchived() {
	if b == nil {
		return
	}
	b.archived.Store(true)
}

// IsArchived 返回块记忆是否已落库（图循环在 switchToNextBlock 兜底归档前读取）。
func (b *SessionBlock) IsArchived() bool {
	if b == nil {
		return false
	}
	return b.archived.Load()
}
