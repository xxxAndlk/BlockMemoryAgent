package subagent

// plan_confirm_test.go 计划确认机制（plan_confirm.go）的单元测试：
// 覆盖下级邮箱审批链路（批准/驳回达上限/超时 fail-open）、开关直通、
// 顶层 ask_user 通路与 review_plan 的校验拒绝分支。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// newPlanConfirmEnv 构造计划确认测试环境：复用 salvage 测试环境的 Dispatcher 接线，
// 注册 submit_plan/review_plan 工具（开关由各用例自行 WithPlanConfirmation 注入）。
func newPlanConfirmEnv(t *testing.T) (*Dispatcher, *tool.Registry, *mailbox.Mailbox) {
	t.Helper()
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, &mockProvider{text: "done"})
	d.RegisterPlanTools(toolsReg)
	return d, toolsReg, mb
}

// extractPlanID 从【计划审批请求】消息 Body 首行提取 plan_id。
func extractPlanID(t *testing.T, body string) string {
	t.Helper()
	marker := "【计划审批请求】plan_id="
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("plan request body missing plan_id marker: %q", body)
	}
	rest := body[idx+len(marker):]
	if end := strings.IndexAny(rest, "\n "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestSubmitPlanDisabled 开关未启用时 submit_plan 直通不阻塞，零行为变化。
func TestSubmitPlanDisabled(t *testing.T) {
	_, toolsReg, _ := newPlanConfirmEnv(t) // 未调 WithPlanConfirmation：默认关闭
	ctx := agent.WithAgentID(context.Background(), "session-1/domain-2")
	res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "s", "plan": "p"})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "未启用") {
		t.Fatalf("expected passthrough when disabled, got success=%v output=%s", res.Success, res.Output)
	}
}

// TestSubmitPlanToParentApproved 下级提交计划 → 父邮箱收到审批请求 → review_plan approve →
// submit_plan 返回已批准。
func TestSubmitPlanToParentApproved(t *testing.T) {
	d, toolsReg, mb := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 5*time.Second, 3)

	type submitOutcome struct {
		output string
		err    error
	}
	done := make(chan submitOutcome, 1)
	go func() {
		ctx := agent.WithAgentID(context.Background(), "session-1/domain-2")
		res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "改造游戏代码", "plan": "1. 读 config.js\n2. 改 game.js\n3. node --check"})
		done <- submitOutcome{res.Output, err}
	}()

	// 轮询父邮箱等审批请求到达（等待者注册先于 Send，取到消息即保证 waiter 在册）。
	var body, planID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range mb.Peek("session-1") {
			if strings.Contains(m.Body, "【计划审批请求】") {
				body = m.Body
				planID = extractPlanID(t, m.Body)
			}
		}
		if planID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if planID == "" {
		t.Fatal("plan request not delivered to parent mailbox in time")
	}
	if !strings.Contains(body, "提交者: session-1/domain-2") || !strings.Contains(body, "任务简介: 改造游戏代码") {
		t.Fatalf("plan request body missing submitter/summary: %s", body)
	}

	// 上级批准。
	rctx := agent.WithAgentID(context.Background(), "session-1")
	res, err := toolsReg.Dispatch(rctx, "review_plan", map[string]any{"plan_id": planID, "verdict": "approve"})
	if err != nil {
		t.Fatalf("review dispatch failed: %v", err)
	}
	if !res.Success {
		t.Fatalf("review_plan failed: %s", res.Error)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("submit_plan failed: %v", out.err)
		}
		if !strings.Contains(out.output, "已批准") {
			t.Fatalf("expected approved output, got: %s", out.output)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("submit_plan did not return after approval")
	}
}

// TestSubmitPlanRejectedLoopUntilApproved 无上限时驳回-修订-重提循环直到批准（不放行未批准执行）。
func TestSubmitPlanRejectedLoopUntilApproved(t *testing.T) {
	d, toolsReg, mb := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 5*time.Second, 0) // 0=不限制，循环直到批准

	submit := func(out chan<- string) {
		ctx := agent.WithAgentID(context.Background(), "session-1/domain-3")
		res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "s", "plan": "p"})
		if err != nil {
			t.Fatalf("submit dispatch failed: %v", err)
		}
		out <- res.Output
	}
	review := func(planID, verdict, feedback string) {
		rctx := agent.WithAgentID(context.Background(), "session-1")
		args := map[string]any{"plan_id": planID, "verdict": verdict}
		if feedback != "" {
			args["feedback"] = feedback
		}
		res, err := toolsReg.Dispatch(rctx, "review_plan", args)
		if err != nil || !res.Success {
			t.Fatalf("review_plan(%s) failed: err=%v out=%+v", verdict, err, res)
		}
	}
	// waitPlanID 轮询父邮箱取未见过的新审批请求（Peek 返回全部历史消息，需按 planID 去重）。
	seen := map[string]bool{}
	waitPlanID := func() string {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			for _, m := range mb.Peek("session-1") {
				if strings.Contains(m.Body, "【计划审批请求】") {
					id := extractPlanID(t, m.Body)
					if !seen[id] {
						seen[id] = true
						return id
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("plan request not delivered in time")
		return ""
	}

	// 第一轮：驳回 → 返回"修订重提"（无上限时不含次数提示）。
	out1 := make(chan string, 1)
	go submit(out1)
	review(waitPlanID(), "reject", "第一步粒度太粗")
	got1 := <-out1
	if !strings.Contains(got1, "被驳回") || !strings.Contains(got1, "粒度太粗") || !strings.Contains(got1, "直到批准") {
		t.Fatalf("first rejection output unexpected: %s", got1)
	}
	if strings.Contains(got1, "还可提交") {
		t.Fatalf("unlimited mode should not show remaining count: %s", got1)
	}

	// 第二轮：再次驳回 → 仍要求修订重提。
	out2 := make(chan string, 1)
	go submit(out2)
	review(waitPlanID(), "reject", "仍然没有覆盖验收标准")
	got2 := <-out2
	if !strings.Contains(got2, "被驳回") || !strings.Contains(got2, "直到批准") {
		t.Fatalf("second rejection output unexpected: %s", got2)
	}

	// 第三轮：批准 → 放行。
	out3 := make(chan string, 1)
	go submit(out3)
	review(waitPlanID(), "approve", "")
	got3 := <-out3
	if !strings.Contains(got3, "已批准") {
		t.Fatalf("expected approved after revision loop, got: %s", got3)
	}
}

// TestSubmitPlanRejectedCapEscalates 配置上限时达上限不放行，转升级仲裁（未经批准禁止执行）。
func TestSubmitPlanRejectedCapEscalates(t *testing.T) {
	d, toolsReg, mb := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 5*time.Second, 2) // 上限 2 次，第 2 次驳回即达上限

	submit := func(out chan<- string) {
		ctx := agent.WithAgentID(context.Background(), "session-1/domain-3")
		res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "s", "plan": "p"})
		if err != nil {
			t.Fatalf("submit dispatch failed: %v", err)
		}
		out <- res.Output
	}
	reject := func(planID, feedback string) {
		rctx := agent.WithAgentID(context.Background(), "session-1")
		res, err := toolsReg.Dispatch(rctx, "review_plan", map[string]any{"plan_id": planID, "verdict": "reject", "feedback": feedback})
		if err != nil || !res.Success {
			t.Fatalf("review_plan(reject) failed: err=%v out=%+v", err, res)
		}
	}
	seen := map[string]bool{}
	waitPlanID := func() string {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			for _, m := range mb.Peek("session-1") {
				if strings.Contains(m.Body, "【计划审批请求】") {
					id := extractPlanID(t, m.Body)
					if !seen[id] {
						seen[id] = true
						return id
					}
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("plan request not delivered in time")
		return ""
	}

	// 第一次提交 → 驳回 → 返回"修订重提"（含剩余次数）。
	out1 := make(chan string, 1)
	go submit(out1)
	reject(waitPlanID(), "第一步粒度太粗")
	got1 := <-out1
	if !strings.Contains(got1, "被驳回") || !strings.Contains(got1, "还可提交 1 次") {
		t.Fatalf("first rejection output unexpected: %s", got1)
	}

	// 第二次提交 → 驳回 → 达上限：不放行，转 escalate 仲裁。
	out2 := make(chan string, 1)
	go submit(out2)
	reject(waitPlanID(), "仍然没有覆盖验收标准")
	got2 := <-out2
	if !strings.Contains(got2, "驳回达上限") || !strings.Contains(got2, "禁止执行") || !strings.Contains(got2, "escalate") {
		t.Fatalf("max-rejection output unexpected: %s", got2)
	}
	if strings.Contains(got2, "best-effort") || strings.Contains(got2, "直接执行") {
		t.Fatalf("capped mode must not authorize execution: %s", got2)
	}
	// 等待者已清理。
	for _, m := range mb.Peek("session-1") {
		if id := extractPlanID(t, m.Body); !seen[id] {
			continue
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pending := false
		for id := range seen {
			if _, ok := d.planState.get(id); ok {
				pending = true
			}
		}
		if !pending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSubmitPlanTimeoutFailOpen 审批等待超时 fail-open：按计划继续。
func TestSubmitPlanTimeoutFailOpen(t *testing.T) {
	d, toolsReg, _ := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 80*time.Millisecond, 3)
	ctx := agent.WithAgentID(context.Background(), "session-1/domain-9")
	res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "s", "plan": "p"})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "超时") {
		t.Fatalf("expected fail-open timeout output, got success=%v output=%s", res.Success, res.Output)
	}
}

// TestSubmitPlanTopLevelUsesAskUser 顶层（agentID 无 "/"）走 ask_user 通路：假 hook
// 返回"批准开工"→ 计划批准；返回修改意见 → 计划被驳回且意见回传。
func TestSubmitPlanTopLevelUsesAskUser(t *testing.T) {
	d, toolsReg, _ := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 5*time.Second, 3)
	toolsReg.SetAskUserHook(func(ctx context.Context, question string, opts tool.AskUserOptions) (string, error) {
		if !strings.Contains(question, "【计划确认】") || !strings.Contains(question, "任务简介") {
			t.Errorf("unexpected ask_user question: %s", question)
		}
		if len(opts.Options) != 2 {
			t.Errorf("expected 2 options, got %d", len(opts.Options))
		}
		return "用户答复: 批准开工", nil
	})
	ctx := agent.WithAgentID(context.Background(), "session-1")
	res, err := toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "多领域改造", "plan": "p"})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "已批准") {
		t.Fatalf("expected approved via ask_user, got: %s", res.Output)
	}

	// 驳回路径：自由文本答复视为修改意见。
	toolsReg.SetAskUserHook(func(ctx context.Context, question string, opts tool.AskUserOptions) (string, error) {
		return "用户答复: 形象必须统一，全部重画", nil
	})
	res, err = toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "多领域改造", "plan": "p"})
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}
	if !res.Success || !strings.Contains(res.Output, "被驳回") || !strings.Contains(res.Output, "形象必须统一") {
		t.Fatalf("expected rejected with feedback, got: %s", res.Output)
	}
}

// TestReviewPlanValidations review_plan 的校验拒绝分支：非法 verdict、驳回缺 feedback、
// 未知 plan_id、非上级调用。
func TestReviewPlanValidations(t *testing.T) {
	d, toolsReg, mb := newPlanConfirmEnv(t)
	d.WithPlanConfirmation(true, 5*time.Second, 3)

	rctx := agent.WithAgentID(context.Background(), "session-1")
	// 非法 verdict。
	res, err := toolsReg.Dispatch(rctx, "review_plan", map[string]any{"plan_id": "x", "verdict": "maybe"})
	if err != nil || res.Success {
		t.Fatalf("expected validation rejection for bad verdict, got err=%v res=%+v", err, res)
	}
	// 未知 plan_id。
	res, err = toolsReg.Dispatch(rctx, "review_plan", map[string]any{"plan_id": "plan-none-1", "verdict": "approve"})
	if err != nil || res.Success {
		t.Fatalf("expected validation rejection for unknown plan_id, got err=%v res=%+v", err, res)
	}
	// 驳回缺 feedback：先提交一份计划再审批。
	go func() {
		ctx := agent.WithAgentID(context.Background(), "session-1/domain-4")
		_, _ = toolsReg.Dispatch(ctx, "submit_plan", map[string]any{"task_summary": "s", "plan": "p"})
	}()
	var planID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && planID == "" {
		for _, m := range mb.Peek("session-1") {
			if strings.Contains(m.Body, "【计划审批请求】") {
				planID = extractPlanID(t, m.Body)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	res, err = toolsReg.Dispatch(rctx, "review_plan", map[string]any{"plan_id": planID, "verdict": "reject"})
	if err != nil || res.Success {
		t.Fatalf("expected validation rejection for reject without feedback, got err=%v res=%+v", err, res)
	}
	// 非上级调用（叶子同级/无关 agent）。
	sctx := agent.WithAgentID(context.Background(), "session-1/domain-5")
	res, err = toolsReg.Dispatch(sctx, "review_plan", map[string]any{"plan_id": planID, "verdict": "approve"})
	if err != nil || res.Success {
		t.Fatalf("expected non-parent rejection, got err=%v res=%+v", err, res)
	}
}
