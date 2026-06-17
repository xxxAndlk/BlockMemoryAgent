package mailbox

import (
	"testing"
)

func TestMailbox_DirectSendAndDrain(t *testing.T) {
	m := New()
	m.Send(&Message{From: "agentA", To: "agentB", Type: MsgInfo, Subject: "hello"})
	m.Send(&Message{From: "agentA", To: "agentB", Type: MsgMilestone, Subject: "done", Priority: 1})

	if got := m.Count("agentB"); got != 2 {
		t.Fatalf("expected 2 unread, got %d", got)
	}
	got := m.Drain("agentB")
	if len(got) != 2 {
		t.Fatalf("expected 2 drained, got %d", len(got))
	}
	// 优先级排序：Priority=1 应排在前
	if got[0].Subject != "done" {
		t.Fatalf("priority sort failed: %s", got[0].Subject)
	}
	if m.Count("agentB") != 0 {
		t.Fatalf("after drain, expected 0 unread")
	}
}

func TestMailbox_BroadcastAndForward(t *testing.T) {
	m := New()
	id := m.Send(&Message{From: "meta", To: "*", Type: MsgEscalate, Subject: "needs help"})

	bcasts := m.DrainBroadcast()
	if len(bcasts) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(bcasts))
	}
	if bcasts[0].ID != id {
		t.Fatalf("id mismatch")
	}

	// drained → 已读，再次 drain 应返回 0
	if drained := m.DrainBroadcast(); len(drained) != 0 {
		t.Fatalf("broadcast already read; expected 0, got %d", len(drained))
	}
}

func TestMailbox_Purge(t *testing.T) {
	m := New()
	m.Send(&Message{To: "x", Subject: "1"})
	m.Send(&Message{To: "x", Subject: "2"})
	m.Purge("x")
	if m.Count("x") != 0 {
		t.Fatalf("Purge failed")
	}
}
