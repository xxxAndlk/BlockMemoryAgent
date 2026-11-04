package team

// binding.go 精确任务绑定（C-03）：把一次派发绑定到已批准计划的 task/attempt，
// 使依赖检查、看板 Assign/终态回写只操作指定任务，旧 attempt 迟到结果被 fence 拒绝。
// 首版（C03-1）只提供契约与兼容路径，不启动自动调度。

import (
	"fmt"
	"strings"
)

// TaskBinding 一次派发的精确任务绑定。
// legacy delegate 允许 TaskID/AttemptID 为空（execution_path=legacy_delegate）；
// auto_dispatch 路径不允许缺 task/attempt（由后续 PR 入口校验）。
type TaskBinding struct {
	SessionID           string `json:"session_id,omitempty"`
	PlanVersion         int64  `json:"plan_version,omitempty"`
	TaskID              string `json:"task_id,omitempty"`
	AttemptID           string `json:"attempt_id,omitempty"`
	AttemptNo           int    `json:"attempt_no,omitempty"`
	// WorkerID C-06 自认领 owner；非空时终态回写走 ClaimService。
	WorkerID            string `json:"worker_id,omitempty"`
	SpecVersion         string `json:"spec_version,omitempty"`
	Risk                string   `json:"risk,omitempty"`
	// Acceptance 冻结计划中的验收清单（C-03/C-06）：该 attempt 的权威验收输入。
	Acceptance          []string `json:"acceptance,omitempty"`
	ContextManifestHash string `json:"context_manifest_hash,omitempty"`
	DispatchKey         string `json:"dispatch_key,omitempty"`
}

// Normalize 归一化并校验；TaskID 必填（调用方仅在显式 task_id 时构造绑定）。
func (b *TaskBinding) Normalize() error {
	b.SessionID = strings.TrimSpace(b.SessionID)
	b.TaskID = strings.TrimSpace(b.TaskID)
	b.AttemptID = strings.TrimSpace(b.AttemptID)
	b.SpecVersion = strings.TrimSpace(b.SpecVersion)
	b.ContextManifestHash = strings.TrimSpace(b.ContextManifestHash)
	b.DispatchKey = strings.TrimSpace(b.DispatchKey)
	if b.TaskID == "" {
		return fmt.Errorf("task binding: task_id required")
	}
	if b.PlanVersion < 0 || b.AttemptNo < 0 {
		return fmt.Errorf("task binding: plan_version/attempt_no must be >= 0")
	}
	if b.AttemptID == "" {
		b.AttemptID = fmt.Sprintf("%s-a%d", b.TaskID, b.AttemptNo)
	}
	if b.DispatchKey == "" {
		b.DispatchKey = fmt.Sprintf("%s|%d|%s|%s", b.SessionID, b.PlanVersion, b.TaskID, b.AttemptID)
	}
	return nil
}

// IsBound 判断绑定是否可精确控制看板任务。
func (b TaskBinding) IsBound() bool {
	return strings.TrimSpace(b.TaskID) != ""
}
