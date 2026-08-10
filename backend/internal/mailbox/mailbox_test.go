package mailbox

import (
	"testing" // Go 测试框架
)

// TestMailbox_DirectSendAndDrain 验证定向投递、Drain 与优先级排序。
func TestMailbox_DirectSendAndDrain(t *testing.T) {
	m := New()
	// 向 agentB 投递两条消息：普通 info 与高优先级 milestone。
	_, _ = m.Send(&Message{From: "agentA", To: "agentB", Type: MsgInfo, Subject: "hello"})
	_, _ = m.Send(&Message{From: "agentA", To: "agentB", Type: MsgMilestone, Subject: "done", Priority: 1})

	// 未读计数应为 2。
	if got := m.Count("agentB"); got != 2 {
		t.Fatalf("expected 2 unread, got %d", got)
	}
	got := m.Drain("agentB")
	if len(got) != 2 {
		t.Fatalf("expected 2 drained, got %d", len(got))
	}
	// 优先级排序：Priority=1 应排在前。
	if got[0].Subject != "done" {
		t.Fatalf("priority sort failed: %s", got[0].Subject)
	}
	// Drain 后未读计数应为 0。
	if m.Count("agentB") != 0 {
		t.Fatalf("after drain, expected 0 unread")
	}
}

// TestMailbox_BroadcastAndForward 验证广播消息的投递、DrainBroadcast 与幂等消费。
func TestMailbox_BroadcastAndForward(t *testing.T) {
	m := New()
	id, _ := m.Send(&Message{From: "meta", To: "*", Type: MsgEscalate, Subject: "needs help"})

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

// TestMailbox_Purge 验证 Purge 能清空指定 Agent 的全部邮件。
func TestMailbox_Purge(t *testing.T) {
	m := New()
	_, _ = m.Send(&Message{To: "x", Subject: "1"})
	_, _ = m.Send(&Message{To: "x", Subject: "2"})
	m.Purge("x")
	if m.Count("x") != 0 {
		t.Fatalf("Purge failed")
	}
}

// TestMailbox_DeadLetterAfterPurge 验证 TODO #23 死信可见：
// Purge 过的收件人再 Send 返回 ErrRecipientClosed，消息不入箱；广播不受影响。
func TestMailbox_DeadLetterAfterPurge(t *testing.T) {
	m := New()
	m.Purge("gone")
	if _, err := m.Send(&Message{To: "gone", Subject: "1"}); err == nil {
		t.Fatal("Send to purged recipient should return dead-letter error")
	}
	if m.Count("gone") != 0 {
		t.Fatal("dead-letter message must not enter inbox")
	}
	// 广播（To="*"）不校验死信。
	if _, err := m.Send(&Message{To: "*", Subject: "b"}); err != nil {
		t.Fatalf("broadcast should not be dead-lettered: %v", err)
	}
	// 未 Purge 的收件人正常投递。
	if _, err := m.Send(&Message{To: "alive", Subject: "ok"}); err != nil {
		t.Fatalf("send to alive recipient should succeed: %v", err)
	}
}

// TestMailbox_ReplyToAndThreadID 验证 ReplyTo/ThreadID 字段在投递后保留，
// 供多 Agent 验证闭环中"被询问方回复"的请求-响应配对使用。
func TestMailbox_ReplyToAndThreadID(t *testing.T) {
	m := New()
	_, _ = m.Send(&Message{
		From:     "code_assistant-1",
		To:       "test_assistant-2",
		Type:     MsgRequest,
		Subject:  "verify the fix",
		Body:     "please run tests on calc.go",
		ReplyTo:  "code_assistant-1",
		ThreadID: "verify-abc",
	})
	msgs := m.Drain("test_assistant-2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].ReplyTo != "code_assistant-1" {
		t.Fatalf("ReplyTo mismatch: got %q", msgs[0].ReplyTo)
	}
	if msgs[0].ThreadID != "verify-abc" {
		t.Fatalf("ThreadID mismatch: got %q", msgs[0].ThreadID)
	}
	if msgs[0].Type != MsgRequest {
		t.Fatalf("Type mismatch: got %q", msgs[0].Type)
	}
}
