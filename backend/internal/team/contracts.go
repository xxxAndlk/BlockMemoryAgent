package team

// contracts.go C-06 自认领中立契约（默认关闭）。
// 只定义规划者冻结的任务契约、认领与错误；Worker 认领后复用 Dispatcher 执行链，
// 不新增第二套 Agent 引擎，也不把 claim 变成模型自由工具。

import (
	"fmt"
	"strings"
	"time"
)

// 自认领冻结状态。
const (
	TaskPending    = "pending"
	TaskInProgress = "in_progress"
	TaskDone       = "done"
	TaskUnverified = "delivered-unverified"
	TaskFailed     = "failed"
	TaskBlocked    = "blocked"
	TaskCancelled  = "cancelled"

	ClaimAttemptClaimed   = "claimed"
	ClaimAttemptRunning   = "running"
	ClaimAttemptCompleted = "completed"
	ClaimAttemptFailed    = "failed"
	ClaimAttemptExpired   = "expired"
	ClaimAttemptCancelled = "cancelled"
)

// 自认领错误（调用方只对 ErrNoClaimableTask 做退避）。
var (
	ErrNoClaimableTask    = fmt.Errorf("no claimable task")
	ErrTaskAlreadyClaimed = fmt.Errorf("task already claimed")
	ErrTaskNotReady       = fmt.Errorf("task dependencies not ready")
	ErrPlanVersionStale   = fmt.Errorf("plan version stale")
	ErrNotEligible        = fmt.Errorf("worker not eligible")
	ErrAttemptFence       = fmt.Errorf("attempt fence rejected")
)

// PlanEnvelope 规划者冻结计划。
type PlanEnvelope struct {
	SessionID         string         `json:"session_id"`
	PlanVersion       int64          `json:"plan_version"`
	ExecutionMode     string         `json:"execution_mode"` // 仅 self_claim
	Goal              string         `json:"goal"`
	IntegrationTaskID string         `json:"integration_task_id"`
	Tasks             []TaskContract `json:"tasks"`
}

// TaskContract 冻结任务契约。
type TaskContract struct {
	TaskID         string   `json:"task_id"`
	RoleID         string   `json:"role_id"`
	Domain         string   `json:"domain,omitempty"`
	Task           string   `json:"task"`
	Responsibility string   `json:"responsibility,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	VerifyKind     string   `json:"verify_kind,omitempty"`
	DependsOn      []string `json:"depends_on,omitempty"`
	Acceptance     []string `json:"acceptance,omitempty"`
	RequiredTools  []string `json:"required_tools,omitempty"`
	ReadSet        []string `json:"read_set,omitempty"`
	WriteSet       []string `json:"write_set,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	MaxAttempts    int      `json:"max_attempts,omitempty"`
	SpecVersion    string   `json:"spec_version,omitempty"`
	Integration    bool     `json:"integration,omitempty"`
	Risk           string   `json:"risk,omitempty"`
}

// Claim 一次原子认领结果。
type Claim struct {
	SessionID   string       `json:"session_id"`
	TaskID      string       `json:"task_id"`
	AttemptID   string       `json:"attempt_id"`
	AttemptNo   int          `json:"attempt_no"`
	PlanVersion int64        `json:"plan_version"`
	SpecVersion string       `json:"spec_version,omitempty"`
	WorkerID    string       `json:"worker_id"`
	LeaseUntil  time.Time    `json:"lease_until"`
	Task        TaskContract `json:"task"`
}

// ClaimBinding 执行/心跳/完成 fencing。
type ClaimBinding struct {
	SessionID   string `json:"session_id"`
	TaskID      string `json:"task_id"`
	AttemptID   string `json:"attempt_id"`
	WorkerID    string `json:"worker_id"`
	AttemptNo   int    `json:"attempt_no"`
	PlanVersion int64  `json:"plan_version"`
	SpecVersion string `json:"spec_version,omitempty"`
}

// ClaimResult 完成结果（由 B-03 AgentResult 摘要而来）。
type ClaimResult struct {
	Status   string `json:"status"` // done/delivered-unverified/failed
	Summary  string `json:"summary"`
	ResultID string `json:"result_id,omitempty"`
}

// NormalizePlan 校验并冻结计划。调用前应已通过角色/工具/审批策略校验。
func NormalizePlan(env *PlanEnvelope, allowedRoles map[string]bool) error {
	if env == nil {
		return fmt.Errorf("plan envelope required")
	}
	env.SessionID = strings.TrimSpace(env.SessionID)
	env.Goal = strings.TrimSpace(env.Goal)
	env.ExecutionMode = strings.TrimSpace(env.ExecutionMode)
	if env.SessionID == "" {
		return fmt.Errorf("plan: session_id required")
	}
	if env.ExecutionMode != "self_claim" {
		return fmt.Errorf("plan: execution_mode must be self_claim, got %q", env.ExecutionMode)
	}
	if env.PlanVersion <= 0 {
		env.PlanVersion = 1
	}
	if len(env.Tasks) == 0 {
		return fmt.Errorf("plan: tasks required")
	}
	ids := map[string]bool{}
	for _, t := range env.Tasks {
		ids[t.TaskID] = true
	}
	integration := 0
	for i := range env.Tasks {
		t := &env.Tasks[i]
		t.TaskID = strings.TrimSpace(t.TaskID)
		t.RoleID = strings.TrimSpace(t.RoleID)
		t.Task = strings.TrimSpace(t.Task)
		if t.TaskID == "" || t.Task == "" || t.RoleID == "" {
			return fmt.Errorf("plan: task[%d] task_id/role_id/task required", i)
		}
		if allowedRoles != nil && !allowedRoles[t.RoleID] {
			return fmt.Errorf("plan: role %q not allowed for self_claim", t.RoleID)
		}
		if t.MaxAttempts <= 0 {
			t.MaxAttempts = 1
		}
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("plan: task %s depends on unknown %s", t.TaskID, dep)
			}
		}
		if t.Integration {
			integration++
		}
		if t.SpecVersion == "" {
			t.SpecVersion = HashContextContent(t.Task + "|" + strings.Join(t.Acceptance, "|"))
		}
	}
	if integration > 1 {
		return fmt.Errorf("plan: only one integration task allowed")
	}
	if integration == 1 && env.IntegrationTaskID != "" && !ids[env.IntegrationTaskID] {
		return fmt.Errorf("plan: integration_task_id %q not found", env.IntegrationTaskID)
	}
	return validateAcyclic(env.Tasks)
}

func validateAcyclic(tasks []TaskContract) error {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.TaskID] = t.DependsOn
	}
	color := map[string]int{}
	var dfs func(string) error
	dfs = func(id string) error {
		color[id] = 1
		for _, d := range deps[id] {
			switch color[d] {
			case 1:
				return fmt.Errorf("plan: dependency cycle at %s -> %s", id, d)
			case 0:
				if err := dfs(d); err != nil {
					return err
				}
			}
		}
		color[id] = 2
		return nil
	}
	for id := range deps {
		if color[id] == 0 {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}
