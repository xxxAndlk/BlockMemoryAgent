package team

import (
	"context"
	"errors"
	"testing"
	"time"
)

func samplePlan() PlanEnvelope {
	return PlanEnvelope{
		SessionID: "s1", PlanVersion: 1, ExecutionMode: "self_claim", Goal: "g",
		IntegrationTaskID: "integr",
		Tasks: []TaskContract{
			{TaskID: "a", RoleID: "code_assistant", Task: "实现 A", Priority: 10, MaxAttempts: 2, WriteSet: []string{"src/a.go"}},
			{TaskID: "b", RoleID: "code_assistant", Task: "实现 B", DependsOn: []string{"a"}, WriteSet: []string{"src/b.go"}},
			{TaskID: "integr", RoleID: "test_assistant", Task: "集成", DependsOn: []string{"a", "b"}, Integration: true},
		},
	}
}

func TestPlanValidation(t *testing.T) {
	env := samplePlan()
	if err := NormalizePlan(&env, map[string]bool{"code_assistant": true, "test_assistant": true}); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	bad := samplePlan()
	bad.Tasks[1].DependsOn = []string{"missing"}
	if err := NormalizePlan(&bad, nil); err == nil {
		t.Fatal("unknown dependency must be rejected")
	}
	cycle := samplePlan()
	cycle.Tasks[0].DependsOn = []string{"b"}
	if err := NormalizePlan(&cycle, nil); err == nil {
		t.Fatal("cycle must be rejected")
	}
	dupInteg := samplePlan()
	dupInteg.Tasks[0].Integration = true
	if err := NormalizePlan(&dupInteg, nil); err == nil {
		t.Fatal("multiple integration tasks must be rejected")
	}
}

func TestClaimDependencyAndFencing(t *testing.T) {
	st := NewMemoryClaimStore()
	svc := &ClaimService{Store: st, AllowedRoles: map[string]bool{"code_assistant": true, "test_assistant": true}}
	ctx := context.Background()
	if err := svc.InitPlan(ctx, samplePlan()); err != nil {
		t.Fatalf("init plan: %v", err)
	}
	c1, err := svc.ClaimNext(ctx, "s1", "code_assistant", "w1", time.Minute)
	if err != nil || c1.TaskID != "a" {
		t.Fatalf("first claim should be a: %+v err=%v", c1, err)
	}
	// 依赖未完成：b 不可认领。
	if _, err := svc.ClaimNext(ctx, "s1", "code_assistant", "w2", time.Minute); !errors.Is(err, ErrNoClaimableTask) {
		t.Fatalf("dependent b must not be claimable: %v", err)
	}
	// 错误 attempt/worker fencing。
	bad := ClaimBinding{SessionID: "s1", TaskID: "a", AttemptID: "wrong", WorkerID: "w1", AttemptNo: c1.AttemptNo, PlanVersion: 1}
	if err := svc.Complete(ctx, bad, ClaimResult{Status: TaskDone}); !errors.Is(err, ErrAttemptFence) {
		t.Fatalf("wrong attempt must be fenced: %v", err)
	}
	good := ClaimBinding{SessionID: "s1", TaskID: "a", AttemptID: c1.AttemptID, WorkerID: "w1", AttemptNo: c1.AttemptNo, PlanVersion: 1}
	if err := svc.Complete(ctx, good, ClaimResult{Status: TaskDone, Summary: "ok"}); err != nil {
		t.Fatalf("complete a: %v", err)
	}
	c2, err := svc.ClaimNext(ctx, "s1", "code_assistant", "w3", time.Minute)
	if err != nil || c2.TaskID != "b" {
		t.Fatalf("b should be claimable after a done: %+v err=%v", c2, err)
	}
}

func TestClaimWriteSetOverlap(t *testing.T) {
	plan := PlanEnvelope{
		SessionID: "s1", PlanVersion: 1, ExecutionMode: "self_claim",
		Tasks: []TaskContract{
			{TaskID: "x", RoleID: "code_assistant", Task: "X", WriteSet: []string{"same.go"}, MaxAttempts: 1},
			{TaskID: "y", RoleID: "code_assistant", Task: "Y", WriteSet: []string{"same.go"}, MaxAttempts: 1},
		},
	}
	st := NewMemoryClaimStore()
	svc := &ClaimService{Store: st, AllowedRoles: map[string]bool{"code_assistant": true}}
	if err := svc.InitPlan(context.Background(), plan); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := svc.ClaimNext(context.Background(), "s1", "code_assistant", "w1", time.Minute); err != nil {
		t.Fatalf("claim x: %v", err)
	}
	if _, err := svc.ClaimNext(context.Background(), "s1", "code_assistant", "w2", time.Minute); !errors.Is(err, ErrNoClaimableTask) {
		t.Fatalf("overlapping write set must not be claimed: %v", err)
	}
}

func TestClaimHeartbeatAndRelease(t *testing.T) {
	st := NewMemoryClaimStore()
	svc := &ClaimService{Store: st, AllowedRoles: map[string]bool{"code_assistant": true, "test_assistant": true}}
	if err := svc.InitPlan(context.Background(), samplePlan()); err != nil {
		t.Fatalf("init: %v", err)
	}
	c, err := svc.ClaimNext(context.Background(), "s1", "code_assistant", "w1", time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	b := ClaimBinding{SessionID: "s1", TaskID: c.TaskID, AttemptID: c.AttemptID, WorkerID: c.WorkerID, AttemptNo: c.AttemptNo, PlanVersion: 1}
	if err := svc.Heartbeat(context.Background(), b, 2*time.Minute); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := svc.Release(context.Background(), b); err != nil {
		t.Fatalf("release: %v", err)
	}
	tasks, _ := st.ListTasks(context.Background(), "s1")
	for _, ts := range tasks {
		if ts.Contract.TaskID == c.TaskID && ts.Status != TaskPending {
			t.Fatalf("released task should be pending: %+v", ts)
		}
	}
}
