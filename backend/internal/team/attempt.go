package team

// attempt.go C-03 最小持久派发防重（team_dispatch_attempts）：
// 只记录 task/attempt 预留与终态，不建完整队列、不做 Worker 认领（C-06 未立项）。
// 唯一键 (session_id, plan_version, task_id, attempt_no)；dispatch_key 也唯一。

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AttemptStatus 派发尝试状态。
const (
	AttemptReserved    = "reserved"
	AttemptRunning     = "running"
	AttemptDone        = "done"
	AttemptUnverified  = "unverified"
	AttemptFailed      = "failed"
	AttemptInterrupted = "interrupted"
)

// ErrAttemptFenced 表示迟到回调试图改写已终结／已中断的 attempt。
var ErrAttemptFenced = fmt.Errorf("dispatch attempt fenced")

// ValidAttemptTerminal 限制执行回调可写入的终态；interrupted 只能由启动恢复写入。
func ValidAttemptTerminal(status string) bool {
	switch status {
	case AttemptDone, AttemptUnverified, AttemptFailed:
		return true
	default:
		return false
	}
}

// DispatchAttempt 一次受控自动派发尝试。
type DispatchAttempt struct {
	SessionID           string     `json:"session_id"`
	PlanVersion         int64      `json:"plan_version"`
	TaskID              string     `json:"task_id"`
	AttemptNo           int        `json:"attempt_no"`
	AttemptID           string     `json:"attempt_id"`
	DispatchKey         string     `json:"dispatch_key"`
	Status              string     `json:"status"`
	ParentAgentID       string     `json:"parent_agent_id,omitempty"`
	AgentID             string     `json:"agent_id,omitempty"`
	SpecVersion         string     `json:"spec_version,omitempty"`
	ContextManifestHash string     `json:"context_manifest_hash,omitempty"`
	ReservedAt          time.Time  `json:"reserved_at"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	ResultID            string     `json:"result_id,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

// Normalize 填充 attempt_id/dispatch_key 与时间。
func (a *DispatchAttempt) Normalize() error {
	if a.SessionID == "" || a.TaskID == "" {
		return fmt.Errorf("dispatch attempt: session_id and task_id required")
	}
	if a.PlanVersion < 0 || a.AttemptNo < 0 {
		return fmt.Errorf("dispatch attempt: plan_version/attempt_no must be >= 0")
	}
	if a.AttemptID == "" {
		a.AttemptID = fmt.Sprintf("%s|%d|%s|a%d", a.SessionID, a.PlanVersion, a.TaskID, a.AttemptNo)
	}
	if a.DispatchKey == "" {
		a.DispatchKey = a.AttemptID
	}
	if a.Status == "" {
		a.Status = AttemptReserved
	}
	if a.ReservedAt.IsZero() {
		a.ReservedAt = time.Now()
	}
	return nil
}

// AttemptStore 最小持久防重存储（内存/PG 语义一致）。
type AttemptStore interface {
	// Reserve 事务预留；已存在唯一键时返回 created=false + 既有记录。
	Reserve(ctx context.Context, a *DispatchAttempt) (created bool, existing *DispatchAttempt, err error)
	Update(ctx context.Context, a *DispatchAttempt) error
	ListBySession(ctx context.Context, sessionID string) ([]DispatchAttempt, error)
	// MarkInterrupted 进程重启时把 reserved/running 标记为 interrupted（返回条数）。
	// 不自动重放有副作用任务；是否允许新 attempt 由 Scheduler.ResumeInterrupted 决定。
	MarkInterrupted(ctx context.Context) (int, error)
	// MarkTerminal 执行终态回写（done/unverified/failed），绑定 result_id。
	MarkTerminal(ctx context.Context, attemptID, status, resultID, errText string) error
}

// MemoryAttemptStore 进程内实现（测试/无 PG 场景）。
type MemoryAttemptStore struct {
	mu    sync.Mutex
	byID  map[string]DispatchAttempt
	byKey map[string]string
}

// NewMemoryAttemptStore 构造内存 attempt 存储。
func NewMemoryAttemptStore() *MemoryAttemptStore {
	return &MemoryAttemptStore{byID: map[string]DispatchAttempt{}, byKey: map[string]string{}}
}

// Reserve 实现 AttemptStore。
func (s *MemoryAttemptStore) Reserve(_ context.Context, a *DispatchAttempt) (bool, *DispatchAttempt, error) {
	if err := a.Normalize(); err != nil {
		return false, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byKey[a.DispatchKey]; ok {
		cur := s.byID[id]
		return false, &cur, nil
	}
	cp := *a
	s.byID[a.AttemptID] = cp
	s.byKey[a.DispatchKey] = a.AttemptID
	return true, &cp, nil
}

// Update 实现 AttemptStore。
func (s *MemoryAttemptStore) Update(_ context.Context, a *DispatchAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[a.AttemptID]; !ok {
		return fmt.Errorf("dispatch attempt not found: %s", a.AttemptID)
	}
	s.byID[a.AttemptID] = *a
	return nil
}

// MarkInterrupted 实现 AttemptStore（内存）。
func (s *MemoryAttemptStore) MarkInterrupted(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	n := 0
	for id, a := range s.byID {
		if a.Status == AttemptReserved || a.Status == AttemptRunning {
			a.Status = AttemptInterrupted
			a.FinishedAt = &now
			s.byID[id] = a
			n++
		}
	}
	return n, nil
}

// MarkTerminal 实现 AttemptStore（内存）。只有活跃 attempt 可进入执行终态。
func (s *MemoryAttemptStore) MarkTerminal(_ context.Context, attemptID, status, resultID, errText string) error {
	if !ValidAttemptTerminal(status) {
		return fmt.Errorf("invalid dispatch attempt terminal status: %s", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return fmt.Errorf("dispatch attempt not found: %s", attemptID)
	}
	if a.Status != AttemptReserved && a.Status != AttemptRunning {
		return fmt.Errorf("%w: attempt=%s status=%s", ErrAttemptFenced, attemptID, a.Status)
	}
	now := time.Now()
	a.Status = status
	a.ResultID = resultID
	a.LastError = errText
	a.FinishedAt = &now
	s.byID[attemptID] = a
	return nil
}

// ListBySession 实现 AttemptStore。
func (s *MemoryAttemptStore) ListBySession(_ context.Context, sessionID string) ([]DispatchAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DispatchAttempt, 0)
	for _, a := range s.byID {
		if a.SessionID == sessionID {
			out = append(out, a)
		}
	}
	return out, nil
}
