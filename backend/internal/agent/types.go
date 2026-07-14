package agent

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// CreateRequest 表示创建新会话的请求。
// Goal 是会话的目标或任务描述，Meta 用于携带额外的元数据。
type CreateRequest struct {
	Goal string         // Goal 本次会话的目标/任务描述
	Meta map[string]any // Meta 附加的键值对元数据，供业务扩展使用
}

// ResumeRequest 表示恢复一个此前暂停或已结束的会话。
// CarryOver 用于携带历史上下文，UserInput 是用户本次的新输入。
type ResumeRequest struct {
	CarryOver string // CarryOver 需要继承到本次会话的历史上下文摘要
	UserInput string // UserInput 用户当前输入的新消息/指令
}

// Message 表示会话中一次单聊消息。
// Role 标识发送者角色，Content 为消息正文，Timestamp 为消息产生时间。
type Message struct {
	Role      string    // Role 消息发送者角色，例如 user / assistant / system
	Content   string    // Content 消息正文内容
	Timestamp time.Time // Timestamp 消息产生的时间戳
}

// Query 表示对会话发起的只读查询。
// Kind 指定查询类别，Args 承载查询所需的参数。
type Query struct {
	Kind string         // Kind 查询类型，通常取 QueryKind 系列常量
	Args map[string]any // Args 查询参数，按具体 Kind 解析
}

// Result 表示 Query 查询的返回结果。
// Data 承载具体的查询结果数据，类型为 any 以适应不同查询类别。
type Result struct {
	Data any // Data 查询返回的具体数据
}

// QueryKind 系列常量定义了只读查询的类别标识。
// 这些常量统一用于 Query.Kind，便于 agent 内部路由到对应的查询处理器。
const (
	QueryKindBoard        = "board"         // QueryKindBoard 看板数据查询
	QueryKindMailbox      = "mailbox"       // QueryKindMailbox 邮箱/消息队列查询
	QueryKindMetrics      = "metrics"       // QueryKindMetrics 通用指标查询
	QueryKindWatchdog     = "watchdog"      // QueryKindWatchdog 看门狗状态查询
	QueryKindTokenMetrics = "token-metrics" // QueryKindTokenMetrics Token 消耗指标查询
	QueryKindLogs         = "logs"          // QueryKindLogs 日志查询
	QueryKindSessionCount = "session-count" // QueryKindSessionCount 会话数量查询
	QueryKindLLMStats     = "llm-stats"     // QueryKindLLMStats LLM 调用统计查询
)

// ControlCommand 表示向会话发送的操作型控制命令。
// Op 指定操作类型，Args 承载该操作所需的参数。
type ControlCommand struct {
	Op   string         // Op 控制操作类型，通常取 ControlOp 系列常量
	Args map[string]any // Args 控制命令的参数，按具体 Op 解析
}

// ControlOp 系列常量定义了会话支持的控制操作。
// 这些常量统一用于 ControlCommand.Op，用于向运行中的会话下发指令。
const (
	ControlOpMessage   = "message"   // ControlOpMessage 向会话发送普通消息
	ControlOpClarify   = "clarify"   // ControlOpClarify 要求会话进行澄清确认
	ControlOpInterrupt = "interrupt" // ControlOpInterrupt 中断会话当前动作
	ControlOpEnqueue   = "enqueue"   // ControlOpEnqueue 将任务/消息加入队列
	ControlOpCancel    = "cancel"    // ControlOpCancel 取消会话或当前任务
	ControlOpTopic     = "topic"     // ControlOpTopic 切换/指定会话主题
)

// Filter 表示列出会话时使用的筛选条件。
// Status 按状态过滤，Limit 限制返回数量。
type Filter struct {
	Status string // Status 期望筛选的会话状态
	Limit  int    // Limit 最大返回条数，0 通常表示使用默认值
}

// ActiveBlock 表示运行中会话块的轻量视图。
// ID 为块标识，Domain 为所属领域，Goal 为块目标。
type ActiveBlock struct {
	ID     string // ID 会话块唯一标识
	Domain string // Domain 会话块所属领域/模块
	Goal   string // Goal 会话块当前目标
}

// ClarifyRequest 是澄清请求的 DTO，字段与 types.ClarifyRequest 对应。
// 使用普通 DTO 字段，避免 agent 包依赖内部 types 包。
type ClarifyRequest struct {
	ID         string     // ID 澄清请求唯一标识
	Question   string     // Question 需要向用户澄清的问题文本
	Context    string     // Context 触发澄清的上下文信息
	AgentID    string     // AgentID 发起澄清的 Agent 标识
	CreatedAt  time.Time  // CreatedAt 澄清请求创建时间
	Answer     string     // Answer 用户给出的答案内容
	AnsweredAt *time.Time // AnsweredAt 用户回答时间，未回答时为 nil
}

// Session 是会话对象的 DTO，按字段逐一对齐 server.Session。
// 使用纯 DTO 类型，使 agent 包与 internal/server 和 backend/pkg/types 解耦。
type Session struct {
	ID             string          // ID 会话唯一标识
	Goal           string          // Goal 会话目标/任务描述
	Status         string          // Status 会话当前状态，例如 running / paused / finished
	Result         string          // Result 会话最终结果或输出摘要
	State          string          // State 会话内部状态机状态
	StartedAt      time.Time       // StartedAt 会话开始时间
	EndedAt        time.Time       // EndedAt 会话结束时间，未结束为零值
	Events         []Event         // Events 会话生命周期中产生的事件列表
	Messages       []Message       // Messages 会话中的聊天消息列表
	TempDir        string          // TempDir 会话使用的临时目录路径
	ActiveBlocks   []ActiveBlock   // ActiveBlocks 当前正在运行的会话块视图
	PendingClarify *ClarifyRequest // PendingClarify 待处理的澄清请求，无则为 nil
}

// Event 是会话事件的 DTO，按字段逐一对齐 server.SessionEvent。
// 记录一次发生在会话中的事件，包括 Agent 动作、工具调用、LLM 交互等信息。
type Event struct {
	Type         string    // Type 事件类型，例如 tool / llm / agent 等
	Agent        string    // Agent 产生该事件的 Agent 名称/标识
	Message      string    // Message 事件描述或输出消息
	Kind         string    // Kind 事件子类型/分类
	Tool         string    // Tool 调用的工具名称
	ToolPath     string    // ToolPath 工具调用路径或标识
	ToolOutput   string    // ToolOutput 工具返回的输出内容
	ToolError    string    // ToolError 工具调用产生的错误信息
	Success      bool      // Success 该事件代表的操作是否成功
	Timestamp    time.Time // Timestamp 事件发生时间
	Prompt       string    // Prompt 发送给 LLM 的提示词内容
	InputTokens  int       // InputTokens LLM 输入 Token 数
	OutputTokens int       // OutputTokens LLM 输出 Token 数
	DetailJSON   string    // DetailJSON 事件的原始 JSON 详情，便于审计与调试
}

// AgentInstance 表示一个已实例化 Agent 的运行时轻量视图。
// 汇总了 Agent 的名称、角色、状态、所属领域及层级关系等关键信息。
type AgentInstance struct {
	Name      string         // Name Agent 实例名称
	Role      string         // Role Agent 担任的角色名称
	ModuleID  string         // ModuleID Agent 所属模块标识
	Status    string         // Status Agent 当前运行状态
	Domain    string         // Domain Agent 所属领域
	RoleType  enums.RoleType // RoleType 角色类型，使用 pkg/enums 中的枚举
	Children  []string       // Children 子 Agent 实例标识列表
	CreatedAt time.Time      // CreatedAt Agent 实例创建时间
	RoleDefID string         // RoleDefID 角色定义唯一标识
}
