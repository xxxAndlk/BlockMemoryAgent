package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/team"
)

func contEnv(t *testing.T, mode string, max, noProgress int) (*Dispatcher, *orchestrator.Tree) {
	t.Helper()
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.WithAutoResume(mode, max, noProgress)
	tr := orchestrator.NewTree("s1", nil)
	d.WithTree(func(string) *orchestrator.Tree { return tr })
	tr.Register(orchestrator.Node{ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "同域", Status: orchestrator.StatusRunning})
	tr.Pause("s1/domain-1", "token budget")
	return d, tr
}

func TestAutoResumableCauses(t *testing.T) {
	cases := map[team.PauseCause]bool{
		team.PauseContextThreshold: true,
		team.PauseIterationLimit:   true,
		team.PauseUser:             false,
		team.PauseApproval:         false,
		team.PauseClarification:    false,
		team.PausePermission:       false,
		team.PauseTeamBudget:       false,
		team.PauseWallClock:        false,
		team.PauseContextInvalid:   false,
		team.PauseVerification:     false,
		team.PauseUncertain:        false,
	}
	for cause, want := range cases {
		if got := team.AutoResumable(cause); got != want {
			t.Fatalf("AutoResumable(%s)=%v want %v", cause, got, want)
		}
	}
}

func TestProgressDigestDetectsNewEvidence(t *testing.T) {
	assistant := agent.ReactMessage{Role: "assistant", Content: "我将继续"}
	same := []agent.ReactMessage{assistant, assistant}
	clone := append([]agent.ReactMessage{}, same...)
	if progressDigest(same) != progressDigest(clone) {
		t.Fatal("digest must be deterministic")
	}
	toolRes, _ := json.Marshal(agent.ToolResult{Tool: "RunCommand", Success: true})
	withTool := append(append([]agent.ReactMessage{}, same...), agent.ReactMessage{Role: "tool", ToolCallID: "c1", Content: string(toolRes)})
	if progressDigest(same) == progressDigest(withTool) {
		t.Fatal("new successful tool evidence must change digest")
	}
}

// TestScheduleShadowKeepsManualPath shadow 只记录，不改变人工暂停链路。
func TestScheduleShadowKeepsManualPath(t *testing.T) {
	d, _ := contEnv(t, AutoResumeShadow, 2, 1)
	st := d.recordContinuationCheckpoint(context.Background(), "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "", "")
	if st == nil {
		t.Fatal("checkpoint not recorded")
	}
	d.scheduleContinuation(context.Background(), "s1", "s1/domain-1")
	if got := d.continuationBudget("s1").Load(); got != 0 {
		t.Fatalf("shadow must not consume auto-resume budget: %d", got)
	}
	if d.continuationPending("s1/domain-1") {
		t.Fatal("shadow must not schedule auto-resume")
	}
	if !d.HasPausedChild("s1") {
		t.Fatal("shadow must preserve paused-child manual path")
	}
}

// TestScheduleEnforceBudgetFuse 任务累计预算耗尽后不再自动续跑。
func TestScheduleEnforceBudgetFuse(t *testing.T) {
	d, _ := contEnv(t, AutoResumeEnforce, 1, 1)
	d.recordContinuationCheckpoint(context.Background(), "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "", "")
	d.continuationBudget("s1").Inc() // 已用满 1 次
	d.scheduleContinuation(context.Background(), "s1", "s1/domain-1")
	if d.continuationPending("s1/domain-1") {
		t.Fatal("budget exhausted must not schedule")
	}
	if !d.HasPausedChild("s1") {
		t.Fatal("blocked continuation must surface paused child")
	}
}

// TestHasPausedChildSkipsPending 已安排自动续跑的节点不算人工暂停。
func TestHasPausedChildSkipsPending(t *testing.T) {
	d, _ := contEnv(t, AutoResumeEnforce, 2, 1)
	st := d.recordContinuationCheckpoint(context.Background(), "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "", "")
	st.mu.Lock()
	st.scheduled = true
	st.blocked = false
	st.mu.Unlock()
	if d.HasPausedChild("s1") {
		t.Fatal("continuation-pending child must not trigger session pause")
	}
}

// TestFindResumableDuplicate 可恢复实例存在时同任务不得重派。
func TestFindResumableDuplicate(t *testing.T) {
	d, _ := contEnv(t, AutoResumeEnforce, 2, 1)
	d.recordContinuationCheckpoint(context.Background(), "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "", "")
	if id, ok := d.findResumableDuplicate("s1", "同域", nil); !ok || id != "s1/domain-1" {
		t.Fatalf("domain duplicate not detected: %q %v", id, ok)
	}
	if _, ok := d.findResumableDuplicate("s1", "他域", nil); ok {
		t.Fatal("different domain must not be treated as duplicate")
	}
	bnd := &team.TaskBinding{TaskID: "task-a", AttemptID: "att-1"}
	if _, ok := d.findResumableDuplicate("s1", "", bnd); ok {
		t.Fatal("legacy checkpoint without task_id must not match bound task")
	}
}

// TestRepauseResetsScheduled 第二次触限必须复位 single-flight，否则永久等待。
func TestRepauseResetsScheduled(t *testing.T) {
	d, _ := contEnv(t, AutoResumeEnforce, 2, 1)
	ctx := withTaskBinding(context.Background(), team.TaskBinding{TaskID: "t1", AttemptID: "a1"})
	st := d.recordContinuationCheckpoint(ctx, "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "t1", "a1")
	st.mu.Lock()
	st.scheduled = true
	st.mu.Unlock()
	// 恢复后再次触限 → 新检查点。
	st2 := d.recordContinuationCheckpoint(ctx, "s1/domain-1", "s1", "s1", "domain", team.PauseContextThreshold, nil, "t1", "a1")
	st2.mu.Lock()
	scheduled := st2.scheduled
	cpTask, cpAttempt := st2.cp.TaskID, st2.cp.AttemptID
	st2.mu.Unlock()
	if scheduled {
		t.Fatal("re-pause must reset scheduled for next continuation")
	}
	if cpTask != "t1" || cpAttempt != "a1" {
		t.Fatalf("re-pause lost task binding: %s/%s", cpTask, cpAttempt)
	}
}

// TestBoundedResumeHistory context_threshold 恢复使用有界视图，iteration_limit 不裁剪。
func TestBoundedResumeHistory(t *testing.T) {
	msgs := []agent.ReactMessage{{Role: "user", Content: "task"}}
	for i := 0; i < 200; i++ {
		msgs = append(msgs, agent.ReactMessage{Role: "assistant", Content: strings.Repeat("x", 4000)})
	}
	got := boundedResumeHistory(msgs, team.PauseContextThreshold)
	if len(got) >= len(msgs) {
		t.Fatalf("context_threshold should bound history: %d -> %d", len(msgs), len(got))
	}
	full := boundedResumeHistory(msgs, team.PauseIterationLimit)
	if len(full) != len(msgs) {
		t.Fatalf("iteration_limit should keep history: %d", len(full))
	}
}

// TestSharedContinuationBudget root 与子 Agent 共用同一任务级计数。
func TestSharedContinuationBudget(t *testing.T) {
	d, _ := contEnv(t, AutoResumeEnforce, 1, 1)
	if n, ok := d.TryConsumeAutoResume("s1"); !ok || n != 1 {
		t.Fatalf("first consume: n=%d ok=%v", n, ok)
	}
	if _, ok := d.TryConsumeAutoResume("s1"); ok {
		t.Fatal("second consume must respect shared max")
	}
	// 同一会话 deadline 首次建立后不延后。
	d1 := d.ContinuationDeadline("s1")
	if d1.IsZero() {
		t.Fatal("deadline not set")
	}
	if d.ContinuationDeadline("s1") != d1 {
		t.Fatal("deadline must not extend on later calls")
	}
}

// TestFinishDispatchAttemptHook C-03 attempt 终态必须回写 done/unverified/failed。
func TestFinishDispatchAttemptHook(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	var gotStatus, gotAttempt, gotResult string
	d.WithAttemptFinisher(func(_ context.Context, attemptID, status, resultID, errText string) {
		gotAttempt, gotStatus, gotResult = attemptID, status, resultID
	})
	ctx := withTaskBinding(context.Background(), team.TaskBinding{TaskID: "t1", AttemptID: "a1", PlanVersion: 1})
	d.finishDispatchAttemptIfNeeded(ctx, "s1/code-1", "ok", nil)
	if gotAttempt != "a1" || gotStatus != "done" || gotResult == "" {
		t.Fatalf("attempt not finalized: %s %s %s", gotAttempt, gotStatus, gotResult)
	}
	d.finishDispatchAttemptIfNeeded(ctx, "s1/code-1", "bad", errVerifyMissing)
	if gotStatus != "unverified" {
		t.Fatalf("verify_missing should map unverified: %s", gotStatus)
	}
}

// TestAutoResumeModeNormalize off/shadow/enforce 归一化，非法值回落 shadow。
func TestAutoResumeModeNormalize(t *testing.T) {
	cases := map[string]string{"": AutoResumeShadow, "off": AutoResumeOff, "shadow": AutoResumeShadow, "ENFORCE": AutoResumeEnforce, "bogus": AutoResumeShadow}
	for in, want := range cases {
		if got := normalizeAutoResumeMode(in); got != want {
			t.Fatalf("normalize(%q)=%q want %q", in, got, want)
		}
	}
}

// TestBlockLeafContinuationBoundTerminal C-06 叶子熔断时看板与 claim 必须同步终结。
func TestBlockLeafContinuationBoundTerminal(t *testing.T) {
	d, tr := contEnv(t, AutoResumeEnforce, 2, 1)
	bd := board.NewTaskBoard("s1", "goal")
	if err := bd.SetPlan("goal", []board.PlanTask{{ID: "t1", Title: "task", Domain: "代码"}}); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if err := bd.AssignBound("t1", "a1", "s1/domain-1/code-1"); err != nil {
		t.Fatalf("assign bound: %v", err)
	}
	d.WithBoard(func(string) *board.TaskBoard { return bd }, nil)
	var attemptStatus, claimStatus string
	d.WithAttemptFinisher(func(_ context.Context, attemptID, status, _, _ string) {
		if attemptID == "a1" {
			attemptStatus = status
		}
	})
	d.WithClaimFinisher(func(_ context.Context, b team.ClaimBinding, status, _, _ string) {
		if b.AttemptID == "a1" && b.WorkerID == "w1" {
			claimStatus = status
		}
	})
	nodeID := "s1/domain-1/code-1"
	tr.Register(orchestrator.Node{ID: nodeID, ParentID: "s1/domain-1", Role: "code_assistant", Domain: "代码", Status: orchestrator.StatusRunning})
	tr.Pause(nodeID, "token budget")
	d.pending.Store("s1/domain-1", &pendingState{})
	d.getOrCreatePending("s1/domain-1").count.Store(1)
	binding := team.TaskBinding{SessionID: "s1", TaskID: "t1", AttemptID: "a1", WorkerID: "w1", Acceptance: []string{"测试 [evidence:command]"}}
	ctx := withTaskBinding(dispatchCtx(), binding)
	st := d.recordContinuationCheckpoint(ctx, nodeID, "s1/domain-1", "s1", "leaf", team.PauseContextThreshold, nil, "t1", "a1")
	d.blockContinuation("s1", nodeID, st, "no_progress")
	if got := taskByID(bd.Snapshot(), "t1"); got.Status != board.TaskUnverified {
		t.Fatalf("board status = %s, want unverified", got.Status)
	}
	if attemptStatus != "" || claimStatus != "delivered-unverified" {
		t.Fatalf("terminal mismatch: attempt=%s claim=%s", attemptStatus, claimStatus)
	}
	if got := d.PendingChildren("s1/domain-1"); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}

// TestBlockLeafContinuationAttemptTerminal C-03 叶子熔断必须将 attempt 置为 unverified。
func TestBlockLeafContinuationAttemptTerminal(t *testing.T) {
	d, tr := contEnv(t, AutoResumeEnforce, 2, 1)
	bd := board.NewTaskBoard("s1", "goal")
	if err := bd.SetPlan("goal", []board.PlanTask{{ID: "t1", Title: "task", Domain: "代码"}}); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	if err := bd.AssignBound("t1", "a1", "s1/domain-1/code-1"); err != nil {
		t.Fatalf("assign bound: %v", err)
	}
	d.WithBoard(func(string) *board.TaskBoard { return bd }, nil)
	var attemptStatus string
	d.WithAttemptFinisher(func(_ context.Context, attemptID, status, _, _ string) {
		if attemptID == "a1" {
			attemptStatus = status
		}
	})
	nodeID := "s1/domain-1/code-1"
	tr.Register(orchestrator.Node{ID: nodeID, ParentID: "s1/domain-1", Role: "code_assistant", Domain: "代码", Status: orchestrator.StatusRunning})
	tr.Pause(nodeID, "token budget")
	d.pending.Store("s1/domain-1", &pendingState{})
	d.getOrCreatePending("s1/domain-1").count.Store(1)
	binding := team.TaskBinding{SessionID: "s1", TaskID: "t1", AttemptID: "a1", Acceptance: []string{"测试 [evidence:command]"}}
	ctx := withTaskBinding(dispatchCtx(), binding)
	st := d.recordContinuationCheckpoint(ctx, nodeID, "s1/domain-1", "s1", "leaf", team.PauseContextThreshold, nil, "t1", "a1")
	d.blockContinuation("s1", nodeID, st, "no_progress")
	if got := taskByID(bd.Snapshot(), "t1"); got.Status != board.TaskUnverified {
		t.Fatalf("board status = %s, want unverified", got.Status)
	}
	if attemptStatus != team.AttemptUnverified {
		t.Fatalf("attempt status = %s, want unverified", attemptStatus)
	}
	if got := d.PendingChildren("s1/domain-1"); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
}

// TestBlockLeafContinuationReleasesParent 叶子无法续跑时通知父并释放未决计数。
func TestBlockLeafContinuationReleasesParent(t *testing.T) {
	d, tr := contEnv(t, AutoResumeEnforce, 2, 1)
	// 叶子节点。
	tr.Register(orchestrator.Node{ID: "s1/domain-1/code-1", ParentID: "s1/domain-1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	tr.Pause("s1/domain-1/code-1", "token budget")
	d.pending.Store("s1/domain-1", &pendingState{})
	d.getOrCreatePending("s1/domain-1").count.Store(1)
	st := d.recordContinuationCheckpoint(context.Background(), "s1/domain-1/code-1", "s1/domain-1", "s1", "leaf", team.PauseContextThreshold, nil, "", "")
	d.blockContinuation("s1", "s1/domain-1/code-1", st, "no_progress")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := tr.Get("s1/domain-1/code-1"); ok && n.Status == orchestrator.StatusUnverified {
			if d.PendingChildren("s1/domain-1") != 0 {
				t.Fatalf("leaf blocked must release parent pending: %d", d.PendingChildren("s1/domain-1"))
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	n, _ := tr.Get("s1/domain-1/code-1")
	t.Fatalf("leaf should be marked delivered-unverified, got %+v", n)
}
