package subagent

// purge_test.go 覆盖会话硬删除的运行时清扫：
//   - Dispatcher.PurgeSession：会话/节点键控状态表（软停/暂停标记/分发计数/挂起/技能/聚合/
//     计划确认/邮箱/台账）全部清空，且不误伤其他会话；
//   - TaskLedger.Purge 与 planConfirmState.purgeSession 的会话隔离语义。

import (
	"sync/atomic"
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// TestDispatcherPurgeSession_SweepsSessionState 清扫本会话全部运行时状态，其他会话不受影响。
func TestDispatcherPurgeSession_SweepsSessionState(t *testing.T) {
	d, _, mb, _, _, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "x"})

	// 布置本会话状态（s1）与其他会话状态（s9，须保留）。
	d.SetSoftStop("s1")
	d.SetSoftStop("s9")
	d.MarkPauseNode("s1/domain-1")
	d.MarkPauseNode("s9/domain-9")
	d.sessionCounts.Store("s1", &atomic.Int64{})
	d.sessionCounts.Store("s9", &atomic.Int64{})
	d.suspendStates.Store("s1", &sessionSuspendState{})
	d.heldSkills.Store("s1/domain-1", []string{"skill-a"})
	d.aggByAgent.Store("s1/domain-1", &mapAggEntry{})
	d.ledger.bySess["s1"] = []*ledgerEntry{{Seq: 1, ChildID: "s1/domain-1"}}
	d.ledger.seq["s1"] = 1
	d.ledger.seeded["s1"] = true
	planID, _ := d.planState.register("s1/domain-1", "s1")
	otherPlanID, _ := d.planState.register("s9/domain-9", "s9")
	d.planState.revisions["s1/domain-1"] = 2
	// 邮箱：本会话节点收件箱有待读消息（Purge 后不可达）。
	if _, err := mb.Send(&mailbox.Message{To: "s1/domain-1", Subject: "ping"}); err != nil {
		t.Fatalf("seed mailbox: %v", err)
	}

	d.PurgeSession("s1", []string{"s1/domain-1"})

	if d.isSoftStop("s1") {
		t.Errorf("softStops[s1] not purged")
	}
	if !d.isSoftStop("s9") {
		t.Errorf("softStops[s9] must remain (other session)")
	}
	d.pauseRequestMu.Lock()
	_, paused := d.pauseRequests["s1/domain-1"]
	_, otherPaused := d.pauseRequests["s9/domain-9"]
	d.pauseRequestMu.Unlock()
	if paused {
		t.Errorf("pauseRequests node not purged")
	}
	if !otherPaused {
		t.Errorf("pauseRequests other session node must remain")
	}
	if _, loaded := d.sessionCounts.Load("s1"); loaded {
		t.Errorf("sessionCounts[s1] not purged")
	}
	if _, loaded := d.sessionCounts.Load("s9"); !loaded {
		t.Errorf("sessionCounts[s9] must remain")
	}
	if _, loaded := d.suspendStates.Load("s1"); loaded {
		t.Errorf("suspendStates[s1] not purged")
	}
	if _, loaded := d.heldSkills.Load("s1/domain-1"); loaded {
		t.Errorf("heldSkills node not purged")
	}
	if _, loaded := d.aggByAgent.Load("s1/domain-1"); loaded {
		t.Errorf("aggByAgent node not purged")
	}
	d.ledger.mu.Lock()
	_, ledgerLeft := d.ledger.bySess["s1"]
	_, seededLeft := d.ledger.seeded["s1"]
	d.ledger.mu.Unlock()
	if ledgerLeft || seededLeft {
		t.Errorf("ledger state not purged: bySess=%v seeded=%v", ledgerLeft, seededLeft)
	}
	if _, ok := d.planState.get(planID); ok {
		t.Errorf("plan waiter of s1 not purged")
	}
	if _, ok := d.planState.get(otherPlanID); !ok {
		t.Errorf("plan waiter of s9 must remain")
	}
	d.planState.mu.Lock()
	_, revLeft := d.planState.revisions["s1/domain-1"]
	d.planState.mu.Unlock()
	if revLeft {
		t.Errorf("revisions of s1 not purged")
	}
	if mb.Count("s1/domain-1") != 0 {
		t.Errorf("mailbox for node not purged, count=%d", mb.Count("s1/domain-1"))
	}
}
