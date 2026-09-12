// Package agent 声明 agent 模块对外暴露的预定义错误，
// 调用方可通过 errors.Is 进行精确匹配与统一错误处理。
package agent

// import 标准库的 errors 包，用于创建预定义的哨兵错误。
import (
	"errors"
	"fmt"
)

// 该 var 块集中声明 agent 模块对外暴露的预定义错误。
// 这些错误可在调用方通过 errors.Is 进行精确匹配，便于统一错误处理。
var (
	// ErrSessionNotFound 表示根据会话 ID 查询时未找到对应会话。
	ErrSessionNotFound = errors.New("agent: session not found")

	// ErrSessionFinished 表示目标会话已经处于完成/结束状态，不再接受新操作。
	ErrSessionFinished = errors.New("agent: session already finished")

	// ErrQueueFull 表示会话内部的命令队列已达到容量上限，无法继续入队。
	ErrQueueFull = errors.New("agent: command queue full")

	// ErrInvalidSessionState 表示当前会话状态不满足执行该操作的前提条件。
	ErrInvalidSessionState = errors.New("agent: invalid session state")

	// ErrPostgresUnavailable 表示 PostgreSQL 数据库连接不可用或服务未响应。
	ErrPostgresUnavailable = errors.New("agent: postgres unavailable")

	// ErrAgentNotFound 表示按实例 ID 查找的 Agent 节点不存在或已终结，无法取消。
	ErrAgentNotFound = errors.New("agent: agent not found")

	// ErrAgentBusy 表示目标 Agent 正在执行任务（非等子返回态），不接受用户直连消息。
	// 前端据此禁用发送框并引导先中断/终止（编排页对话面板三态发送）。
	ErrAgentBusy = errors.New("agent 正在执行任务")

	// ErrAgentNotDirectable 表示编排页用户直连的目标不在可直连状态：
	// Paused（走监控页恢复）/ Idle（热驻待复用，经主 Agent 派发）/ meta（走主对话页）
	// / 直连通道未接线。是 ErrInvalidSessionState 的细化（errors.Is 仍可匹配父错误），
	// HTTP 层映射 409 冲突而非 400 参数错——前端据此提示"该状态不可直连"。
	ErrAgentNotDirectable = fmt.Errorf("%w: 该状态不可直连", ErrInvalidSessionState)

)
