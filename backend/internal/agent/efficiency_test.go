package agent

// efficiency_test.go 验证效率一等指标查询（TODO 第9⑥/第10③）与 PauseAgent 校验路径：
//   - efficiencyQueryResult：树节点 → 支路表（轮次口径 = 工具轮+1、非-meta 计入派发、
//     任务截断、墙钟）；pgStore 缺席时 token 类指标归零不报错（纯聚合降级）。
//   - agentEventsQueryResult：pgStore 缺席或空 agentID 降级空列表（审计面不报错）。
//   - isVerifyRole：review/verify/judge/audit 子串分类。
//   - PauseAgent：会话不存在 / 节点不存在 / 非 domain / 非 Running 四类拒绝
//     （happy path 的 dispatcher 收尾分流在 domain/subagent/dispatcher_pause_test.go 覆盖）。

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestEfficiencyQueryResult_BranchesFromTree 验证支路表组装与轮次/派发口径：
// pgStore 缺席（测试环境）时 token 类指标归零、无事件统计，branches 全部来自权威树。
func TestEfficiencyQueryResult_BranchesFromTree(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("效率指标测试", "")
	ctx := context.Background()

	tr := svc.TreeFor(sess.ID)
	start := time.Now().Add(-time.Minute)
	finish := time.Now()
	// 非-meta 派发 ×2（domain + 校验角色）。
	tr.Register(orchestrator.Node{ID: sess.ID + "/domain-1", ParentID: sess.ID, Role: "domain", Domain: "配置",
		Task: "实现 config.js 渲染接口并完成自检", Status: orchestrator.StatusDone, Started: start, Finished: finish})
	tr.Register(orchestrator.Node{ID: sess.ID + "/review-1", ParentID: sess.ID, Role: "code_review",
		Task: "评审交付", Status: orchestrator.StatusDone, Started: start, Finished: finish})
	// meta 节点不计入派发数。
	tr.Register(orchestrator.Node{ID: sess.ID + "/meta-1", ParentID: "", Role: "meta",
		Task: "编排", Status: orchestrator.StatusDone, Started: start, Finished: finish})

	res := svc.efficiencyQueryResult(ctx, sess.ID)
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data should be map[string]any, got %T", res.Data)
	}
	branches, ok := data["branches"].([]efficiencyBranchWire)
	if !ok || len(branches) != 3 {
		t.Fatalf("want 3 branches, got %+v", data["branches"])
	}
	byID := map[string]efficiencyBranchWire{}
	for _, b := range branches {
		byID[b.NodeID] = b
	}
	domain := byID[sess.ID+"/domain-1"]
	if domain.Rounds != 1 || domain.ToolCalls != 0 {
		t.Errorf("domain rounds/toolCalls = %d/%d, want 1/0 (轮次=工具轮+终答)", domain.Rounds, domain.ToolCalls)
	}
	if domain.WallClockSec <= 0 {
		t.Errorf("domain wall clock should be positive, got %v", domain.WallClockSec)
	}
	// 派发数 = 非-meta 支路数（domain + review），meta 排除 → 平均轮次 1.0。
	if got := data["avg_rounds_per_dispatch"].(float64); got != 1.0 {
		t.Errorf("avg_rounds_per_dispatch = %v, want 1.0", got)
	}
	// pgStore 缺席：token/P50 指标归零。
	if got := data["total_input_tokens"].(int64); got != 0 {
		t.Errorf("total_input_tokens should be 0 without pgStore, got %v", got)
	}
	if got := data["input_p95"].(float64); got != 0 {
		t.Errorf("input_p95 should be 0 without pgStore, got %v", got)
	}
}

// TestEfficiencyQueryResult_LongTaskTruncated 验证支路任务描述截断（80 runes + 省略号）。
func TestEfficiencyQueryResult_LongTaskTruncated(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("任务截断测试", "")
	long := make([]rune, 200)
	for i := range long {
		long[i] = '测'
	}
	svc.TreeFor(sess.ID).Register(orchestrator.Node{
		ID: sess.ID + "/domain-1", ParentID: sess.ID, Role: "domain", Domain: "配置",
		Task: string(long), Status: orchestrator.StatusRunning, Started: time.Now(),
	})
	data := svc.efficiencyQueryResult(context.Background(), sess.ID).Data.(map[string]any)
	b := data["branches"].([]efficiencyBranchWire)[0]
	if got := len([]rune(b.Task)); got != 81 { // 80 runes + "…"
		t.Errorf("task should be truncated to 81 runes, got %d", got)
	}
	if b.Task[80*3:] != "…" {
		t.Errorf("truncated task should end with ellipsis, got %q", b.Task[80*3:])
	}
}

// TestAgentEventsQueryResult_Degrades 验证审计下钻降级：pgStore 缺席或 agentID 为空
// 都返回空事件列表而非报错。
func TestAgentEventsQueryResult_Degrades(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("审计降级测试", "")
	ctx := context.Background()

	res := svc.agentEventsQueryResult(ctx, sess.ID, sess.ID+"/domain-1", 0, 0)
	data := res.Data.(map[string]any)
	if evs := data["events"].([]map[string]any); len(evs) != 0 {
		t.Errorf("events should degrade to empty without pgStore, got %d", len(evs))
	}
	// 空 agentID：同样空列表。
	res = svc.agentEventsQueryResult(ctx, sess.ID, "", 0, 0)
	data = res.Data.(map[string]any)
	if evs := data["events"].([]map[string]any); len(evs) != 0 {
		t.Errorf("empty agentID should yield empty events, got %d", len(evs))
	}
}

// TestIsVerifyRole 校验开销占比的角色分类口径。
func TestIsVerifyRole(t *testing.T) {
	for _, role := range []string{"code_review", "reviewer", "verifier", "judge", "audit_bot", "SelfReview"} {
		if !isVerifyRole(role) {
			t.Errorf("isVerifyRole(%q) should be true", role)
		}
	}
	for _, role := range []string{"code_assistant", "domain", "meta", "writer"} {
		if isVerifyRole(role) {
			t.Errorf("isVerifyRole(%q) should be false", role)
		}
	}
}

// TestPauseAgent_Rejections 验证手动暂停的入参校验：会话不存在 / 节点不存在 /
// 非 domain 角色 / 非 Running 状态均拒绝且不触发任何标记。
func TestPauseAgent_Rejections(t *testing.T) {
	svc := newReactServiceForTest(nil, "")
	sess := svc.store.createSession("暂停校验测试", "")
	ctx := context.Background()

	// 会话不存在。
	if err := svc.PauseAgent(ctx, "session-none", "x"); err == nil {
		t.Fatal("unknown session must be rejected")
	}
	tr := svc.TreeFor(sess.ID)
	// 节点不存在。
	if err := svc.PauseAgent(ctx, sess.ID, sess.ID+"/domain-none"); err == nil {
		t.Fatal("unknown node must be rejected")
	}
	// 非 domain 角色（叶子用 cancel）。
	tr.Register(orchestrator.Node{ID: sess.ID + "/leaf-1", ParentID: sess.ID, Role: "code_assistant", Status: orchestrator.StatusRunning})
	if err := svc.PauseAgent(ctx, sess.ID, sess.ID+"/leaf-1"); err == nil {
		t.Fatal("non-domain node must be rejected")
	}
	// 非 Running 状态（已 Done）。
	tr.Register(orchestrator.Node{ID: sess.ID + "/domain-9", ParentID: sess.ID, Role: "domain", Status: orchestrator.StatusDone})
	if err := svc.PauseAgent(ctx, sess.ID, sess.ID+"/domain-9"); err == nil {
		t.Fatal("non-running domain must be rejected")
	}
}
