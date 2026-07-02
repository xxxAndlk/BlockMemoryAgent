package types

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// Event 工作区事件：跨 Agent 协作的原子语义消息，由 Mailbox 分发。
type Event struct {
	// ID 事件唯一标识。
	ID string `json:"id"`
	// Type 事件类型，决定消费方的处理分支。
	Type enums.EventType `json:"type"`
	// SourceAgent 发起事件的 Agent ID。
	SourceAgent string `json:"source_agent"`
	// TargetAgent 目标 Agent ID；空表示广播。
	TargetAgent string `json:"target_agent"`
	// Payload 事件负载，结构由 Type 决定。
	Payload map[string]any `json:"payload"`
	// Priority 优先级（数值越大越优先），影响 Mailbox 投递顺序。
	Priority int `json:"priority"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// Status 当前处理状态。
	Status enums.EventStatus `json:"status"`
}

// ChatMessage 对话消息：与用户/系统对话的逐条记录，用于上下文构造。
type ChatMessage struct {
	// Role 角色：ChatRoleSystem / ChatRoleUser / ChatRoleAssistant。
	Role enums.ChatRole `json:"role"`
	// Content 消息正文。
	Content string `json:"content"`
	// Timestamp 消息时间。
	Timestamp time.Time `json:"timestamp"`
}

// ClarifyRequest 人机对话请求：Agent 在执行中遇到需要用户确认的问题时挂起，
// 由 server 层通过 HTTP 暴露给前端，用户答复后回填 Answer 并恢复 graph。
type ClarifyRequest struct {
	ID         string     `json:"id"`                    // 请求唯一 ID（用于答复对齐）
	Question   string     `json:"question"`              // Agent 提给用户的问题
	Context    string     `json:"context"`               // 触发澄清的上下文摘要（便于用户理解）
	AgentID    string     `json:"agent_id"`              // 发起澄清的 Agent 实例 ID
	CreatedAt  time.Time  `json:"created_at"`            // 创建时间
	Answer     string     `json:"answer,omitempty"`      // 用户答复（回填）
	AnsweredAt *time.Time `json:"answered_at,omitempty"` // 答复时间
}

// GraphState 三层图共享状态：早期版本的全局状态载体（向后兼容保留）。
// 运行时主路径使用 ThreeLayerState，本结构用于事件序列化与外部 API。
type GraphState struct {
	// TopicID 当前话题 ID。
	TopicID string `json:"topic_id"`
	// TopicGoal 话题目标描述。
	TopicGoal string `json:"topic_goal"`
	// Constraints 话题级约束（键值对）。
	Constraints map[string]string `json:"constraints"`
	// EventQueue 待处理事件队列。
	EventQueue []*Event `json:"event_queue"`
	// CurrentAgent 当前活跃 Agent ID。
	CurrentAgent string `json:"current_agent"`
	// AgentOutputs 各 Agent 的最新公开输出。
	AgentOutputs map[string]*AgentOutput `json:"agent_outputs"`
	// SnapshotRefs 各 Agent 的快照存储键。
	SnapshotRefs map[string]string `json:"snapshot_refs"`
	// NextAction 下一步控制信号。
	NextAction enums.ActionType `json:"next_action"`
	// TargetAgent Switch 动作的目标 Agent ID。
	TargetAgent string `json:"target_agent"`
	// Reason 本次动作的理由，供审计与调试。
	Reason string `json:"reason"`
}

// UIEvent TUI 推送事件：向 bubbletea TUI / Web SSE 订阅者广播的事件信封。
type UIEvent struct {
	// Type 事件类型字符串，由发送方约定。
	Type string `json:"type"`
	// Timestamp 事件时间。
	Timestamp time.Time `json:"timestamp"`
	// Payload 事件负载，结构由 Type 决定。
	Payload any `json:"payload"`
}

// GraphStepPayload Graph 步骤事件负载：描述一次 Node 执行的统计信息。
type GraphStepPayload struct {
	// NodeName 执行节点名（如 MetaAgent / DomainAgent）。
	NodeName string `json:"node_name"`
	// AgentID 关联 Agent ID，可空。
	AgentID string `json:"agent_id,omitempty"`
	// Duration 执行耗时。
	Duration time.Duration `json:"duration"`
	// InputSize 输入 Token 数。
	InputSize int `json:"input_tokens"`
	// OutputSize 输出 Token 数。
	OutputSize int `json:"output_tokens"`
	// Action 本次节点产出的控制动作，可空。
	Action string `json:"action,omitempty"`
}

// AgentStatusPayload Agent 状态事件负载：向 UI 广播 Agent 运行态变化。
type AgentStatusPayload struct {
	// AgentID Agent 实例 ID。
	AgentID string `json:"agent_id"`
	// State 当前状态字符串（与 RoleStatus 对齐）。
	State string `json:"state"`
	// CurrentStep 当前步骤序号，可空。
	CurrentStep int `json:"current_step,omitempty"`
	// LastOutput 最近一次输出摘要，可空。
	LastOutput string `json:"last_output,omitempty"`
}

// EpisodePayload Episode 事件负载：向 UI 广播新增 Episode 的精简信息。
type EpisodePayload struct {
	// AgentID 产出 Episode 的 Agent。
	AgentID string `json:"agent_id"`
	// StepID 步骤 ID。
	StepID string `json:"step_id"`
	// Importance 重要性评分。
	Importance float64 `json:"importance"`
	// Summary 摘要文本。
	Summary string `json:"summary"`
	// Facts 事实列表。
	Facts []string `json:"facts"`
	// Timestamp 步骤时间。
	Timestamp time.Time `json:"timestamp"`
	// ToolCalls 工具调用明细，可空。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// EventPayload Event 事件负载：向 UI 广播工作区事件的精简信息。
type EventPayload struct {
	// ID 事件 ID。
	ID string `json:"id"`
	// Type 事件类型。
	Type enums.EventType `json:"type"`
	// Summary 事件摘要。
	Summary string `json:"summary"`
	// Status 事件状态。
	Status enums.EventStatus `json:"status"`
	// Priority 优先级。
	Priority int `json:"priority"`
}

// StatsView 统计面板数据：供 TUI / Web 展示的运行时统计快照。
type StatsView struct {
	// TopicID 所属话题。
	TopicID string `json:"topic_id"`
	// PrivateEpisodes 私有 Episode 总数。
	PrivateEpisodes int `json:"private_episodes"`
	// CompressedL1 压缩到 LevelStandard 的 Episode 数。
	CompressedL1 int `json:"compressed_l1"`
	// CompressedL2 压缩到 LevelCompact 及以上的 Episode 数。
	CompressedL2 int `json:"compressed_l2"`
	// GlobalKBHits 全局知识库命中次数。
	GlobalKBHits int `json:"global_kb_hits"`
	// TokenBudgetUsed 已用 Token 预算。
	TokenBudgetUsed int `json:"token_budget_used"`
	// ActiveAgents 当前活跃 Agent 数。
	ActiveAgents int `json:"active_agents"`
	// TotalAgents Agent 总数。
	TotalAgents int `json:"total_agents"`
}
