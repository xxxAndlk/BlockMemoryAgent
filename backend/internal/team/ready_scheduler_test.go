package team

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/blockmemory/agent/backend/internal/board"
)

type fakeDispatcher struct {
	mu    sync.Mutex
	specs []AutoTaskSpec
	err   error
	n     int
}

func (f *fakeDispatcher) DispatchBoundTask(_ context.Context, spec AutoTaskSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.n++
	f.specs = append(f.specs, spec)
	return spec.SessionID + "/" + spec.RoleID + "-" + spec.TaskID, nil
}

func schedEnv(t *testing.T) (*ReadyScheduler, *board.TaskBoard, *fakeDispatcher) {
	t.Helper()
	b := board.NewTaskBoard("s1", "goal")
	if err := b.SetPlan("", []board.PlanTask{
		{ID: "t1", Title: "A", RoleID: "code_assistant", Task: "实现 A", Acceptance: []string{"tests [evidence:command]"}},
		{ID: "t2", Title: "B", RoleID: "code_assistant", Task: "实现 B", DependsOn: []string{"t1"}},
		{ID: "manual", Title: "人工任务", Domain: "文档"}, // 无 role_id：不进入自动调度
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	b.ApprovePlan()
	fd := &fakeDispatcher{}
	s := NewReadyScheduler(func(string) *board.TaskBoard { return b }, func() []string { return []string{"s1"} }, NewMemoryAttemptStore(), fd, nil)
	return s, b, fd
}

// TestReadySchedulerDependencyOrder 只派发依赖就绪任务，不重复派已预留任务。
func TestReadySchedulerDependencyOrder(t *testing.T) {
	s, b, fd := schedEnv(t)
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fd.specs) != 1 || fd.specs[0].TaskID != "t1" || fd.specs[0].AttemptID == "" {
		t.Fatalf("only t1 should dispatch: %+v", fd.specs)
	}
	// 再次 reconcile：t1 已有 active attempt，不应重复派。
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if len(fd.specs) != 1 {
		t.Fatalf("active attempt must not redispatch: %d", len(fd.specs))
	}
	// t1 完成后 t2 就绪。
	if err := b.MarkDone("t1", "ok"); err != nil {
		t.Fatalf("MarkDone: %v", err)
	}
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if len(fd.specs) != 2 || fd.specs[1].TaskID != "t2" {
		t.Fatalf("t2 should dispatch after t1 done: %+v", fd.specs)
	}
}

// TestReadySchedulerRequiresApproval 未人工批准的计划不得自动执行。
func TestReadySchedulerRequiresApproval(t *testing.T) {
	b := board.NewTaskBoard("s1", "goal")
	if err := b.SetPlan("", []board.PlanTask{{ID: "t1", Title: "A", RoleID: "code_assistant", Task: "do"}}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	fd := &fakeDispatcher{}
	s := NewReadyScheduler(func(string) *board.TaskBoard { return b }, func() []string { return []string{"s1"} }, NewMemoryAttemptStore(), fd, nil)
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(fd.specs) != 0 {
		t.Fatalf("unapproved plan must not dispatch: %+v", fd.specs)
	}
	if !b.ApprovePlan() {
		t.Fatal("approve should succeed with tasks")
	}
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile approved: %v", err)
	}
	if len(fd.specs) != 1 {
		t.Fatalf("approved plan should dispatch: %+v", fd.specs)
	}
}

// TestReadySchedulerFailureBlocksDependents 派发失败按 block_dependents 标阻塞。
func TestReadySchedulerFailureBlocksDependents(t *testing.T) {
	s, b, fd := schedEnv(t)
	fd.err = errors.New("dispatch rejected")
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	snap := b.Snapshot()
	for _, task := range snap.Tasks {
		if task.ID == "t1" && task.Status != board.TaskBlocked {
			t.Fatalf("failed dispatch should block t1: %+v", task)
		}
	}
}

// TestInterruptedRecoveryPolicy 默认不重放 interrupted；显式开启后建新 attempt_no。
func TestInterruptedRecoveryPolicy(t *testing.T) {
	st := NewMemoryAttemptStore()
	a := &DispatchAttempt{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptNo: 0}
	if created, _, err := st.Reserve(context.Background(), a); err != nil || !created {
		t.Fatalf("reserve: %v", err)
	}
	if n, err := st.MarkInterrupted(context.Background()); err != nil || n != 1 {
		t.Fatalf("mark interrupted: n=%d err=%v", n, err)
	}

	b := board.NewTaskBoard("s1", "goal")
	_ = b.SetPlan("", []board.PlanTask{{ID: "t1", Title: "A", RoleID: "code_assistant", Task: "do"}})
	b.ApprovePlan()
	fd := &fakeDispatcher{}
	s := NewReadyScheduler(func(string) *board.TaskBoard { return b }, func() []string { return []string{"s1"} }, st, fd, nil)
	// 默认 closed：interrupted 计入 max_attempts，不重派。
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile closed: %v", err)
	}
	if len(fd.specs) != 0 {
		t.Fatalf("interrupted must not auto-replay by default: %+v", fd.specs)
	}
	// 显式开启：新建 attempt_no=1，不重放旧 attempt。
	s.ResumeInterrupted = true
	if err := s.Reconcile(context.Background(), "s1"); err != nil {
		t.Fatalf("reconcile open: %v", err)
	}
	if len(fd.specs) != 1 {
		t.Fatalf("resume_interrupted should create new attempt: %+v", fd.specs)
	}
	attempts, _ := st.ListBySession(context.Background(), "s1")
	if len(attempts) != 2 {
		t.Fatalf("expected old + new attempt, got %d", len(attempts))
	}
}

// TestAttemptMarkTerminal attempt 终态与 result_id 可回写。
func TestAttemptMarkTerminal(t *testing.T) {
	st := NewMemoryAttemptStore()
	a := &DispatchAttempt{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptNo: 0}
	if created, _, err := st.Reserve(context.Background(), a); err != nil || !created {
		t.Fatalf("reserve: %v", err)
	}
	if err := st.MarkTerminal(context.Background(), a.AttemptID, AttemptDone, "res-1", ""); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
	all, _ := st.ListBySession(context.Background(), "s1")
	if len(all) != 1 || all[0].Status != AttemptDone || all[0].ResultID != "res-1" || all[0].FinishedAt == nil {
		t.Fatalf("terminal not persisted: %+v", all)
	}
}

// TestAttemptTerminalFencing interrupted/已终结 attempt 拒绝迟到完成与非法状态。
func TestAttemptTerminalFencing(t *testing.T) {
	st := NewMemoryAttemptStore()
	a := &DispatchAttempt{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptNo: 0}
	if created, _, err := st.Reserve(context.Background(), a); err != nil || !created {
		t.Fatalf("reserve: %v", err)
	}
	if err := st.MarkTerminal(context.Background(), a.AttemptID, AttemptInterrupted, "", ""); err == nil {
		t.Fatal("execution callback must not write interrupted")
	}
	if _, err := st.MarkInterrupted(context.Background()); err != nil {
		t.Fatalf("mark interrupted: %v", err)
	}
	if err := st.MarkTerminal(context.Background(), a.AttemptID, AttemptDone, "late", ""); !errors.Is(err, ErrAttemptFenced) {
		t.Fatalf("late completion must be fenced, got %v", err)
	}
	all, _ := st.ListBySession(context.Background(), "s1")
	if len(all) != 1 || all[0].Status != AttemptInterrupted || all[0].ResultID != "" {
		t.Fatalf("fenced completion changed attempt: %+v", all)
	}
}

// TestMemoryAttemptStoreUnique 唯一键重复预留返回 created=false。
func TestMemoryAttemptStoreUnique(t *testing.T) {
	st := NewMemoryAttemptStore()
	a := &DispatchAttempt{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptNo: 0}
	created, _, err := st.Reserve(context.Background(), a)
	if err != nil || !created {
		t.Fatalf("first reserve: created=%v err=%v", created, err)
	}
	b := &DispatchAttempt{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptNo: 0}
	created, existing, err := st.Reserve(context.Background(), b)
	if err != nil || created || existing == nil || existing.AttemptID != a.AttemptID {
		t.Fatalf("duplicate reserve: created=%v existing=%+v err=%v", created, existing, err)
	}
}
