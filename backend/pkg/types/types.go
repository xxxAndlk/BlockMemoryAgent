package types

import (
	"time"
)

// Episode 结构化情节记录
type Episode struct {
	StepID             string     `json:"step_id"`
	Timestamp          time.Time  `json:"timestamp"`
	Action             string     `json:"action"`
	ObservationSummary string     `json:"observation_summary"`
	FullObservation    string     `json:"full_observation,omitempty"`
	Facts              []string   `json:"facts"`
	Reflection         string     `json:"reflection,omitempty"`
	Importance         float64    `json:"importance"`
	TopicBound         bool       `json:"topic_bound"`
	ToolCalls          []ToolCall `json:"tool_calls,omitempty"`
}

// ToolCall 工具调用记录
type ToolCall struct {
	Name     string `json:"name"`
	Input    string `json:"input"`
	Output   string `json:"output"`
	Duration int64  `json:"duration_ms"`
}

// AgentOutput Agent 公开输出
type AgentOutput struct {
	AgentID     string    `json:"agent_id"`
	Version     int       `json:"version"`
	Summary     string    `json:"summary"`
	DataRefs    []string  `json:"data_refs"`
	KnownIssues []string  `json:"known_issues"`
	Timestamp   time.Time `json:"timestamp"`
	Validated   bool      `json:"validated"`
}

// EventType 事件类型
type EventType string

const (
	EventCrossModify    EventType = "CrossModify"
	EventDependencyMet  EventType = "DependencyMet"
	EventEscalation     EventType = "Escalation"
)

// EventStatus 事件状态
type EventStatus string

const (
	EventPending     EventStatus = "Pending"
	EventProcessing  EventStatus = "Processing"
	EventDone        EventStatus = "Done"
)

// Event 工作区事件
type Event struct {
	ID          string         `json:"id"`
	Type        EventType      `json:"type"`
	SourceAgent string         `json:"source_agent"`
	TargetAgent string         `json:"target_agent"`
	Payload     map[string]any `json:"payload"`
	Priority    int            `json:"priority"`
	CreatedAt   time.Time      `json:"created_at"`
	Status      EventStatus    `json:"status"`
}

// AgentSnapshot Agent 私有快照
type AgentSnapshot struct {
	AgentID      string         `json:"agent_id"`
	TopicID      string         `json:"topic_id"`
	LastStepID   string         `json:"last_step_id"`
	KeySummaries []SummaryBlock `json:"key_summaries"`
	OpenIssues   []Issue        `json:"open_issues"`
	LocalVars    map[string]any `json:"local_vars"`
	PublishedVer int            `json:"published_ver"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// SummaryBlock 摘要块
type SummaryBlock struct {
	StepID    string    `json:"step_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Issue 未解决问题
type Issue struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// ChatMessage 对话消息
type ChatMessage struct {
	Role      string    `json:"role"` // "user" | "assistant" | "system"
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// ActionType Graph 控制信号
type ActionType string

const (
	ActionContinue ActionType = "Continue"
	ActionSwitch   ActionType = "Switch"
	ActionEscalate ActionType = "Escalate"
	ActionFinish   ActionType = "Finish"
)

// GraphState 三层图共享状态
type GraphState struct {
	TopicID      string                 `json:"topic_id"`
	TopicGoal    string                 `json:"topic_goal"`
	Constraints  map[string]string      `json:"constraints"`
	EventQueue   []*Event               `json:"event_queue"`
	CurrentAgent string                 `json:"current_agent"`
	AgentOutputs map[string]*AgentOutput `json:"agent_outputs"`
	SnapshotRefs map[string]string      `json:"snapshot_refs"`
	NextAction   ActionType             `json:"next_action"`
	TargetAgent  string                 `json:"target_agent"`
	Reason       string                 `json:"reason"`
}

// CompressionLevel 记忆压缩层级
type CompressionLevel int

const (
	LevelRaw       CompressionLevel = iota // 完整原始记录
	LevelStandard                           // 摘要 + Facts，去除 FullObservation
	LevelCompact                            // 仅保留 Summary 一句话
	LevelMarker                             // 仅存在性标记
)

// TokenBudget Token 预算分配
type TokenBudget struct {
	SystemRole    int `json:"system_role"`
	TopicGlobal   int `json:"topic_global"`
	SharedState   int `json:"shared_state"`
	GlobalKB      int `json:"global_kb"`
	PrivateMemory int `json:"private_memory"`
	TaskQuery     int `json:"task_query"`
	Reserve       int `json:"reserve"`
}

// Total 总 Token
func (b *TokenBudget) Total() int {
	return b.SystemRole + b.TopicGlobal + b.SharedState + b.GlobalKB +
		b.PrivateMemory + b.TaskQuery + b.Reserve
}

// RelevanceScore 多信号相关性评分
type RelevanceScore struct {
	SemanticSim   float64 `json:"semantic_sim"`
	EntityOverlap float64 `json:"entity_overlap"`
	TemporalDecay float64 `json:"temporal_decay"`
	CausalChain   float64 `json:"causal_chain"`
	FinalScore    float64 `json:"final_score"`
}

// KnowledgeRecord 全局知识库记录
type KnowledgeRecord struct {
	ID           int64          `json:"id"`
	KnowledgeType string        `json:"knowledge_type"`
	TopicID      string         `json:"topic_id,omitempty"`
	Content      string         `json:"content"`
	Embedding    []float32      `json:"embedding,omitempty"`
	Meta         map[string]any `json:"meta"`
	AccessCount  int            `json:"access_count"`
	LastAccessed *time.Time     `json:"last_accessed,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	Archived     bool           `json:"archived"`
}

// TopicMeta 话题元数据
type TopicMeta struct {
	ID          string    `json:"id"`
	Goal        string    `json:"goal"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

// UIEvent TUI 推送事件
type UIEvent struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload"`
}

// GraphStepPayload Graph 步骤事件负载
type GraphStepPayload struct {
	NodeName   string        `json:"node_name"`
	AgentID    string        `json:"agent_id,omitempty"`
	Duration   time.Duration `json:"duration"`
	InputSize  int           `json:"input_tokens"`
	OutputSize int           `json:"output_tokens"`
	Action     string        `json:"action,omitempty"`
}

// AgentStatusPayload Agent 状态事件负载
type AgentStatusPayload struct {
	AgentID     string `json:"agent_id"`
	State       string `json:"state"`
	CurrentStep int    `json:"current_step,omitempty"`
	LastOutput  string `json:"last_output,omitempty"`
}

// EpisodePayload Episode 事件负载
type EpisodePayload struct {
	AgentID    string    `json:"agent_id"`
	StepID     string    `json:"step_id"`
	Importance float64   `json:"importance"`
	Summary    string    `json:"summary"`
	Facts      []string  `json:"facts"`
	Timestamp  time.Time `json:"timestamp"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// EventPayload Event 事件负载
type EventPayload struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Summary  string `json:"summary"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}

// StatsView 统计面板数据
type StatsView struct {
	TopicID          string `json:"topic_id"`
	PrivateEpisodes  int    `json:"private_episodes"`
	CompressedL1     int    `json:"compressed_l1"`
	CompressedL2     int    `json:"compressed_l2"`
	GlobalKBHits     int    `json:"global_kb_hits"`
	TokenBudgetUsed  int    `json:"token_budget_used"`
	ActiveAgents     int    `json:"active_agents"`
	TotalAgents      int    `json:"total_agents"`
}
