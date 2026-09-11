// Package agent 定义了会话、消息、查询等核心 DTO 与常量，
// 作为 Agent 编排层与上层（HTTP/TUI）之间的数据契约。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// 用户消息图片限流（与 MCP 图片透传 mcpbridge 上限对齐：4 张 / 单张 4MiB）。
// 双层执行：TUI 粘贴时拒绝 + HandleSessionMessage 400。
const (
	// MaxMessageImages 单条用户消息最多携带图片张数。
	MaxMessageImages = 4
	// MaxMessageImageBytes 单张图片 base64 解码后的最大字节数。
	MaxMessageImageBytes = 4 << 20
)

// WireImage 是用户消息图片的 HTTP 线型 DTO（TUI/Web -> server）。
// server 不直接依赖 tool 包，经 ToResultImage 转入内存类型。
type WireImage struct {
	// MIMEType 图片类型（image/png、image/jpeg/gif/webp）。
	MIMEType string `json:"mime_type"`
	// Data 为 base64 编码内容（encoding/json 对 []byte 原生 base64 编解码）。
	Data []byte `json:"data"`
}

// ToResultImage 把线型 DTO 转为链路内统一图片类型。
func (w WireImage) ToResultImage() tool.ResultImage {
	return tool.ResultImage{MIMEType: w.MIMEType, Data: w.Data}
}

// ParseWireImages 校验并转换 HTTP 线型图片列表（超限报错），
// 供 server 层使用（server 不直接依赖 tool 包）。
func ParseWireImages(imgs []WireImage) ([]tool.ResultImage, error) {
	if len(imgs) > MaxMessageImages {
		return nil, fmt.Errorf("单条消息最多携带 %d 张图片", MaxMessageImages)
	}
	out := make([]tool.ResultImage, 0, len(imgs))
	for _, img := range imgs {
		if len(img.Data) > MaxMessageImageBytes {
			return nil, fmt.Errorf("单张图片超过大小上限（4MiB）")
		}
		out = append(out, img.ToResultImage())
	}
	return out, nil
}

// 用户消息视频限流：视频体积远超图片，传宿主路径不传内容
//（TUI 与本地 Gin 服务同机同进程，路径语义成立；未来前后端分离需改 multipart 上传）。
// 双层执行：TUI 粘贴时拒绝 + HandleSessionMessage 400。
const (
	// MaxMessageVideos 单条用户消息最多携带视频个数。
	MaxMessageVideos = 2
	// DefaultMaxVideoBytes 单个视频文件默认大小上限（200MB）。
	DefaultMaxVideoBytes = 200 << 20
)

// videoExtMIME 视频扩展名白名单（key 均为小写，查找时 strings.ToLower）。
var videoExtMIME = map[string]string{
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".avi":  "video/x-msvideo",
}

// VideoMIMEByExt 按文件扩展名（含点、大小写不敏感）返回视频 MIME 类型；
// 非白名单扩展名返回 false。TUI 侧判定剪贴板文件是否视频复用此函数。
func VideoMIMEByExt(ext string) (string, bool) {
	mime, ok := videoExtMIME[strings.ToLower(ext)]
	return mime, ok
}

// WireVideo 是用户消息视频附件的 HTTP 线型 DTO：只传宿主机文件路径，
// 服务端（与 TUI 同机）读文件抽帧，避免 base64 大体积过 JSON。
type WireVideo struct {
	// Path 视频文件的宿主机绝对路径。
	Path string `json:"path"`
	// MIMEType 视频类型，可省略（服务端按扩展名推断回填）。
	MIMEType string `json:"mime_type,omitempty"`
}

// ParseWireVideos 校验并规范化 HTTP 线型视频列表：数量上限、扩展名白名单、
// 文件存在、大小 ≤ maxBytes（maxBytes<=0 取 DefaultMaxVideoBytes），
// mime 缺省按扩展名回填。供 server 层使用（超限返回 400 语义的错误）。
func ParseWireVideos(vs []WireVideo, maxBytes int64) ([]WireVideo, error) {
	if len(vs) > MaxMessageVideos {
		return nil, fmt.Errorf("单条消息最多携带 %d 个视频", MaxMessageVideos)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxVideoBytes
	}
	out := make([]WireVideo, 0, len(vs))
	for _, v := range vs {
		if v.Path == "" {
			return nil, fmt.Errorf("视频路径为空")
		}
		mime, ok := VideoMIMEByExt(filepath.Ext(v.Path))
		if !ok {
			return nil, fmt.Errorf("不支持的视频格式 %q（支持 mp4/webm/mov/mkv/avi）", filepath.Ext(v.Path))
		}
		info, err := os.Stat(v.Path)
		if err != nil {
			return nil, fmt.Errorf("视频文件不可读 %s: %w", v.Path, err)
		}
		if info.Size() > maxBytes {
			return nil, fmt.Errorf("视频超过大小上限（%dMB）: %s", maxBytes>>20, v.Path)
		}
		if v.MIMEType == "" {
			v.MIMEType = mime
		}
		out = append(out, v)
	}
	return out, nil
}

// CreateRequest 表示创建新会话的请求。
// Goal 是会话的目标或任务描述，Meta 用于携带额外的元数据。
type CreateRequest struct {
	Goal string // Goal 本次会话的目标/任务描述
	// WorkDir 每会话工作目录(绝对路径),空=进程默认
	WorkDir string
	Meta    map[string]any
	// Images 首条消息携带的图片（Alt+V 粘贴）：内存透传不持久化，仅首轮注入。
	Images []tool.ResultImage
	// Videos 首条消息携带的视频（Alt+V 粘贴视频文件）：CreateSession 内抽帧
	// 并入 Images、元数据文本并入 Goal，Videos 本身不持久化。
	Videos []WireVideo
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
	// Images 用户随消息粘贴的图片（Alt+V）：内存透传不持久化，ToServerSession
	// 显式映射不透出；重启后历史仅保留 Content 里的 [image:N] 占位文本。
	Images []tool.ResultImage
	// Videos 用户随消息粘贴的视频文件（Alt+V 粘贴视频）：sendMessage 内抽帧
	// 并入 Images、元数据文本并入 Content 后即弃（不入 session.Messages），
	// 与 Images 同为内存透传不持久化。
	Videos []WireVideo
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
	// QueryKindEfficiency 会话效率一等指标（TODO 第9项⑥/第10项③）：五项指标 + 支路成本表。
	QueryKindEfficiency = "efficiency"
	// QueryKindAgentEvents 子 Agent 审计下钻：Args{"agent": 实例ID, "limit", "offset"}，
	// 返回该实例 agent_events 逐轮事件（回放数据源）。
	QueryKindAgentEvents = "agent-events"
	// QueryKindWorktrees 会话 worktree 副本清单（TODO 第9⑤/#10⑤）：
	// 返回 ListWorktrees 快照（路径/分支/base/patch 路径与 stat/合并状态）。
	QueryKindWorktrees = "worktrees"
	// QueryKindWorktreeDiff 指定 worktree 的全量 diff（review 数据源）：
	// Args{"agent": 实例ID}，返回 patch 全文（未收尾时为副本实时 diff）。
	QueryKindWorktreeDiff = "worktree-diff"
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
	ControlOpStop      = "stop"      // ControlOpStop 软停止（TODO #37）：停止当前会话全部子任务，可续跑
	ControlOpTopic     = "topic"     // ControlOpTopic 切换/指定会话主题
	// ControlOpTrustMode 切换会话信任模式（TODO 第10⑥）：Args{"mode": "suggest|auto-edit|full-auto"}，
	// atomic 即时生效（正在阻塞的 ReAct 循环下一次工具派发按新模式裁决）。
	ControlOpTrustMode = "trust-mode"
	// ControlOpWorktree worktree 合并门操作（TODO 第9⑤/#10⑤）：
	// Args{"action": "merge|reject", "agent": 实例ID, "comments": 驳回意见（reject 可选）}。
	ControlOpWorktree = "worktree"
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

// ClarifyOption 是澄清请求的一个结构化选项（TODO #53）。
// 用户可点选选项答复（单选/多选），也可忽略选项自由文本答复。
// ClarifyOtherOptionID 是「其他」选项的固定 ID（TODO #53 补）：非 yes/no 确认的
// choice 类提问由会话层自动追加该选项；用户点选后前端引导自由文本输入答案
// （选项不精确或方向错误时的逃生通道），点选本身不产生提交。
// 取值与 pkg/types.ClarifyOtherOptionID 保持一致（agent 包不依赖 types 包，故重复定义）。
const ClarifyOtherOptionID = "other"

type ClarifyOption struct {
	ID          string // ID 选项唯一标识（确认场景固定 confirm/reject）
	Label       string // Label 选项展示文本
	Description string // Description 选项补充说明（可选）
}

// ClarifyQuestionItem 是批量澄清模式（任务 140）中的单个问题及其答复回填。
// 批量提问一次挂出全部题目，用户前后翻页改选后统一提交；每题独立记录
// 命中选项与答复时间，顶层 ClarifyRequest 的 Answer/AnswerOptionIDs 镜像第一题
// （旧客户端只读顶层字段仍能拿到 Q1 的答复）。
type ClarifyQuestionItem struct {
	Question        string          // Question 本题问题文本（短句，长上下文放 ClarifyRequest.Detail）
	Kind            string          // Kind choice=选项选择 / text=纯自由文本
	MultiSelect     bool            // MultiSelect 是否允许多选（仅 choice 有意义）
	Options         []ClarifyOption // Options 本题结构化选项；空表示自由文本答复
	Answer          string          // Answer 用户对本题的答复（回填，解析后文本）
	AnswerOptionIDs []string        // AnswerOptionIDs 答复命中的选项 ID（多选按序）
	AnsweredAt      *time.Time      // AnsweredAt 本题答复时间
}

// ClarifyRequest 是澄清请求的 DTO，字段与 types.ClarifyRequest 对应。
// 使用普通 DTO 字段，避免 agent 包依赖内部 types 包。
type ClarifyRequest struct {
	ID         string     // ID 澄清请求唯一标识
	Question   string     // Question 需要向用户澄清的问题文本（批量模式镜像第一题）
	Context    string     // Context 触发澄清的上下文信息
	AgentID    string     // AgentID 发起澄清的 Agent 标识
	CreatedAt  time.Time  // CreatedAt 澄清请求创建时间
	Answer     string     // Answer 用户给出的答案内容（批量模式镜像第一题）
	AnsweredAt *time.Time // AnsweredAt 用户回答时间，未回答时为 nil
	// Kind 澄清类型（TODO #53）：confirm=破坏性操作确认 / choice=选项选择 /
	// text=纯自由文本。缺省空串视为 text（向后兼容）。
	Kind string
	// MultiSelect 是否允许多选（仅 choice 有意义；confirm 恒单选）。
	MultiSelect bool
	// Options 结构化选项列表；空表示无选项（自由文本答复）。
	Options []ClarifyOption
	// AnswerOptionIDs 答复时命中的选项 ID（多选按序）；自由文本答复为空。
	AnswerOptionIDs []string
	// Detail 附加长上下文（如 submit_plan 计划全文、进度盘点，任务 140）：
	// 先于问题完整展示（事件流 clarify_detail 事件 + 面板 detail 块），
	// question 只承载短问题句。
	Detail string
	// Questions 批量模式（任务 140）的全部题目；len>1 表示批量（同屏分页、
	// 统一提交）。单题路径为 nil，Question/Options 等顶层字段即唯一题目。
	Questions []ClarifyQuestionItem
}

// Session 是会话对象的 DTO，按字段逐一对齐 server.Session。
// 使用纯 DTO 类型，使 agent 包与 internal/server 和 backend/pkg/types 解耦。
type Session struct {
	ID        string    // ID 会话唯一标识
	Goal      string    // Goal 会话目标/任务描述
	Status    string    // Status 会话当前状态，例如 running / paused / finished
	Result    string    // Result 会话最终结果或输出摘要
	State     string    // State 会话内部状态机状态
	StartedAt time.Time // StartedAt 会话开始时间
	EndedAt   time.Time // EndedAt 会话结束时间，未结束为零值
	Events    []Event   // Events 会话生命周期中产生的事件列表
	Messages  []Message // Messages 会话中的聊天消息列表
	TempDir   string    // TempDir 会话使用的临时目录路径
	// WorkDir 每会话工作目录（绝对路径，空=进程默认）。json 名 `work_dir` 由
	// Task 10 的 server DTO 接线决定，此处先行声明。
	WorkDir        string          `json:"work_dir,omitempty"`
	StreamingText  string          // StreamingText 当前正在流式生成的助手文本（仅运行中有值）
	ThinkingText   string          // ThinkingText 当前思考阶段的过程文本（瞬时，仅运行中有值）
	ActiveBlocks   []ActiveBlock   // ActiveBlocks 当前正在运行的会话块视图
	PendingClarify *ClarifyRequest // PendingClarify 待处理的澄清请求，无则为 nil
	// DestroyAt 软停止销毁倒计时截止时间（TODO #37）：Stop 后非 nil，续跑/到期后清空；
	// 供 TUI 显示"MM:SS 后销毁"。未软停止的会话为 nil。
	DestroyAt *time.Time
	// ActiveTopicID 当前活跃话题 ID。切换话题时旧 Agent 树终结 + 新树起,
	// 旧话题摘要写入 sharedKV `topic:{id}:summary` 供新话题 MetaAgent 召回。
	// 空表示尚未切换过话题(单话题会话)。
	ActiveTopicID string
	// TrustMode 会话当前信任模式（TODO 第10⑥）：suggest|auto-edit|full-auto；
	// 空串 = 未设置（Registry 回退现网生产边界 + 危险命令语义）。
	TrustMode string
}

// Event 是会话事件的 DTO，按字段逐一对齐 server.SessionEvent。
// 记录一次发生在会话中的事件，包括 Agent 动作、工具调用、LLM 交互等信息。
type Event struct {
	Type            string    // Type 事件类型，例如 tool / llm / agent 等
	Agent           string    // Agent 产生该事件的 Agent 名称/标识
	Message         string    // Message 事件描述或输出消息
	Kind            string    // Kind 事件子类型/分类
	Tool            string    // Tool 调用的工具名称
	ToolPath        string    // ToolPath 工具调用路径或标识
	ToolOutput      string    // ToolOutput 工具返回的输出内容
	ToolError       string    // ToolError 工具调用产生的错误信息
	Success         bool      // Success 该事件代表的操作是否成功
	Timestamp       time.Time // Timestamp 事件发生时间
	Prompt          string    // Prompt 发送给 LLM 的提示词内容
	InputTokens     int       // InputTokens LLM 输入 Token 数
	OutputTokens    int       // OutputTokens LLM 输出 Token 数
	CacheHitTokens  int       // CacheHitTokens 缓存命中 token 数（TODO #40 可观测）
	CacheMissTokens int       // CacheMissTokens 缓存未命中 token 数（TODO #40 可观测）
	DetailJSON      string    // DetailJSON 事件的原始 JSON 详情，便于审计与调试
}

// AgentInstance 表示一个已实例化 Agent 的运行时轻量视图。
// 汇总了 Agent 的名称、角色、状态、所属领域及层级关系等关键信息。
type AgentInstance struct {
	Name      string         // Name Agent 实例名称
	Role      string         // Role Agent 担任的角色名称
	ModuleID  string         // ModuleID Agent 所属模块标识
	ParentID  string         // ParentID 父实例标识（空表示顶层）
	Status    string         // Status Agent 当前运行状态
	Domain    string         // Domain Agent 所属领域
	Goal      string         // Goal Agent 当前任务目标（可选）
	RoleType  enums.RoleType // RoleType 角色类型，使用 pkg/enums 中的枚举
	Children  []string       // Children 子 Agent 实例标识列表
	CreatedAt time.Time      // CreatedAt Agent 实例创建时间
	RoleDefID string         // RoleDefID 角色定义唯一标识
	// ActivityKind 最近活动种类（TODO 第10项②展示面：llm_start/llm_end/tool:<名>/
	// tool_end/stream/user_wait/descendant）；空串表示无活动监控条目。
	ActivityKind string
	// LastActivityAgo 最近活动距今时长（"5m12s" 格式，展示用）；空串同上。
	LastActivityAgo string
}
