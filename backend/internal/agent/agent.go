package agent

import "context"

// Agent 是 Agent 编排模块的统一外观接口（Facade）。
// HTTP 层与 TUI 层应依赖此接口，而不是直接访问 internal/graph 或 internal/runtime 等内部包，
// 从而降低上层与内部实现的耦合度，方便后续替换或扩展具体实现。
type Agent interface {
	// CreateSession 创建一个新的会话。
	// ctx 用于控制请求的超时与取消；req 携带创建会话所需的初始参数。
	// 返回创建的 Session 指针，若创建失败则返回非 nil 的 error。
	CreateSession(ctx context.Context, req CreateRequest) (*Session, error)

	// ResumeSession 恢复一个已存在的会话。
	// ctx 用于控制请求的超时与取消；sessionID 为要恢复的会话唯一标识；req 携带恢复时的附加参数。
	// 返回恢复后的 Session 指针，若会话不存在或恢复失败则返回 error。
	ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error)

	// Send 向指定会话发送一条用户或外部消息。
	// ctx 用于控制请求的超时与取消；sessionID 为目标会话 ID；msg 为待发送的消息内容。
	// 若会话不存在或发送失败，返回对应的 error。
	Send(ctx context.Context, sessionID string, msg Message) error

	// Stream 获取指定会话的事件流，用于向客户端推送运行过程中的事件。
	// ctx 用于控制流的生命周期；sessionID 为目标会话 ID。
	// 返回只读事件通道，若会话不存在或建立流失败则返回 error。
	Stream(ctx context.Context, sessionID string) (<-chan Event, error)

	// Query 在指定会话上执行一次查询。
	// ctx 用于控制请求的超时与取消；sessionID 为目标会话 ID；q 为查询条件。
	// 返回查询结果 Result，若查询失败则返回 error。
	Query(ctx context.Context, sessionID string, q Query) (Result, error)

	// Control 向指定会话发送控制命令，例如暂停、继续、终止等。
	// ctx 用于控制请求的超时与取消；sessionID 为目标会话 ID；cmd 为控制命令。
	// 若命令执行失败或会话不存在，返回 error。
	Control(ctx context.Context, sessionID string, cmd ControlCommand) error

	// List 按照给定过滤条件列出所有符合条件的会话。
	// ctx 用于控制请求的超时与取消；filter 为过滤条件。
	// 返回会话指针切片，若查询失败则返回 error。
	List(ctx context.Context, filter Filter) ([]*Session, error)

	// Get 根据会话 ID 获取单个会话的详细信息。
	// ctx 用于控制请求的超时与取消；sessionID 为要查询的会话 ID。
	// 返回会话指针，若会话不存在或查询失败则返回 error。
	Get(ctx context.Context, sessionID string) (*Session, error)

	// ListAgents 列出指定会话当前运行中的所有 Agent 实例。
	// ctx 用于控制请求的超时与取消；sessionID 为目标会话 ID。
	// 返回 AgentInstance 切片，若会话不存在或查询失败则返回 error。
	ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error)

	// Shutdown 优雅关闭整个 Agent 编排模块，释放资源并停止后台任务。
	// ctx 用于控制关闭操作的超时与取消。
	// 若关闭过程中出现错误，返回 error。
	Shutdown(ctx context.Context) error

	// SummarizeTaskTitle 将较长的任务标题压缩为简短的展示标题。
	// 该能力暴露在外观层，使 TUI 无需直接依赖 ModelFactory 即可获得摘要结果。
	// ctx 用于控制请求的超时与取消；title 为原始任务标题。
	// 返回摘要后的字符串。
	SummarizeTaskTitle(ctx context.Context, title string) string
}
