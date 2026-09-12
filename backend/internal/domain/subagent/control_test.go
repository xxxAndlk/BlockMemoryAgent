package subagent

// control_test.go 验证 TODO #25 MetaAgent 控制面：
//   - cancel_agent 工具：树节点 Cancelled + 父计数兜底 + 父 mailbox 收"已被上级取消"；
//     子 goroutine 经 context.Canceled 路径不重复通知（单通知）；
//   - stall 防误杀版（TODO 第10项② 证据化判定）：后代活动沿 parentID 链冒泡保活；
//     domain 超其阈值（2× 叶子）步间静默才判假死。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/go-kratos/blades"
)

// newEvidenceAt 构造 lastTS=v 的活动证据（测试用；活动种类记为 test）。
func newEvidenceAt(v int64) *activityEvidence {
	e := &activityEvidence{}
	e.lastTS.Store(v)
	e.lastKind.Store("test")
	return e
}

// blockingProvider Generate 阻塞直到 ctx 取消，模拟挂起/长跑的子 Agent。
type blockingProvider struct{}

func (p *blockingProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (p *blockingProvider) Name() string { return "blocking" }

// TestCancelAgentTool_CancelsChild 端到端：派发挂起子 Agent -> cancel_agent 工具取消 ->
// 树节点 Cancelled + 父 mailbox 收"已被上级取消"单条通知（goroutine 不重复 notify）。
func TestCancelAgentTool_CancelsChild(t *testing.T) {
	d, mb, tr, toolsReg, _ := newSalvageTestEnv(t, &blockingProvider{})
	d.RegisterControlTool(toolsReg)

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "长任务",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)

	// 等节点注册进树后取消。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := tr.Get(subID); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelRes, err := toolsReg.Dispatch(dispatchCtx(), "cancel_agent", map[string]any{"agent_id": subID})
	if err != nil || !cancelRes.Success {
		t.Fatalf("cancel_agent failed: err=%v res=%+v", err, cancelRes)
	}

	// 树节点 Cancelled。
	time.Sleep(50 * time.Millisecond)
	node, ok := tr.Get(subID)
	if !ok {
		t.Fatalf("node %s not in tree", subID)
	}
	if node.Status != orchestrator.StatusCancelled {
		t.Fatalf("node should be Cancelled, got status=%v", node.Status)
	}

	// 父 mailbox：收到取消通知，且无重复失败通知。
	var got *mailbox.Message
	dl := time.Now().Add(3 * time.Second)
	for time.Now().Before(dl) {
		msgs := mb.Drain("s1")
		for _, m := range msgs {
			if strings.Contains(m.Body, "已被上级取消") {
				got = m
			}
			if strings.Contains(m.Body, "sub-agent failed") {
				t.Fatalf("cancelled child should not double-notify generic failure, got: %s", m.Body)
			}
		}
		if got != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("parent mailbox never got cancel notification")
	}
	// 注意区分：cancel_agent 显式取消（本用例）retryable=false（上级有意终止）；
	// 心跳假死 kill（killStuckSubAgent）retryable=true（盘上产物可续建）。
	if !strings.HasPrefix(got.Body, "[failure kind=killed retryable=false]") {
		t.Fatalf("cancel notify should carry killed marker, got: %s", got.Body)
	}
}

// TestCancelAgentTool_UnknownAgent 取消不存在/已终止的 agent 返回错误。
func TestCancelAgentTool_UnknownAgent(t *testing.T) {
	d, _, _, toolsReg, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	d.RegisterControlTool(toolsReg)
	res, err := toolsReg.Dispatch(dispatchCtx(), "cancel_agent", map[string]any{"agent_id": "s1/domain-999"})
	if err != nil {
		t.Fatalf("dispatch should not error: %v", err)
	}
	if res.Success {
		t.Fatalf("cancelling unknown agent should fail, got: %+v", res)
	}
	if !strings.Contains(res.Error, "不存在或已终止") {
		t.Fatalf("unexpected error: %s", res.Error)
	}
}

// TestBubbleActivity_RefreshesAncestors 后代活动沿 parentID 链冒泡刷新祖先时间戳。
func TestBubbleActivity_RefreshesAncestors(t *testing.T) {
	d, _, _, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	old := time.Now().Add(-time.Hour).UnixNano()
	now := time.Now().UnixNano()

	d.activity.Store("s1/domain-1", newEvidenceAt(old))
	d.activity.Store("s1/domain-1/code_assistant-1", newEvidenceAt(old))
	d.subMeta.Store("s1/domain-1", &subAgentMeta{parentID: "s1", sessionID: "s1"})
	d.subMeta.Store("s1/domain-1/code_assistant-1", &subAgentMeta{parentID: "s1/domain-1", sessionID: "s1"})

	d.bubbleActivity("s1/domain-1/code_assistant-1", now)

	if v, ok := d.activity.Load("s1/domain-1"); !ok || v.(*activityEvidence).lastTS.Load() != now {
		t.Fatalf("domain ancestor should be refreshed by descendant activity")
	}
}

// TestScanStuck_DomainThreshold domain 超其阈值才判假死；阈值内（含叶子已死但后代
// 冒泡保活期间）不杀。叶子按自身阈值判定。
func TestScanStuck_DomainThreshold(t *testing.T) {
	d, _, _, _, _ := newSalvageTestEnv(t, &mockProvider{text: "ok"})
	d.WithHeartbeatTimeout(50 * time.Millisecond)
	d.WithDomainHeartbeatTimeout(200 * time.Millisecond)

	now := time.Now().UnixNano()
	noopCancel := func() {}
	// 叶子静默 100ms（>50ms 阈值）→ 被杀。
	leafID := "s1/code_assistant-1"
	d.activity.Store(leafID, newEvidenceAt(now-100*time.Millisecond.Nanoseconds()))
	d.subMeta.Store(leafID, &subAgentMeta{parentID: "s1", sessionID: "s1", cancel: noopCancel})
	// domain 静默 100ms（<200ms 阈值）→ 存活；静默 300ms（>阈值）→ 被杀。
	aliveDomain := "s1/domain-1"
	deadDomain := "s1/domain-2"
	d.activity.Store(aliveDomain, newEvidenceAt(now-100*time.Millisecond.Nanoseconds()))
	d.activity.Store(deadDomain, newEvidenceAt(now-300*time.Millisecond.Nanoseconds()))
	d.subMeta.Store(aliveDomain, &subAgentMeta{parentID: "s1", sessionID: "s1", cancel: noopCancel})
	d.subMeta.Store(deadDomain, &subAgentMeta{parentID: "s1", sessionID: "s1", cancel: noopCancel})

	d.scanStuck()

	if _, ok := d.activity.Load(leafID); ok {
		t.Fatal("stale leaf should be killed")
	}
	if _, ok := d.activity.Load(aliveDomain); !ok {
		t.Fatal("domain within its threshold should survive")
	}
	if _, ok := d.activity.Load(deadDomain); ok {
		t.Fatal("domain beyond its threshold should be killed")
	}
}
