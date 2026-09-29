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
// Purge 过的收件人再 Send 返回 ErrRecipientClosed，消息不入箱。
func TestMailbox_DeadLetterAfterPurge(t *testing.T) {
	m := New()
	m.Purge("gone")
	if _, err := m.Send(&Message{To: "gone", Subject: "1"}); err == nil {
		t.Fatal("Send to purged recipient should return dead-letter error")
	}
	if m.Count("gone") != 0 {
		t.Fatal("dead-letter message must not enter inbox")
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

// Send 拒绝空/广播收件人（广播桶已删除，防止消息永久堆积无人消费）。
func TestSendRejectsBroadcast(t *testing.T) {
	m := New()
	if _, err := m.Send(&Message{From: "a", To: "", Type: MsgInfo, Subject: "x"}); err == nil {
		t.Fatal("To 为空应返回错误")
	}
	if _, err := m.Send(&Message{From: "a", To: "*", Type: MsgInfo, Subject: "x"}); err == nil {
		t.Fatal("To=* 应返回错误")
	}
}

// request/escalate 且未填 thread_id 时，Send 自动回填 ThreadID=消息 ID（问答链配对锚点）。
func TestSendAutoThreadID(t *testing.T) {
	m := New()
	msg := &Message{From: "a", To: "b", Type: MsgRequest, Subject: "问"}
	id, err := m.Send(msg)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ThreadID != id {
		t.Fatalf("request 未回填 ThreadID=id：got %q want %q", msg.ThreadID, id)
	}
	// info 不回填；显式 thread_id 不被覆盖。
	info := &Message{From: "a", To: "b", Type: MsgInfo, Subject: "报"}
	if _, err := m.Send(info); err != nil {
		t.Fatal(err)
	}
	if info.ThreadID != "" {
		t.Fatalf("info 不应回填 ThreadID：got %q", info.ThreadID)
	}
	explicit := &Message{From: "a", To: "b", Type: MsgRequest, Subject: "问2", ThreadID: "t-fixed"}
	if _, err := m.Send(explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.ThreadID != "t-fixed" {
		t.Fatalf("显式 ThreadID 被覆盖：got %q", explicit.ThreadID)
	}
}

// Purge 返回被丢弃的未读消息（死信通知数据源），已读不在其列。
func TestPurgeReturnsDroppedUnread(t *testing.T) {
	m := New()
	if _, err := m.Send(&Message{From: "x", To: "a", Type: MsgRequest, Subject: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Send(&Message{From: "x", To: "a", Type: MsgInfo, Subject: "i1"}); err != nil {
		t.Fatal(err)
	}
	// drain 掉第一条使其已读，第二条保持未读。
	m.Drain("a")
	if _, err := m.Send(&Message{From: "y", To: "a", Type: MsgRequest, Subject: "r2"}); err != nil {
		t.Fatal(err)
	}
	dropped := m.Purge("a")
	if len(dropped) != 1 || dropped[0].Subject != "r2" {
		t.Fatalf("Purge 应只返回未读 r2：got %+v", dropped)
	}
	if m.Count("a") != 0 {
		t.Fatal("Purge 后收件箱应为空")
	}
}

// 四类 hook 触发时机。
func TestMailboxHooks(t *testing.T) {
	m := New()
	var persisted, readMarked, deadMarked, restored []string
	m.WithPersist(func(msg *Message) { persisted = append(persisted, msg.ID) })
	m.WithReadMarker(func(ids []string) { readMarked = append(readMarked, ids...) })
	m.WithDeadMarker(func(ids []string) { deadMarked = append(deadMarked, ids...) })
	m.WithRestoreHandler(func(msg *Message) { restored = append(restored, msg.ID) })

	id, err := m.Send(&Message{From: "x", To: "a", Type: MsgRequest, Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0] != id {
		t.Fatalf("persist hook 未触发：%v", persisted)
	}
	m.Drain("a")
	if len(readMarked) != 1 || readMarked[0] != id {
		t.Fatalf("readMark hook 未触发：%v", readMarked)
	}
	id2, _ := m.Send(&Message{From: "x", To: "a", Type: MsgInfo, Subject: "s2"})
	m.Purge("a")
	if len(deadMarked) != 1 || deadMarked[0] != id2 {
		t.Fatalf("deadMark hook 应只含未读 id2：%v", deadMarked)
	}
	// Restore：保留原 ID 入箱、不触发 persist/trace、触发 restoreHandler、可被 Drain 读到。
	persisted = nil
	m.Reopen("a") // 清上面的 closed 标记
	err = m.Restore(&Message{ID: "msg_restore_1", From: "x", To: "a", Type: MsgRequest, Subject: "rs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 0 {
		t.Fatalf("Restore 不应触发 persist：%v", persisted)
	}
	if len(restored) != 1 || restored[0] != "msg_restore_1" {
		t.Fatalf("restoreHandler 未触发：%v", restored)
	}
	got := m.Drain("a")
	if len(got) != 1 || got[0].ID != "msg_restore_1" {
		t.Fatalf("Restore 后 Drain 应读到原 ID 消息：%+v", got)
	}
	// Restore 幂等：同 ID 重复恢复不产生第二条。
	if err := m.Restore(&Message{ID: "msg_restore_1", From: "x", To: "a", Type: MsgRequest, Subject: "rs"}); err != nil {
		t.Fatal(err)
	}
	if n := len(m.Drain("a")); n != 0 {
		t.Fatalf("同 ID Restore 应幂等去重，Drain 应空，got %d", n)
	}
}
