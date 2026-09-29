package mailbox

import "testing"

// TestSendTraceHook 验证 WithTrace 钩子：定向投递成功后触发一次；
// 死信（收件人已 Purge）不触发；钩子收到的副本与 inbox 中消息解耦。
func TestSendTraceHook(t *testing.T) {
	m := New()
	var traced []*Message
	m.WithTrace(func(msg *Message) { traced = append(traced, msg) })

	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	if len(traced) != 1 || traced[0].To != "b" || traced[0].From != "a" {
		t.Fatalf("定向投递应留痕 1 条, got %+v", traced)
	}
	m.Purge("b")
	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "x"}); err == nil {
		t.Fatal("已销毁收件人应死信")
	}
	if len(traced) != 1 {
		t.Fatalf("死信不应留痕, got %d", len(traced))
	}
}

// TestReopenAfterPurge 验证复活重开邮箱：Purge 后投递死信，Reopen 后同一 ID 恢复可投递
// （编排页"复活重跑"沿用同一实例 ID，不重开则子 Agent 回传与直连注入全部丢失）。
func TestReopenAfterPurge(t *testing.T) {
	m := New()
	m.Purge("b")
	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "x"}); err == nil {
		t.Fatal("Purge 后应死信")
	}
	m.Reopen("b")
	if _, err := m.Send(&Message{From: "a", To: "b", Type: MsgRequest, Subject: "y"}); err != nil {
		t.Fatalf("Reopen 后应可投递: %v", err)
	}
	if got := m.Peek("b"); len(got) != 1 {
		t.Fatalf("Reopen 后 Peek = %d, want 1", len(got))
	}
	// 空 ID 幂等安全（调用方无需判空）。
	m.Reopen("")
}
