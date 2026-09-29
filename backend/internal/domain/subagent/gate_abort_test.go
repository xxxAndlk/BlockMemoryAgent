package subagent

// gate_abort_test.go 验证 settleGateAbort（并发池排队期取消的软停止收口补漏）：
//   - 软停止 + domain：树节点 Paused（可续跑、HasPausedChild 可见）、不 notify 父、
//     返回 true（调用方跳过 trackChildDone，PendingChildren 保持 >0）；
//   - 软停止 + 叶子：树节点终态（不再卡 Running）、父邮箱收"排队中随会话停止被取消"，
//     返回 false（调用方走正常尾部递减计数）；
//   - 非软停止：返回 false 零副作用（cancel_agent/巡检硬取消由取消方收口）。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

func TestSettleGateAbort_SoftStopDomainPauses(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	subID := "s1/domain-1"
	tr.Register(orchestrator.Node{ID: subID, ParentID: "s1", Role: "domain", Status: orchestrator.StatusRunning})
	d.SetSoftStop("s1")
	ctx := tool.WithSessionID(context.Background(), "s1")

	if !d.settleGateAbort(ctx, "s1", subID, "domain") {
		t.Fatal("soft-stop domain gate-abort should return paused=true (skip trackChildDone)")
	}
	node, ok := tr.Get(subID)
	if !ok {
		t.Fatalf("node %s not in tree", subID)
	}
	if node.Status != orchestrator.StatusPaused {
		t.Fatalf("node should be Paused, got status=%v", node.Status)
	}
	// 恢复路由零改动生效：Paused domain 对父终结保护可见。
	if !d.HasPausedChild("s1") {
		t.Fatal("paused domain node should be visible to HasPausedChild")
	}
	// domain 暂停不 notify 父（与 runSubAgent 软停止分支同口径）。
	if msgs := mb.Drain("s1"); len(msgs) != 0 {
		t.Fatalf("domain pause should not notify parent, got %d messages: %s", len(msgs), msgs[0].Body)
	}
}

func TestSettleGateAbort_SoftStopLeafCancelled(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	subID := "s1/code_assistant-1"
	tr.Register(orchestrator.Node{ID: subID, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	d.SetSoftStop("s1")
	ctx := tool.WithSessionID(context.Background(), "s1")

	if d.settleGateAbort(ctx, "s1", subID, "code_assistant") {
		t.Fatal("soft-stop leaf gate-abort should return false (caller proceeds to trackChildDone)")
	}
	node, ok := tr.Get(subID)
	if !ok {
		t.Fatalf("node %s not in tree", subID)
	}
	// 终态不再卡 Running：treeFinish 带 context.Canceled → Failed（err 记取消原因），
	// 与 ResumePaused gate-abort 收口（treeFinish + gateErr）同口径。
	if node.Status != orchestrator.StatusFailed {
		t.Fatalf("node should be terminal Failed, got status=%v", node.Status)
	}
	if !strings.Contains(node.Err, "context canceled") {
		t.Fatalf("node err should record cancellation, got: %q", node.Err)
	}
	// 父邮箱收到"排队中随会话停止被取消，未执行"通知（带失败机读标记，聚合计数不误判完成）。
	found := false
	for _, m := range mb.Drain("s1") {
		if strings.Contains(m.Body, "排队中随会话停止被取消") {
			found = true
			if !strings.HasPrefix(m.Body, "[failure kind=") {
				t.Fatalf("queued-cancelled notice should carry failure marker, got: %s", m.Body)
			}
		}
	}
	if !found {
		t.Fatal("parent mailbox never got queued-cancelled notice")
	}
}

func TestSettleGateAbort_NotSoftStopNoOp(t *testing.T) {
	d, mb, tr, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	subID := "s1/code_assistant-1"
	tr.Register(orchestrator.Node{ID: subID, ParentID: "s1", Role: "code_assistant", Status: orchestrator.StatusRunning})
	ctx := tool.WithSessionID(context.Background(), "s1")

	if d.settleGateAbort(ctx, "s1", subID, "code_assistant") {
		t.Fatal("non-soft-stop gate-abort should return false")
	}
	node, _ := tr.Get(subID)
	if node.Status != orchestrator.StatusRunning {
		t.Fatalf("node should stay Running (cancel-owner settles), got status=%v", node.Status)
	}
	if msgs := mb.Drain("s1"); len(msgs) != 0 {
		t.Fatalf("expected zero side effects, got %d messages", len(msgs))
	}
}
