package mailbox

import (
	"testing" // Go 测试框架
	"time"
)

// TestMailbox_DirectSendAndDrain 验证定向投递、Drain 与优先级排序。
func TestMailbox_DirectSendAndDrain(t *testing.T) {
	m := New()
	// 向 agentB 投递两条消息：普通 info 与高优先级 milestone。
	m.Send(&Message{From: "agentA", To: "agentB", Type: MsgInfo, Subject: "hello"})
	m.Send(&Message{From: "agentA", To: "agentB", Type: MsgMilestone, Subject: "done", Priority: 1})

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

// TestMailbox_Purge 验证 Purge 能清空指定 Agent 的全部邮件。
func TestMailbox_Purge(t *testing.T) {
	m := New()
	m.Send(&Message{To: "x", Subject: "1"})
	m.Send(&Message{To: "x", Subject: "2"})
	m.Purge("x")
	if m.Count("x") != 0 {
		t.Fatalf("Purge failed")
	}
}

// TestMailbox_ReplyToAndThreadID 验证 ReplyTo/ThreadID 字段在投递后保留，
// 供多 Agent 验证闭环中"被询问方回复"的请求-响应配对使用。
func TestMailbox_ReplyToAndThreadID(t *testing.T) {
	m := New()
	m.Send(&Message{
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

// TestMailbox_WaitForMessage_Immediate 验证已有未读消息时 WaitForMessage 立即返回 true。
func TestMailbox_WaitForMessage_Immediate(t *testing.T) {
	m := New()
	m.Send(&Message{To: "a", Subject: "hi"})
	if !m.WaitForMessage("a", 100*time.Millisecond) {
		t.Fatal("expected immediate return when message already present")
	}
}

// TestMailbox_WaitForMessage_BlocksAndSignals 验证无消息时 WaitForMessage 阻塞，
// 收到新消息后被唤醒返回 true。
func TestMailbox_WaitForMessage_BlocksAndSignals(t *testing.T) {
	m := New()
	done := make(chan bool, 1)
	go func() {
		done <- m.WaitForMessage("worker", 500*time.Millisecond)
	}()
	// 给 goroutine 一点时间进入阻塞等待。
	time.Sleep(30 * time.Millisecond)
	m.Send(&Message{To: "worker", Subject: "wake up"})
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("expected true after signal, got false")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("WaitForMessage did not return after signal")
	}
}

// TestMailbox_WaitForMessage_Timeout 验证无消息且超时后返回 false。
func TestMailbox_WaitForMessage_Timeout(t *testing.T) {
	m := New()
	start := time.Now()
	if m.WaitForMessage("nobody", 50*time.Millisecond) {
		t.Fatal("expected false on timeout")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected to wait at least 50ms, got %v", elapsed)
	}
}
