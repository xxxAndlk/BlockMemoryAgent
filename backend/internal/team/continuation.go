package team

// continuation.go B-11 执行连续性中立契约：
// 暂停原因、检查点与续跑选项。subagent 只依赖本契约，agent 控制器通过接口适配，
// 避免 agent ↔ subagent 循环依赖。首版实现进程内系统触限同 ID 续跑；
// 跨进程恢复（resume_interrupted）保持默认关闭。

import (
	"fmt"
	"strings"
	"time"
)

// PauseCause 暂停/中断原因。
type PauseCause string

const (
	PauseContextThreshold PauseCause = "context_threshold"
	PauseIterationLimit   PauseCause = "iteration_limit"
	PauseUser             PauseCause = "user_pause"
	PauseApproval         PauseCause = "approval_required"
	PauseClarification    PauseCause = "clarification_required"
	PausePermission       PauseCause = "permission_denied"
	PauseTeamBudget       PauseCause = "team_budget_exhausted"
	PauseWallClock        PauseCause = "wall_clock_exhausted"
	PauseContextInvalid   PauseCause = "required_context_invalid"
	PauseVerification     PauseCause = "verification_failed"
	PauseUncertain        PauseCause = "uncertain_side_effect"
)

// AutoResumable 判断该原因是否允许系统自动续跑。
// 用户暂停/审批/澄清/权限/硬预算/不确定副作用一律人工处理。
func AutoResumable(cause PauseCause) bool {
	switch cause {
	case PauseContextThreshold, PauseIterationLimit:
		return true
	default:
		return false
	}
}

// ContinuationCheckpoint 一次确定的暂停检查点。
type ContinuationCheckpoint struct {
	SessionID        string     `json:"session_id"`
	RunID            string     `json:"run_id"`
	ParentAgentID    string     `json:"parent_agent_id"`
	AgentID          string     `json:"agent_id"`
	NodeID           string     `json:"node_id"`
	TaskID           string     `json:"task_id,omitempty"`
	AttemptID        string     `json:"attempt_id,omitempty"`
	BudgetScopeID    string     `json:"budget_scope_id"`
	OwnerKind        string     `json:"owner_kind"` // root/domain/leaf
	Cause            PauseCause `json:"cause"`
	CheckpointID     string     `json:"checkpoint_id"`
	HistoryRef       string     `json:"history_ref,omitempty"`
	HistoryVersion   int64      `json:"history_version,omitempty"`
	ProgressDigest   string     `json:"progress_digest,omitempty"`
	OriginalDeadline time.Time  `json:"original_deadline,omitempty"`
	ContinuationSeq  int        `json:"continuation_seq"`
	CreatedAt        time.Time  `json:"created_at"`
}

// Normalize 填充预算作用域与标识；旧路径无 task/attempt 允许为空。
func (c *ContinuationCheckpoint) Normalize() error {
	c.SessionID = strings.TrimSpace(c.SessionID)
	c.AgentID = strings.TrimSpace(c.AgentID)
	c.NodeID = strings.TrimSpace(c.NodeID)
	if c.AgentID == "" {
		return fmt.Errorf("continuation checkpoint: agent_id required")
	}
	if c.NodeID == "" {
		c.NodeID = c.AgentID
	}
	if c.BudgetScopeID == "" {
		// legacy 口径：会话级预算作用域，root/domain/leaf 共享同一累计次数。
		c.BudgetScopeID = c.SessionID
		if c.BudgetScopeID == "" {
			c.BudgetScopeID = c.AgentID
		}
	}
	if c.OwnerKind == "" {
		c.OwnerKind = "domain"
	}
	if c.CheckpointID == "" {
		c.CheckpointID = fmt.Sprintf("cp-%s-%d-%d", c.AgentID, c.HistoryVersion, c.CreatedAt.UnixNano())
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now()
	}
	return nil
}

// ResumeOptions 续跑选项（用户/系统来源、单次切片与累计序号）。
type ResumeOptions struct {
	CheckpointID     string     `json:"checkpoint_id,omitempty"`
	Cause            PauseCause `json:"cause,omitempty"`
	RemainingTimeout time.Duration `json:"remaining_timeout,omitempty"`
	ContinuationSeq  int        `json:"continuation_seq,omitempty"`
	Origin           string     `json:"origin,omitempty"` // system_continuation/user_resume
	// Binding 保留 C-03 task/attempt 绑定，重暂停时不得丢失。
	Binding *TaskBinding `json:"binding,omitempty"`
}

// SystemContinuation 判断是否为系统自动续跑（决定重暂停是否再通知父）。
func (o ResumeOptions) SystemContinuation() bool {
	return o.Origin == "system_continuation"
}
