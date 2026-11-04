package board

import (
	"strings"
	"testing"
)

// TestBoundTaskIsolation 同 Domain 两条任务精确绑定互不串状态。
func TestBoundTaskIsolation(t *testing.T) {
	b := NewTaskBoard("s1", "goal")
	if err := b.SetPlan("", []PlanTask{
		{ID: "a", Title: "A", Domain: "同域"},
		{ID: "b", Title: "B", Domain: "同域"},
	}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if v := b.Snapshot().PlanVersion; v != 1 {
		t.Fatalf("plan version = %d, want 1", v)
	}
	if err := b.AssignBound("a", "att-1", "agent-1"); err != nil {
		t.Fatalf("AssignBound a: %v", err)
	}
	snap := b.Snapshot()
	for _, task := range snap.Tasks {
		switch task.ID {
		case "a":
			if task.Status != TaskInProgress || task.Assignee != "agent-1" || task.AttemptID != "att-1" {
				t.Fatalf("task a mismatch: %+v", task)
			}
		case "b":
			if task.Status != TaskPending || task.Assignee != "" {
				t.Fatalf("task b must stay pending: %+v", task)
			}
		}
	}
	// 旧 attempt 迟到结果被 fence 拒绝，不改状态。
	if err := b.FinalizeBound("a", "att-old", TaskDone, "late"); err == nil || !strings.Contains(err.Error(), "stale attempt") {
		t.Fatalf("stale attempt should be rejected: %v", err)
	}
	if err := b.FinalizeBound("a", "att-1", TaskDone, "ok"); err != nil {
		t.Fatalf("FinalizeBound a: %v", err)
	}
	snap = b.Snapshot()
	if snap.Tasks[0].Status != TaskDone || snap.Tasks[1].Status != TaskPending {
		t.Fatalf("finalize must not touch sibling: %+v", snap.Tasks)
	}
	// 后一条任务仍可独立绑定执行。
	if err := b.AssignBound("b", "att-2", "agent-2"); err != nil {
		t.Fatalf("AssignBound b: %v", err)
	}
}

// TestAssignBoundDoesNotResurrectDone 已完成任务不被重派翻回进行中。
func TestAssignBoundDoesNotResurrectDone(t *testing.T) {
	b := NewTaskBoard("s1", "goal")
	_ = b.SetPlan("", []PlanTask{{ID: "a", Title: "A", Domain: "d"}})
	_ = b.AssignBound("a", "att-1", "agent-1")
	_ = b.FinalizeBound("a", "att-1", TaskDone, "ok")
	if err := b.AssignBound("a", "att-2", "agent-2"); err != nil {
		t.Fatalf("AssignBound done: %v", err)
	}
	if got := b.Snapshot().Tasks[0]; got.Status != TaskDone || got.AttemptID != "att-1" {
		t.Fatalf("done task must not be reset: %+v", got)
	}
}

// TestSetPlanReplanUpdatesFieldsAndVersion 重规划更新标题/依赖并单调升版本。
func TestSetPlanReplanUpdatesFieldsAndVersion(t *testing.T) {
	b := NewTaskBoard("s1", "goal")
	_ = b.SetPlan("", []PlanTask{{ID: "a", Title: "旧标题", Domain: "d"}})
	_ = b.SetPlan("", []PlanTask{
		{ID: "a", Title: "新标题", Domain: "d2", DependsOn: []string{}, Acceptance: []string{"验收"}},
	})
	snap := b.Snapshot()
	if snap.PlanVersion != 2 {
		t.Fatalf("plan version = %d, want 2", snap.PlanVersion)
	}
	got := snap.Tasks[0]
	if got.Title != "新标题" || got.Domain != "d2" || len(got.Acceptance) != 1 {
		t.Fatalf("replan must update fields: %+v", got)
	}
}
