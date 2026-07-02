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

// MemoryWriteFailure 记忆写入死信记录：写入失败经重试耗尽后归档，供启动期回放补写。
type MemoryWriteFailure struct {
	// ID 死信记录主键。
	ID int `json:"id"`
	// AgentID 失败记录所属 Agent 实例 ID。
	AgentID string `json:"agent_id"`
	// TopicID 所属话题/会话 ID。
	TopicID string `json:"topic_id"`
	// StepCount 步骤序号，作为幂等键的一部分。
	StepCount int `json:"step_count"`
	// Action 失败的写入动作类型（如 episode_write / snapshot_save）。
	Action string `json:"action"`
	// RawContent 待写入的原始内容摘要。
	RawContent string `json:"raw_content"`
	// Error 失败错误信息。
	Error string `json:"error"`
	// RetryCount 已重试次数。
	RetryCount int `json:"retry_count"`
	// CreatedAt 死信记录创建时间。
	CreatedAt time.Time `json:"created_at"`
	// ResolvedAt 回放成功后的解析时间；nil 表示未解析。
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}
