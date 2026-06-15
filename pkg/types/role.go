package types

import (
	"time"
)

// RoleType 角色类型
type RoleType string

const (
	RoleTypeMeta      RoleType = "meta"      // 主Agent / MetaAgent
	RoleTypeDomain    RoleType = "domain"    // 会话块Agent / DomainAgent
	RoleTypeSubDomain RoleType = "subdomain" // 子领域Agent / SubDomainAgent
	RoleTypeFixed     RoleType = "fixed"     // 固定助手角色（配置文件定义）
	RoleTypeDynamic   RoleType = "dynamic"   // 临时助手角色（大模型动态创建）
)

// RoleLifecycle 角色生命周期
type RoleLifecycle string

const (
	RoleLifecyclePermanent RoleLifecycle = "permanent" // 固定角色，长期存在
	RoleLifecycleSession   RoleLifecycle = "session"   // 会话级角色，随会话结束消亡
	RoleLifecycleTask      RoleLifecycle = "task"      // 任务级角色，单次任务后消亡
)

// RoleDefinition 角色定义（配置层面）
type RoleDefinition struct {
	ID          string            `json:"id" yaml:"id"`
	Name        string            `json:"name" yaml:"name"`
	Type        RoleType          `json:"type" yaml:"type"`
	Lifecycle   RoleLifecycle     `json:"lifecycle" yaml:"lifecycle"`
	Description string            `json:"description" yaml:"description"`
	SystemPrompt string           `json:"system_prompt" yaml:"system_prompt"`
	Keywords    []string          `json:"keywords" yaml:"keywords"`
	Skills      []string          `json:"skills" yaml:"skills"`       // 技能列表
	Tools       []string          `json:"tools" yaml:"tools"`         // 可用工具ID
	ModelConfig AgentModelConfig  `json:"model_config" yaml:"model_config"`
	CanBeCalled bool              `json:"can_be_called" yaml:"can_be_called"` // 是否可被其他Agent调用
	Parents     []string          `json:"parents" yaml:"parents"`     // 可调用此角色的父角色ID列表
}

// AgentModelConfig Agent模型配置
type AgentModelConfig struct {
	Provider    string  `json:"provider" yaml:"provider"`
	Model       string  `json:"model" yaml:"model"`
	APIKey      string  `json:"api_key" yaml:"api_key"`
	BaseURL     string  `json:"base_url" yaml:"base_url"`
	Temperature float64 `json:"temperature" yaml:"temperature"`
	MaxTokens   int     `json:"max_tokens" yaml:"max_tokens"`
}

// RoleInstance 角色实例（运行时）
type RoleInstance struct {
	ID          string            `json:"id"`
	RoleDefID   string            `json:"role_def_id"`     // 引用RoleDefinition.ID
	Type        RoleType          `json:"type"`
	Lifecycle   RoleLifecycle     `json:"lifecycle"`
	SessionID   string            `json:"session_id"`      // 所属会话
	Domain      string            `json:"domain"`          // 负责领域（如"商城页面"）
	Status      RoleStatus        `json:"status"`
	CreatedAt   time.Time         `json:"created_at"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
	ParentID    string            `json:"parent_id"`       // 创建此实例的父Agent ID
	Children    []string          `json:"children"`        // 子角色实例ID列表
	ContextRef  string            `json:"context_ref"`     // 上下文引用（snapshot key）
}

// RoleStatus 角色状态
type RoleStatus string

const (
	RoleStatusIdle      RoleStatus = "idle"
	RoleStatusActive    RoleStatus = "active"
	RoleStatusWaiting   RoleStatus = "waiting"
	RoleStatusCalling   RoleStatus = "calling"   // 正在调用助手
	RoleStatusDone      RoleStatus = "done"
	RoleStatusError     RoleStatus = "error"
)

// CallRequest 角色间调用请求
type CallRequest struct {
	ID          string         `json:"id"`
	CallerID    string         `json:"caller_id"`       // 调用者角色实例ID
	CalleeID    string         `json:"callee_id"`       // 被调用者角色实例ID
	Task        string         `json:"task"`            // 任务描述
	Context     map[string]any `json:"context"`         // 传递的上下文
	Priority    int            `json:"priority"`
	CreatedAt   time.Time      `json:"created_at"`
}

// CallResponse 角色间调用响应
type CallResponse struct {
	RequestID   string         `json:"request_id"`
	Result      string         `json:"result"`
	Status      string         `json:"status"`          // success / failed
	Output      *AgentOutput   `json:"output,omitempty"`
}

// SessionBlock 会话块（DomainAgent管理）
type SessionBlock struct {
	ID              string            `json:"id"`
	SessionID       string            `json:"session_id"`
	Domain          string            `json:"domain"`
	Goal            string            `json:"goal"`
	Status          string            `json:"status"`
	Agents          []string          `json:"agents"`           // 该会话块内的角色实例ID
	Events          []*Event          `json:"events"`
	TaskResults     map[string]string `json:"task_results"`     // 已完成任务的结果映射
	SubDomainSplit  bool              `json:"subdomain_split"`  // 是否已拆分为子领域
	SubDomainList   []string          `json:"subdomain_list"`   // 子领域名称列表
	SubDomainIndex  int               `json:"subdomain_index"`  // 当前处理到的子领域索引
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// ThreeLayerState 三层架构全局状态
type ThreeLayerState struct {
	// Layer 1: MetaAgent
	SessionID      string                 `json:"session_id"`
	SessionSummary string                 `json:"session_summary"`    // 会话信息总结
	ActiveBlocks   map[string]*SessionBlock `json:"active_blocks"`     // 活跃的会话块
	CompletedBlocks []string              `json:"completed_blocks"`

	// Layer 2: DomainAgent（当前活跃的）
	CurrentBlockID string                 `json:"current_block_id"`
	CurrentDomain  string                 `json:"current_domain"`
	DomainGoal     string                 `json:"domain_goal"`

	// Layer 3: Assistant（当前调用的）
	CurrentAssistantID string             `json:"current_assistant_id"`
	CallStack          []*CallRequest     `json:"call_stack"`         // 调用栈，支持嵌套调用

	// 全局
	RoleInstances  map[string]*RoleInstance `json:"role_instances"`    // 所有角色实例
	NextAction     ActionType             `json:"next_action"`
	TargetRoleID   string                 `json:"target_role_id"`
	Reason         string                 `json:"reason"`
}

// NewThreeLayerState 创建三层状态
func NewThreeLayerState(sessionID string) *ThreeLayerState {
	return &ThreeLayerState{
		SessionID:      sessionID,
		ActiveBlocks:   make(map[string]*SessionBlock),
		RoleInstances:  make(map[string]*RoleInstance),
		CallStack:      make([]*CallRequest, 0),
		NextAction:     ActionContinue,
	}
}

// GetRoleInstance 获取角色实例
func (s *ThreeLayerState) GetRoleInstance(roleID string) *RoleInstance {
	return s.RoleInstances[roleID]
}

// AddRoleInstance 添加角色实例
func (s *ThreeLayerState) AddRoleInstance(inst *RoleInstance) {
	s.RoleInstances[inst.ID] = inst
}

// GetActiveAssistants 获取当前可活跃的助手列表
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

// PushCallStack 压入调用栈
func (s *ThreeLayerState) PushCallStack(req *CallRequest) {
	s.CallStack = append(s.CallStack, req)
	s.CurrentAssistantID = req.CalleeID
}

// PopCallStack 弹出调用栈
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

// IsCalling 判断是否处于助手调用中
func (s *ThreeLayerState) IsCalling() bool {
	return len(s.CallStack) > 0
}
