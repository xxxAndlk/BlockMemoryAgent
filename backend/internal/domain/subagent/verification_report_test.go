package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/team"
)

func toolCallMsg(id, name string, input map[string]any) agent.ReactMessage {
	return agent.ReactMessage{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: id, Name: name, Input: input}}}
}

func toolResultMsg(id string, success bool) agent.ReactMessage {
	data, _ := json.Marshal(agent.ToolResult{Tool: "RunCommand", Success: success})
	return agent.ReactMessage{Role: "tool", ToolCallID: id, Content: string(data)}
}

// TestVerificationReportCommandEvidence 逐项报告只认验证类命令的成功证据。
func TestVerificationReportCommandEvidence(t *testing.T) {
	history := []agent.ReactMessage{
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "echo done"}),
		toolResultMsg("c1", true),
		toolCallMsg("c2", "RunCommand", map[string]any{"command": "go test ./..."}),
		toolResultMsg("c2", true),
	}
	rep := buildVerificationReport("a1", "", "", "", history, []string{"测试通过 [evidence:command]"}, nil, nil)
	if rep.Status != VerifyStatusPassed {
		t.Fatalf("report status = %s, want passed (%+v)", rep.Status, rep.Unresolved)
	}
	if len(rep.Items) != 1 || len(rep.Items[0].EvidenceRefs) != 1 {
		t.Fatalf("command evidence refs mismatch: %+v", rep.Items)
	}
	if rep.Items[0].EvidenceRefs[0].Command != "go test ./..." {
		t.Fatalf("unrelated echo must not become evidence: %+v", rep.Items[0].EvidenceRefs)
	}
}

// TestVerificationReportMissingAndLegacy 无证据/无标记验收不得 passed。
func TestVerificationReportMissingAndLegacy(t *testing.T) {
	history := []agent.ReactMessage{
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "echo done"}),
		toolResultMsg("c1", true),
	}
	rep := buildVerificationReport("a1", "", "", "", history,
		[]string{"测试通过 [evidence:command]", "纯人工验收"}, nil, nil)
	if rep.Status != VerifyStatusUnverified {
		t.Fatalf("missing/legacy must be unverified, got %s", rep.Status)
	}
	if rep.Items[0].Status != VerifyItemMissing || rep.Items[1].Status != VerifyItemLegacyUnproven {
		t.Fatalf("item status mismatch: %+v", rep.Items)
	}
	if len(rep.Unresolved) != 2 {
		t.Fatalf("unresolved = %v, want 2 entries", rep.Unresolved)
	}
}

// TestVerificationReportNoAcceptance 无结构化验收时报告保持 unverified（不能声称机器通过）。
func TestVerificationReportNoAcceptance(t *testing.T) {
	rep := buildVerificationReport("a1", "", "", "", nil, nil, nil, nil)
	if rep.Status != VerifyStatusUnverified {
		t.Fatalf("no acceptance must be unverified, got %s", rep.Status)
	}
	if len(rep.Unresolved) == 0 {
		t.Fatal("unresolved reason missing")
	}
}

// TestVerificationReportTerminalStatus 出口错误校正报告状态。
func TestVerificationReportTerminalStatus(t *testing.T) {
	history := []agent.ReactMessage{
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "go test ./..."}),
		toolResultMsg("c1", true),
	}
	passed := buildVerificationReport("a1", "", "", "", history, []string{"测试通过 [evidence:command]"}, nil, nil)
	if passed.Status != VerifyStatusPassed {
		t.Fatalf("expected passed, got %s", passed.Status)
	}
	unv := buildVerificationReport("a1", "", "", "", history, []string{"测试通过 [evidence:command]"}, nil, errVerifyMissing)
	if unv.Status != VerifyStatusUnverified {
		t.Fatalf("verify_missing must be unverified, got %s", unv.Status)
	}
	failed := buildVerificationReport("a1", "", "", "", history, []string{"测试通过 [evidence:command]"}, nil, errors.New("boom"))
	if failed.Status != VerifyStatusFailed {
		t.Fatalf("generic error must be failed, got %s", failed.Status)
	}
}

// TestAgentResultV2Report AgentResult v2 保留 v1 字段并携带报告与失败归因。
func TestAgentResultV2Report(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	rep := &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusUnverified, Unresolved: []string{"A1 missing"}}
	r := d.buildAgentResultWithReport("p", "p/code-1", "部分完成，未验证", []string{"a.go"}, rep)
	if r.Version != AgentResultVersion || r.Verification != VerifyStatusUnverified {
		t.Fatalf("v2 result mismatch: %+v", r)
	}
	if r.VerificationReport == nil || len(r.Unresolved) != 1 || r.FailureKind != "" {
		t.Fatalf("report fields mismatch: %+v", r)
	}
	repPassed := &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusPassed}
	r2 := d.buildAgentResultWithReport("p", "p/code-1", "完成", nil, repPassed)
	if r2.Verification != VerifyStatusPassed {
		t.Fatalf("report passed must map to verification passed, got %s", r2.Verification)
	}

	summary := "[failure kind=timeout retryable=true]\n子 Agent 超时"
	r3 := d.buildAgentResultWithReport("p", "p/code-1", summary, nil, nil)
	if r3.Status != "failed" || r3.FailureKind != "timeout" || !r3.Retryable || r3.Verification != "unknown" {
		t.Fatalf("failure attribution mismatch: %+v", r3)
	}
}

// TestEnforceVerificationGate 仅绑定任务 + enforce 时阻断未通过报告；legacy 放行。
func TestEnforceVerificationGate(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.WithVerificationPolicyMode("enforce")
	d.verifyReports.Store("s1/code-1", &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusUnverified, Unresolved: []string{"A1 missing"}})
	boundCtx := withTaskBinding(context.Background(), team.TaskBinding{TaskID: "t1", AttemptID: "a1"})
	if msg, blocked := d.enforceVerificationGate(boundCtx, "s1/code-1"); !blocked || !strings.Contains(msg, "A1 missing") {
		t.Fatalf("bound unverified task must be blocked: %q %v", msg, blocked)
	}
	if _, blocked := d.enforceVerificationGate(context.Background(), "s1/code-1"); blocked {
		t.Fatal("legacy path must not be blocked")
	}
	d.verifyReports.Store("s1/code-2", &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusPassed})
	if _, blocked := d.enforceVerificationGate(boundCtx, "s1/code-2"); blocked {
		t.Fatal("passed report must not be blocked")
	}
	d.WithVerificationPolicyMode("shadow")
	if _, blocked := d.enforceVerificationGate(boundCtx, "s1/code-1"); blocked {
		t.Fatal("shadow must not block")
	}
}

// TestEnforceGateMissingReportBlocks 绑定任务 + enforce 缺报告不得默认放行。
func TestEnforceGateMissingReportBlocks(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	d.WithVerificationPolicyMode("enforce")
	boundCtx := withTaskBinding(context.Background(), team.TaskBinding{TaskID: "t1", AttemptID: "a1"})
	// 先清空报告，模拟“门禁先于报告生成”的旧时序。
	d.verifyReports.Delete("s1/code-1")
	if _, blocked := d.enforceVerificationGate(boundCtx, "s1/code-1"); !blocked {
		t.Fatal("bound task without report must be blocked")
	}
	// ensureVerificationReport 先生成报告；无验收条目 → unverified → 仍阻断。
	res := agent.ReactResult{History: []agent.ReactMessage{{Role: "user", Content: "do"}}}
	rep := d.ensureVerificationReport(boundCtx, "s1", "d", "s1/code-1", res)
	if rep == nil || rep.Status != VerifyStatusUnverified {
		t.Fatalf("ensure report mismatch: %+v", rep)
	}
	if msg, blocked := d.enforceVerificationGate(boundCtx, "s1/code-1"); !blocked || !strings.Contains(msg, "未通过") {
		t.Fatalf("unverified report must block: %q %v", msg, blocked)
	}
}

// TestBindingAcceptanceIsAuthoritative 冻结计划验收清单优先于 parentSpec 缓存。
func TestBindingAcceptanceIsAuthoritative(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	ctx := withTaskBinding(context.Background(), team.TaskBinding{
		TaskID: "t1", AttemptID: "a1",
		Acceptance: []string{"功能可用 [evidence:command]"},
	})
	history := []agent.ReactMessage{
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "go test ./..."}),
		toolResultMsg("c1", true),
	}
	rep := d.verificationFor(ctx, "s", "d", "s/code-1", agent.ReactResult{History: history}, nil)
	if len(rep.Items) != 1 || rep.Items[0].Text != "功能可用" {
		t.Fatalf("binding acceptance not used: %+v", rep.Items)
	}
	if rep.Status != VerifyStatusPassed {
		t.Fatalf("expected passed with command evidence, got %s: %v", rep.Status, rep.Unresolved)
	}
}

// TestRefreshVerificationReportAfterPause 恢复后的最终 History 必须覆盖暂停阶段旧报告。
func TestRefreshVerificationReportAfterPause(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	ctx := withTaskBinding(context.Background(), team.TaskBinding{
		TaskID: "t1", AttemptID: "a1", Acceptance: []string{"测试通过 [evidence:command]"},
	})
	agentID := "s1/code-1"
	paused := d.refreshVerificationReport(ctx, "s1", "代码", agentID, agent.ReactResult{}, errPaused)
	if paused.Status != VerifyStatusUnverified {
		t.Fatalf("paused report = %s, want unverified", paused.Status)
	}
	final := agent.ReactResult{History: []agent.ReactMessage{
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "go test ./..."}),
		toolResultMsg("c1", true),
	}}
	resumed := d.refreshVerificationReport(ctx, "s1", "代码", agentID, final, nil)
	if resumed.Status != VerifyStatusPassed || resumed.AttemptID != "a1" {
		t.Fatalf("resume did not rebuild final report: %+v", resumed)
	}
	if got := d.peekVerificationReport(agentID); got != resumed || got.Status != VerifyStatusPassed {
		t.Fatalf("stale paused report survived resume: %+v", got)
	}
}

// TestResumePausedRebuildsReportAndFinalizesAttempt 覆盖真实恢复入口，防止误用暂停阶段旧报告。
func TestResumePausedRebuildsReportAndFinalizesAttempt(t *testing.T) {
	d, _, _, msgStore, tr, _ := newPauseTestEnv(t, &scriptProvider{lines: []string{"恢复完成"}})
	d.WithLoopConfigByRole(func(string) agent.LoopConfig { return agent.LoopConfig{TokenBudget: 1 << 30, MaxIterations: 50} })
	d.WithVerificationPolicyMode("enforce")
	subID := "s1/code_assistant-1"
	tr.Register(orchestrator.Node{ID: subID, ParentID: "s1", Role: "code_assistant", Domain: "代码", Status: orchestrator.StatusPaused})
	history := []agent.ReactMessage{
		{Role: "user", Content: "实现并验证"},
		toolCallMsg("c1", "RunCommand", map[string]any{"command": "go test ./..."}),
		toolResultMsg("c1", true),
	}
	msgStore.saved = []savedMsg{{agentID: subID, sessionID: "s1", msgs: history}}
	binding := team.TaskBinding{SessionID: "s1", PlanVersion: 1, TaskID: "t1", AttemptID: "a1", Acceptance: []string{"测试通过 [evidence:command]"}}
	d.verifyReports.Store(subID, &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusUnverified, Unresolved: []string{"暂停阶段无证据"}})
	attemptStatus := ""
	d.WithAttemptFinisher(func(_ context.Context, attemptID, status, _, _ string) {
		if attemptID == "a1" {
			attemptStatus = status
		}
	})
	ctx := withTaskBinding(dispatchCtx(), binding)
	result, err := d.resumePausedWithOptions(ctx, subID, team.ResumeOptions{Origin: "user_resume", Binding: &binding})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result.LimitReached {
		t.Fatalf("resume unexpectedly paused: %+v", result)
	}
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusDone {
		t.Fatalf("node status = %s, want done; result=%+v", n.Status, result)
	}
	if attemptStatus != team.AttemptDone {
		t.Fatalf("stale paused report blocked finalization: %s", attemptStatus)
	}
}

// TestTakeVerificationReport 报告一次性消费，取后即删。
func TestTakeVerificationReport(t *testing.T) {
	d := NewDispatcher(nil, nil, nil, nil, nil)
	rep := &VerificationReport{Version: VerificationReportVersion, Status: VerifyStatusPassed}
	d.verifyReports.Store("p/code-1", rep)
	if got := d.takeVerificationReport("p/code-1"); got != rep {
		t.Fatalf("report not returned: %+v", got)
	}
	if got := d.takeVerificationReport("p/code-1"); got != nil {
		t.Fatalf("report must be consumed once: %+v", got)
	}
}
