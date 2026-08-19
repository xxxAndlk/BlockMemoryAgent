package types

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// Event 工作区事件：跨 Agent 协作的原子语义消息，由 Mailbox 分发。
// 每个事件携带类型、来源、目标与负载，消费方按 Type 走不同处理分支。
type Event struct {
	// ID 事件唯一标识，用于去重、追踪与日志。
	ID string `json:"id"`
	// Type 事件类型，决定消费方的处理分支。
	Type enums.EventType `json:"type"`
	// SourceAgent 发起事件的 Agent ID。
	SourceAgent string `json:"source_agent"`
	// TargetAgent 目标 Agent ID；空字符串表示广播，由所有相关 Agent 消费。
	TargetAgent string `json:"target_agent"`
	// Payload 事件负载，结构由 Type 决定，通常按约定反序列化。
	Payload map[string]any `json:"payload"`
	// Priority 优先级（数值越大越优先），影响 Mailbox 投递顺序。
	Priority int `json:"priority"`
	// CreatedAt 创建时间，用于排序与超时判断。
	CreatedAt time.Time `json:"created_at"`
	// Status 当前处理状态，记录事件在生命周期中的阶段。
	Status enums.EventStatus `json:"status"`
}

// ChatMessage 对话消息：与用户/系统对话的逐条记录，用于上下文构造。
// 作为 LLM Chat Completion 接口的消息单元，Role 与 OpenAI 协议对齐。
type ChatMessage struct {
	// Role 角色：ChatRoleSystem / ChatRoleUser / ChatRoleAssistant。
	Role enums.ChatRole `json:"role"`
	// Content 消息正文，直接作为 LLM 输入。
	Content string `json:"content"`
	// Timestamp 消息时间，用于展示与时间衰减。
	Timestamp time.Time `json:"timestamp"`
}

// ClarifyOtherOptionID 是「其他」选项的固定 ID（TODO #53 补）：非 yes/no 确认的
// choice 类提问由会话层自动追加该选项；用户点选后前端引导自由文本输入答案
// （选项不精确或方向错误时的逃生通道），不产生提交。
const ClarifyOtherOptionID = "other"

// ClarifyOption 是澄清请求的一个结构化选项（TODO #53）。
// 用户可点选选项答复（单选/多选），也可忽略选项自由文本答复。
type ClarifyOption struct {
	ID          string `json:"id"`                    // 选项唯一标识（确认场景固定 confirm/reject）
	Label       string `json:"label"`                 // 选项展示文本
	Description string `json:"description,omitempty"` // 选项补充说明（可选）
}

// ClarifyRequest 人机对话请求：Agent 在执行中遇到需要用户确认的问题时挂起，
// 由 server 层通过 HTTP 暴露给前端，用户答复后恢复会话。
type ClarifyRequest struct {
	ID         string     `json:"id"`                    // 请求唯一 ID（用于答复对齐）
	Question   string     `json:"question"`              // Agent 提给用户的问题
	Context    string     `json:"context"`               // 触发澄清的上下文摘要（便于用户理解）
	AgentID    string     `json:"agent_id"`              // 发起澄清的 Agent 实例 ID
	CreatedAt  time.Time  `json:"created_at"`            // 创建时间
	Answer     string     `json:"answer,omitempty"`      // 用户答复（回填）
	AnsweredAt *time.Time `json:"answered_at,omitempty"` // 答复时间
	// Kind 澄清类型（TODO #53）：confirm=破坏性操作确认 / choice=选项选择 /
	// text=纯自由文本。缺省空串视为 text（向后兼容）。
	Kind string `json:"kind,omitempty"`
	// MultiSelect 是否允许多选（仅 choice 有意义；confirm 恒单选）。
	MultiSelect bool `json:"multi_select,omitempty"`
	// Options 结构化选项列表；空表示无选项（自由文本答复）。
	Options []ClarifyOption `json:"options,omitempty"`
	// AnswerOptionIDs 答复时命中的选项 ID（多选按序）；自由文本答复为空。
	AnswerOptionIDs []string `json:"answer_option_ids,omitempty"`
}

// UIEvent TUI 推送事件：向 bubbletea TUI / Web SSE 订阅者广播的事件信封。
// 发送方与消费方通过 Type 约定 Payload 结构。
type UIEvent struct {
	// Type 事件类型字符串，由发送方约定。
	Type string `json:"type"`
	// Timestamp 事件时间，用于排序与展示。
	Timestamp time.Time `json:"timestamp"`
	// Payload 事件负载，结构由 Type 决定。
	Payload any `json:"payload"`
}

// GraphStepPayload Graph 步骤事件负载：描述一次 Node 执行的统计信息。
// 主要用于 UI 展示单次节点执行的耗时与 Token 消耗。
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
// 前端据此更新 Agent 列表或详情面板。
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
// 让前端无需读取完整记忆即可展示最新执行摘要。
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
// 用于事件列表或通知面板展示。
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
// 由后台定时聚合后推送到前端。
type StatsView struct {
	// TopicID 所属话题。
	TopicID string `json:"topic_id"`
	// PrivateEpisodes 私有 Episode 总数。
	PrivateEpisodes int `json:"private_episodes"`
	// CompressedStandard 压缩到 LevelStandard 的 Episode 数。
	CompressedStandard int `json:"compressed_standard"`
	// CompressedRaw 保持 LevelRaw 的 Episode 数。
	CompressedRaw int `json:"compressed_raw"`
	// GlobalKBHits 全局知识库命中次数。
	GlobalKBHits int `json:"global_kb_hits"`
	// TokenBudgetUsed 已用 Token 预算。
	TokenBudgetUsed int `json:"token_budget_used"`
	// ActiveAgents 当前活跃 Agent 数。
	ActiveAgents int `json:"active_agents"`
	// TotalAgents Agent 总数。
	TotalAgents int `json:"total_agents"`
}
