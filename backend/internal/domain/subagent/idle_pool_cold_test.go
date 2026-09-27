package subagent

// idle_pool_cold_test.go 验证 TTL 冷驻 + reuse 冷恢复语义（TTL 到期不再置 Done）：
//   - TTL 到期销毁后树节点保持 Idle（可复活注册表条目），IdleRoster 列出 Cold=true 条目；
//   - reuse_agent_id 命中冷驻节点：restoreColdSlot 从 agent_messages 重建 dormant 槽，
//     唤醒序列惰性启动 supervisor 续跑（history 种子含旧任务上下文）；
//   - 无 history 的冷节点恢复失败，回落"请新建"错误。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestHotDomain_TTLColdResidentRoster 验证 TTL 到期冷驻：槽销毁、树节点保 Idle、
// IdleRoster 由热驻条目转为 Cold=true 条目、SlotAlive 由 true 翻 false。
func TestHotDomain_TTLColdResidentRoster(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, 300*time.Millisecond)
	d.WithMessagesStore(&captureMessagesStore{})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// 热驻期：roster 列热驻条目（Cold=false），SlotAlive=true。
	roster := d.IdleRoster("s1")
	if len(roster) != 1 || roster[0].Cold {
		t.Fatalf("hot roster = %+v, want 1 hot entry (Cold=false)", roster)
	}
	if !d.SlotAlive("s1", subID) {
		t.Error("SlotAlive should be true for hot slot")
	}

	// TTL 到期：槽销毁（冷驻）——树节点保 Idle，roster 转为 Cold=true，SlotAlive=false。
	waitForCond(t, "slot destroyed after TTL", func() bool {
		return d.pool.slot("s1", subID) == nil
	})
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusIdle {
		t.Fatalf("tree status after TTL = %v, want Idle (cold-resident, revivable)", n.Status)
	}
	waitForCond(t, "roster cold entry", func() bool {
		roster := d.IdleRoster("s1")
		return len(roster) == 1 && roster[0].Cold && roster[0].AgentID == subID
	})
	if d.SlotAlive("s1", subID) {
		t.Error("SlotAlive should be false after TTL destroy (cold-resident)")
	}
}

// TestHotDomain_ColdRestoreReuse 验证冷恢复：TTL 销毁后 reuse_agent_id 命中冷驻节点，
// restoreColdSlot 从 agent_messages 重建 dormant 槽并落入既有 idle 唤醒序列
//（惰性启动 supervisor），任务带 history 种子续跑完成。
func TestHotDomain_ColdRestoreReuse(t *testing.T) {
	provider := &scriptProvider{lines: []string{"first result", "second result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, 300*time.Millisecond)
	d.WithMessagesStore(&captureMessagesStore{})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "第一任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})

	// TTL 到期进入冷驻（槽销毁、节点 Idle）。
	waitForCond(t, "slot destroyed after TTL", func() bool {
		return d.pool.slot("s1", subID) == nil
	})

	// reuse_agent_id 命中冷驻节点：冷恢复重建槽并成功派发。
	res2, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"reuse_agent_id": subID,
		"task":           "第二任务",
	})
	if err != nil || !res2.Success {
		t.Fatalf("cold restore reuse failed: err=%v res=%+v", err, res2)
	}

	// 续跑任务执行完成回 idle（dormant 槽惰性启动的 supervisor 跑完第二任务）。
	waitForCond(t, "tree idle after cold restore task", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle && d.PendingChildren("s1") == 0
	})
	s := d.pool.slot("s1", subID)
	if s == nil {
		t.Fatal("slot missing after cold restore wake")
	}
	s.mu.Lock()
	rc, dormant := s.reuseCount, s.dormant
	foundSeed := false
	for _, m := range s.history {
		if m.Role == "user" && strings.Contains(m.Content, "第一任务") {
			foundSeed = true
		}
	}
	s.mu.Unlock()
	if rc != 1 {
		t.Errorf("reuseCount = %d, want 1", rc)
	}
	if dormant {
		t.Error("slot should not stay dormant after wake (supervisor must be started)")
	}
	if !foundSeed {
		t.Error("cold-restored run should carry history seeded from agent_messages (含第一任务)")
	}
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls < 2 {
		t.Errorf("provider calls = %d, want >= 2 (cold-restored task executed)", calls)
	}
}

// TestHotDomain_ColdRestoreNoHistoryRejected 验证无 history 的冷节点恢复失败，
// 回落原"请新建"错误且不建空壳槽。
func TestHotDomain_ColdRestoreNoHistoryRejected(t *testing.T) {
	provider := &scriptProvider{lines: []string{"result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	d.WithMessagesStore(&captureMessagesStore{})

	// 冷驻节点：树 Idle 且池内无槽、无 history（模拟落库数据不够）。
	tr.Register(orchestrator.Node{ID: "s1/domain-7", ParentID: "s1", Role: "domain", Domain: "金融", Status: orchestrator.StatusIdle})

	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"reuse_agent_id": "s1/domain-7",
		"task":           "任务",
	})
	if err != nil || res.Success {
		t.Fatalf("expected rejection for history-less cold node, got err=%v res=%+v", err, res)
	}
	if res.Category != tool.ResultCategoryValidationRejected {
		t.Errorf("category = %s, want validation_rejected", res.Category)
	}
	if !strings.Contains(res.Error, "请用 role_id=domain 新建") {
		t.Errorf("error = %q, want 回落新建提示", res.Error)
	}
	if d.pool.slot("s1", "s1/domain-7") != nil {
		t.Error("无 history 不应建空壳槽")
	}
}
