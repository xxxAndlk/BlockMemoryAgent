package types

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// Episode 结构化情节记录：一次 Agent 执行步骤的完整留痕。
// 作为记忆系统的最小单元，进入压缩/检索流水线（见 internal/memory/）。
type Episode struct {
	// StepID 全局唯一的步骤标识，用于跨 Agent 引用与去重。
	StepID string `json:"step_id"`
	// Timestamp 步骤发生时间，用于时间衰减与排序。
	Timestamp time.Time `json:"timestamp"`
	// Action Agent 本步采取的动作摘要（如 "调用工具 X" / "回答用户"）。
	Action string `json:"action"`
	// ObservationSummary 观察结果的一句话摘要，用于压缩层级展示。
	ObservationSummary string `json:"observation_summary"`
	// FullObservation 原始完整观察文本；压缩到 LevelStandard 后会被丢弃。
	FullObservation string `json:"full_observation,omitempty"`
	// Facts 从本步提炼出的事实三元组/短句，参与实体重叠评分。
	Facts []string `json:"facts"`
	// Reflection Agent 自反思文本，可选；用于 episodic 记忆回顾。
	Reflection string `json:"reflection,omitempty"`
	// Importance 重要性评分 [0,1]，决定是否进入长期记忆与压缩层级。
	Importance float64 `json:"importance"`
	// TopicBound 是否已绑定到某个 Topic；未绑定的 Episode 走通用流水线。
	TopicBound bool `json:"topic_bound"`
	// ToolCalls 本步触发的工具调用明细，便于审计与回放。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall 工具调用记录：单次工具执行的结构化留痕。
type ToolCall struct {
	// Name 工具名（如 ReadFile / RunCommand），与 ToolExecutor case 对齐。
	Name string `json:"name"`
	// Input 工具入参 JSON 字符串。
	Input string `json:"input"`
	// Output 工具返回结果文本。
	Output string `json:"output"`
	// Duration 执行耗时（毫秒），用于性能监控与超时分析。
	Duration int64 `json:"duration_ms"`
}

// AgentOutput Agent 公开输出：Agent 向外发布的结构化产物。
// 与私有快照（AgentSnapshot）相对，供其他 Agent 与全局状态消费。
type AgentOutput struct {
	// AgentID 产出该结果的 Agent 实例 ID。
	AgentID string `json:"agent_id"`
	// Version 输出版本号，单调递增，用于快照与依赖追踪。
	Version int `json:"version"`
	// Summary 面向其他 Agent 的一句话摘要。
	Summary string `json:"summary"`
	// DataRefs 关联的数据引用（如文件路径、知识库 ID 列表）。
	DataRefs []string `json:"data_refs"`
	// KnownIssues 该输出已知的遗留问题/风险提示。
	KnownIssues []string `json:"known_issues"`
	// Timestamp 发布时间。
	Timestamp time.Time `json:"timestamp"`
	// Validated 是否经过校验（自检/他检），未校验输出不可被下游依赖。
	Validated bool `json:"validated"`
}



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

// AgentSnapshot Agent 私有快照：Agent 在某 Topic 下的本地状态持久化。
// 进入 Redis 热存与 Postgres 冷存（见 internal/memory/snapshot.go）。
type AgentSnapshot struct {
	// AgentID 快照所属 Agent。
	AgentID string `json:"agent_id"`
	// TopicID 所属话题，用于按话题隔离私有记忆。
	TopicID string `json:"topic_id"`
	// LastStepID 最近一次纳入快照的 StepID，用于增量更新。
	LastStepID string `json:"last_step_id"`
	// KeySummaries 关键步骤摘要块列表（压缩后的 Episode 摘要）。
	KeySummaries []SummaryBlock `json:"key_summaries"`
	// OpenIssues 尚未解决的问题列表，跨步骤延续。
	OpenIssues []Issue `json:"open_issues"`
	// LocalVars Agent 私有变量（如中间计算结果），不对外发布。
	LocalVars map[string]any `json:"local_vars"`
	// PublishedVer 已对外发布的最新输出版本号。
	PublishedVer int `json:"published_ver"`
	// UpdatedAt 快照最后更新时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// SummaryBlock 摘要块：单步 Episode 压缩后的可引用摘要单元。
type SummaryBlock struct {
	// StepID 对应原始 Episode 的 StepID。
	StepID string `json:"step_id"`
	// Content 摘要正文。
	Content string `json:"content"`
	// Timestamp 摘要生成时间。
	Timestamp time.Time `json:"timestamp"`
}

// Issue 未解决问题：在执行过程中识别并跟踪的待办风险项。
type Issue struct {
	// ID 问题唯一标识。
	ID string `json:"id"`
	// Description 问题描述。
	Description string `json:"description"`
	// CreatedAt 发现时间。
	CreatedAt time.Time `json:"created_at"`
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
	ID        string    `json:"id"`         // 请求唯一 ID（用于答复对齐）
	Question  string    `json:"question"`   // Agent 提给用户的问题
	Context   string    `json:"context"`    // 触发澄清的上下文摘要（便于用户理解）
	AgentID   string    `json:"agent_id"`   // 发起澄清的 Agent 实例 ID
	CreatedAt time.Time `json:"created_at"` // 创建时间
	Answer    string    `json:"answer,omitempty"` // 用户答复（回填）
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


// TokenBudget Token 预算分配：上下文 4 段式拼接的 Token 上限配置。
// 见 memory/assembler.go 的 4 段装配（System/TopicGlobal/SharedState/PrivateMemory）。
type TokenBudget struct {
	// SystemRole 系统角色段（soul.md + 角色定义）预算。
	SystemRole int `json:"system_role"`
	// TopicGlobal 话题全局段（同 Topic 跨 Agent 共享摘要）预算。
	TopicGlobal int `json:"topic_global"`
	// SharedState 共享状态段（其他 Agent 的公开输出）预算。
	SharedState int `json:"shared_state"`
	// GlobalKB 全局知识库段（KnowledgeRecord 检索结果）预算。
	GlobalKB int `json:"global_kb"`
	// PrivateMemory 私有记忆段（本 Agent 的 Episode/快照）预算。
	PrivateMemory int `json:"private_memory"`
	// TaskQuery 当前任务/用户查询段预算。
	TaskQuery int `json:"task_query"`
	// Reserve 预留缓冲，防止超出模型上下文窗口。
	Reserve int `json:"reserve"`
}

// Total 返回所有段预算之和，即上下文总 Token 上限。
// 副作用：无。参数：无。返回：各段预算累加值。
func (b *TokenBudget) Total() int {
	return b.SystemRole + b.TopicGlobal + b.SharedState + b.GlobalKB +
		b.PrivateMemory + b.TaskQuery + b.Reserve
}

// RelevanceScore 多信号相关性评分：综合多源信号衡量 Episode/记录与查询的匹配度。
// 见 internal/memory/search.go 的多信号融合逻辑。
type RelevanceScore struct {
	// SemanticSim 语义相似度（pgvector 向量余弦）。
	SemanticSim float64 `json:"semantic_sim"`
	// EntityOverlap 实体重叠度（Facts 命中率）。
	EntityOverlap float64 `json:"entity_overlap"`
	// TemporalDecay 时间衰减因子（越新越接近 1）。
	TemporalDecay float64 `json:"temporal_decay"`
	// CausalChain 因果链匹配分（与上下文因果连续性）。
	CausalChain float64 `json:"causal_chain"`
	// FinalScore 加权融合后的最终评分，用于排序。
	FinalScore float64 `json:"final_score"`
}

// KnowledgeRecord 全局知识库记录：跨 Topic 共享的长期知识条目。
// 存储于 Postgres + pgvector，见 internal/store/postgres.go。
type KnowledgeRecord struct {
	// ID 自增主键。
	ID int64 `json:"id"`
	// KnowledgeType 知识类型（playbook / postmortem / rule / block_memory / domain_archive）。
	KnowledgeType enums.KnowledgeType `json:"knowledge_type"`
	// TopicID 关联话题，可空表示全局知识。
	TopicID string `json:"topic_id,omitempty"`
	// Content 知识正文。
	Content string `json:"content"`
	// Embedding 向量嵌入，用于语义检索；序列化时可省略。
	Embedding []float32 `json:"embedding,omitempty"`
	// Meta 扩展元数据。
	Meta map[string]any `json:"meta"`
	// AccessCount 被检索命中次数，反映热度。
	AccessCount int `json:"access_count"`
	// LastAccessed 最近访问时间，可空。
	LastAccessed *time.Time `json:"last_accessed,omitempty"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// Archived 是否归档；归档后不再参与默认检索。
	Archived bool `json:"archived"`
}

// TopicMeta 话题元数据：描述一个话题的生命周期与目标。
type TopicMeta struct {
	// ID 话题唯一标识。
	ID string `json:"id"`
	// Goal 话题目标。
	Goal string `json:"goal"`
	// Status 话题状态（active / done / archived）。
	Status enums.TopicStatus `json:"status"`
	// CreatedAt 创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ExpiresAt 过期时间，可空表示不过期。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
