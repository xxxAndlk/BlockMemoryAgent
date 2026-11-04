package team

// claim_store.go 自认领存储接口与内存实现（C-06，默认关闭）。
// 事务语义：认领/心跳/完成/失败/释放都必须带 ClaimBinding fencing；
// 内存版用单锁模拟事务，PG 版由 store/team_task_store.go 用真实事务实现。

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TaskState 任务当前投影。
type TaskState struct {
	Contract   TaskContract `json:"contract"`
	Status     string       `json:"status"`
	AttemptNo  int          `json:"attempt_no"`
	AttemptID  string       `json:"attempt_id,omitempty"`
	WorkerID   string       `json:"worker_id,omitempty"`
	LeaseUntil time.Time    `json:"lease_until,omitempty"`
	Result     string       `json:"result,omitempty"`
}

// ClaimStore 自认领协调存储。
type ClaimStore interface {
	InitPlan(ctx context.Context, env PlanEnvelope) error
	ListSessions(ctx context.Context) ([]string, error)
	ListTasks(ctx context.Context, sessionID string) ([]TaskState, error)
	ClaimNext(ctx context.Context, sessionID, roleID, workerID string, lease time.Duration) (*Claim, error)
	Heartbeat(ctx context.Context, b ClaimBinding, lease time.Duration) error
	Complete(ctx context.Context, b ClaimBinding, res ClaimResult) error
	Fail(ctx context.Context, b ClaimBinding, status, errText string) error
	Release(ctx context.Context, b ClaimBinding) error
}

// MemoryClaimStore 进程内实现（测试/无 PG）。
type MemoryClaimStore struct {
	mu    sync.Mutex
	plans map[string]PlanEnvelope
	tasks map[string]map[string]*TaskState
	seq   int64
}

// NewMemoryClaimStore 构造内存自认领存储。
func NewMemoryClaimStore() *MemoryClaimStore {
	return &MemoryClaimStore{plans: map[string]PlanEnvelope{}, tasks: map[string]map[string]*TaskState{}}
}

// InitPlan 实现 ClaimStore。
func (m *MemoryClaimStore) InitPlan(_ context.Context, env PlanEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := NormalizePlan(&env, nil); err != nil {
		return err
	}
	if old, ok := m.plans[env.SessionID]; ok && old.PlanVersion >= env.PlanVersion {
		return fmt.Errorf("plan: version %d not greater than current %d", env.PlanVersion, old.PlanVersion)
	}
	m.plans[env.SessionID] = env
	tmap := map[string]*TaskState{}
	for _, t := range env.Tasks {
		tmap[t.TaskID] = &TaskState{Contract: t, Status: TaskPending}
	}
	m.tasks[env.SessionID] = tmap
	return nil
}

// ListSessions 实现 ClaimStore。
func (m *MemoryClaimStore) ListSessions(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.plans))
	for sid := range m.plans {
		out = append(out, sid)
	}
	sort.Strings(out)
	return out, nil
}

// ListTasks 实现 ClaimStore。
func (m *MemoryClaimStore) ListTasks(_ context.Context, sessionID string) ([]TaskState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tmap := m.tasks[sessionID]
	out := make([]TaskState, 0, len(tmap))
	for _, st := range tmap {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contract.TaskID < out[j].Contract.TaskID })
	return out, nil
}

// ClaimNext 实现 ClaimStore（原子认领）。
func (m *MemoryClaimStore) ClaimNext(_ context.Context, sessionID, roleID, workerID string, lease time.Duration) (*Claim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.plans[sessionID]
	if !ok || env.ExecutionMode != "self_claim" {
		return nil, ErrNoClaimableTask
	}
	tmap := m.tasks[sessionID]
	done := map[string]bool{}
	inProgressWrites := [][]string{}
	for id, st := range tmap {
		if st.Status == TaskDone {
			done[id] = true
		}
		if st.Status == TaskInProgress {
			inProgressWrites = append(inProgressWrites, st.Contract.WriteSet)
		}
	}
	var candidates []*TaskState
	for _, st := range tmap {
		if st.Status != TaskPending || st.Contract.RoleID != roleID {
			continue
		}
		ready := true
		for _, d := range st.Contract.DependsOn {
			if !done[d] {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if writeSetOverlap(st.Contract.WriteSet, inProgressWrites) {
			continue
		}
		if st.AttemptNo >= st.Contract.MaxAttempts {
			continue
		}
		candidates = append(candidates, st)
	}
	if len(candidates) == 0 {
		return nil, ErrNoClaimableTask
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Contract.Priority != candidates[j].Contract.Priority {
			return candidates[i].Contract.Priority > candidates[j].Contract.Priority
		}
		return candidates[i].Contract.TaskID < candidates[j].Contract.TaskID
	})
	st := candidates[0]
	m.seq++
	st.AttemptNo++
	st.AttemptID = fmt.Sprintf("%s|%d|%s|a%d", sessionID, env.PlanVersion, st.Contract.TaskID, st.AttemptNo)
	st.WorkerID = workerID
	st.Status = TaskInProgress
	st.LeaseUntil = time.Now().Add(lease)
	return &Claim{
		SessionID: sessionID, TaskID: st.Contract.TaskID, AttemptID: st.AttemptID,
		AttemptNo: st.AttemptNo, PlanVersion: env.PlanVersion, SpecVersion: st.Contract.SpecVersion,
		WorkerID: workerID, LeaseUntil: st.LeaseUntil, Task: st.Contract,
	}, nil
}

// Heartbeat 实现 ClaimStore。
func (m *MemoryClaimStore) Heartbeat(_ context.Context, b ClaimBinding, lease time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.lookupLocked(b)
	if err != nil {
		return err
	}
	st.LeaseUntil = time.Now().Add(lease)
	return nil
}

// Complete 实现 ClaimStore。
func (m *MemoryClaimStore) Complete(_ context.Context, b ClaimBinding, res ClaimResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.lookupLocked(b)
	if err != nil {
		return err
	}
	st.Status = res.Status
	st.Result = res.Summary
	st.LeaseUntil = time.Time{}
	return nil
}

// Fail 实现 ClaimStore。
func (m *MemoryClaimStore) Fail(_ context.Context, b ClaimBinding, status, errText string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.lookupLocked(b)
	if err != nil {
		return err
	}
	st.Status = status
	st.Result = errText
	st.LeaseUntil = time.Time{}
	return nil
}

// Release 实现 ClaimStore。
func (m *MemoryClaimStore) Release(_ context.Context, b ClaimBinding) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.lookupLocked(b)
	if err != nil {
		return err
	}
	st.Status = TaskPending
	st.LeaseUntil = time.Time{}
	st.WorkerID = ""
	return nil
}

func (m *MemoryClaimStore) lookupLocked(b ClaimBinding) (*TaskState, error) {
	tmap := m.tasks[b.SessionID]
	if tmap == nil {
		return nil, ErrAttemptFence
	}
	st := tmap[b.TaskID]
	if st == nil {
		return nil, ErrAttemptFence
	}
	env := m.plans[b.SessionID]
	if env.PlanVersion != b.PlanVersion || st.AttemptID != b.AttemptID || st.WorkerID != b.WorkerID {
		return nil, ErrAttemptFence
	}
	return st, nil
}

func writeSetOverlap(a []string, active [][]string) bool {
	if len(a) == 0 {
		return false
	}
	set := map[string]bool{}
	for _, p := range a {
		set[p] = true
	}
	for _, ws := range active {
		for _, p := range ws {
			if set[p] {
				return true
			}
		}
	}
	return false
}
