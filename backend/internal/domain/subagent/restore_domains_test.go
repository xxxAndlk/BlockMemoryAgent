package subagent

// restore_domains_test.go 会话恢复中间层（TODO #21④）：进程重启后 resume 会话时
// 按树节点+agent_messages 重建热驻槽——未终态/终态不久实例回温，无 history 不建空壳，
// 已在池不重建。

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
)

// TestRestoreSessionDomains_RebuildsFromMessages 验证恢复中间层核心语义：
//   - 未终态（Paused/Idle）+ 有 history 的 domain 节点 → 重建 Idle 槽（history 带入）；
//   - 无 history 节点 → 跳过（不建空壳）；
//   - 已在池的槽 → 不重复重建；
//   - 终态过久（>30min）节点 → 跳过（冷复活归 #17）。
func TestRestoreSessionDomains_RebuildsFromMessages(t *testing.T) {
	provider := &scriptProvider{lines: []string{"x"}}
	d, _, tr, _ := newIdleTestEnv(t, provider, time.Hour)
	msgs := &captureMessagesStore{}
	msgs.saved = []savedMsg{
		{agentID: "s1/domain-1", sessionID: "s1", msgs: []agent.ReactMessage{
			{Role: "user", Content: "任务A"},
			{Role: "assistant", Content: "结论A"},
		}},
		// domain-2：无 history（不建槽）。
		// domain-3：终态过久（不建槽）。
	}
	d.WithMessagesStore(msgs)

	tr.Register(orchestrator.Node{ID: "s1/domain-1", ParentID: "s1", Role: "domain", Domain: "金融", Status: orchestrator.StatusPaused})
	tr.Register(orchestrator.Node{ID: "s1/domain-2", ParentID: "s1", Role: "domain", Domain: "物流", Status: orchestrator.StatusIdle})
	tr.Register(orchestrator.Node{ID: "s1/domain-3", ParentID: "s1", Role: "domain", Domain: "旧域", Status: orchestrator.StatusDone, Finished: time.Now().Add(-2 * time.Hour)})
	tr.Register(orchestrator.Node{ID: "s1/leaf-1", ParentID: "s1/domain-1", Role: "code_assistant", Status: orchestrator.StatusPaused})

	n := d.RestoreSessionDomains("s1")
	if n != 1 {
		t.Fatalf("restored = %d, want 1 (仅 domain-1)", n)
	}
	s := d.pool.slot("s1", "s1/domain-1")
	if s == nil {
		t.Fatal("domain-1 slot must be rebuilt")
	}
	s.mu.Lock()
	state, hist := s.state, len(s.history)
	s.mu.Unlock()
	if state != slotIdle {
		t.Errorf("rebuilt slot state = %v, want slotIdle", state)
	}
	if hist != 2 {
		t.Errorf("rebuilt slot history = %d msgs, want 2", hist)
	}
	if d.pool.slot("s1", "s1/domain-2") != nil {
		t.Error("domain-2 has no persisted messages: must not build empty slot")
	}
	if d.pool.slot("s1", "s1/domain-3") != nil {
		t.Error("domain-3 terminal >30min: must not rebuild (cold revive is #17)")
	}

	// 已在池不重复重建（幂等）。
	if got := d.RestoreSessionDomains("s1"); got != 0 {
		t.Errorf("second restore = %d, want 0 (slot already in pool)", got)
	}
}

// TestRestoreSessionDomains_TerminalRecentAndResumeHook 验证终态不久实例可回温 +
// ResumeSessionAgents 头部自动触发恢复（零特判接入唤醒广播）。
func TestRestoreSessionDomains_TerminalRecentAndResumeHook(t *testing.T) {
	provider := &scriptProvider{lines: []string{"x"}}
	d, _, tr, _ := newIdleTestEnv(t, provider, time.Hour)
	msgs := &captureMessagesStore{}
	msgs.saved = []savedMsg{
		{agentID: "s1/domain-9", sessionID: "s1", msgs: []agent.ReactMessage{
			{Role: "user", Content: "任务B"},
		}},
	}
	d.WithMessagesStore(msgs)
	tr.Register(orchestrator.Node{ID: "s1/domain-9", ParentID: "s1", Role: "domain", Domain: "支付", Status: orchestrator.StatusFailed, Finished: time.Now().Add(-5 * time.Minute)})

	// 走 ResumeSessionAgents（含恢复中间层前置）。
	d.ResumeSessionAgents("s1")

	if d.pool.slot("s1", "s1/domain-9") == nil {
		t.Fatal("recently-terminal domain must be rebuilt via ResumeSessionAgents")
	}
}
