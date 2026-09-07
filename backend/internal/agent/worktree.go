package agent

// worktree.go 定义 worktree 隔离派发（TODO 第9⑤/#10⑤）的跨层 DTO 与结构化接口。
// agent 包不 import subagent（依赖规则），server/service 层经结构化接口断言
// 访问 Dispatcher 的 worktree 能力（与 SoftStopMarker / DestroyAllIdle 同手法）。

import (
	"context"
	"time"
)

// WorktreeView 是 worktree 句柄的 HTTP 线型视图（subagent.Dispatcher.ListWorktrees 返回）。
type WorktreeView struct {
	AgentID    string    `json:"agent_id"`           // 子 Agent 实例 ID（合并门寻址键）
	Domain     string    `json:"domain"`             // 领域名（叶子派发为角色 ID）
	Path       string    `json:"path"`               // worktree 副本绝对路径
	Branch     string    `json:"branch"`             // 专属分支名
	BaseCommit string    `json:"base_commit"`        // 建副本时主仓库 HEAD
	PatchPath  string    `json:"patch_path"`         // 交付 patch 落盘路径（未收尾为空）
	PatchStat  string    `json:"patch_stat"`         // --stat 摘要
	Merged     bool      `json:"merged"`             // 已合入主仓库
	Removed    bool      `json:"removed"`            // 副本已删除
	CreatedAt  time.Time `json:"created_at"`         // 创建时间
}

// WorktreeReader 是 worktree 只读查询能力（ReactService.Query 经 s.stopMarker 断言使用）。
type WorktreeReader interface {
	// ListWorktrees 列出会话全部 worktree 句柄快照。
	ListWorktrees(sessionID string) []WorktreeView
	// WorktreeDiff 返回指定句柄的 patch 全文（review 数据源）。
	WorktreeDiff(sessionID, agentID string) (string, error)
}

// WorktreeOperator 是 worktree 合并门操作能力（Control 通道经断言使用）。
type WorktreeOperator interface {
	// MergeWorktree 执行合并门：base 漂移检测 + 契约检查 + git apply。
	MergeWorktree(ctx context.Context, sessionID, agentID string) (string, error)
	// RejectWorktree 驳回交付，comments 经 mailbox 回该域。
	RejectWorktree(ctx context.Context, sessionID, agentID, comments string) (string, error)
	// CleanupSessionWorktrees 会话终态清理残留副本。
	CleanupSessionWorktrees(sessionID string)
}
