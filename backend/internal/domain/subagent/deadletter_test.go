package subagent

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// 子 Agent 终结被 Purge 时，未读 request 的发送方收到"未送达"死信通知，且问答销账。
func TestPurgeMailboxWithNotice(t *testing.T) {
	mb := mailbox.New()
	d := &Dispatcher{mailbox: mb, pendingReqs: newPendingRequestRegistry()}
	// a 向 victim 发出 request（登记 pending）。
	id, err := mb.Send(&mailbox.Message{From: "sess/a", To: "sess/victim", Type: mailbox.MsgRequest, Subject: "问接口"})
	if err != nil {
		t.Fatal(err)
	}
	d.pendingReqs.add(&pendingRequest{MsgID: id, From: "sess/a", To: "sess/victim", Subject: "问接口"})
	// info 类未读不产生通知。
	if _, err := mb.Send(&mailbox.Message{From: "sess/c", To: "sess/victim", Type: mailbox.MsgInfo, Subject: "纯告知"}); err != nil {
		t.Fatal(err)
	}
	d.purgeMailboxWithNotice("sess/victim", "已终结")
	// a 收到死信通知。
	got := mb.Drain("sess/a")
	if len(got) != 1 || !strings.Contains(got[0].Subject, "未送达") {
		t.Fatalf("a 应收到死信通知：%+v", got)
	}
	// pending 已销账（死信等价于"永远不会有回复"）。
	if r := d.pendingReqs.complete(id); r != nil {
		t.Fatal("死信后 pending 应已销账")
	}
	// c 的 info 不产生通知。
	if n := mb.Count("sess/c"); n != 0 {
		t.Fatalf("info 未读不应通知发送方，c 收到 %d 条", n)
	}
}

// 收件箱为空/无未读 request 时零行为。
func TestPurgeMailboxWithNoticeNoop(t *testing.T) {
	mb := mailbox.New()
	d := &Dispatcher{mailbox: mb, pendingReqs: newPendingRequestRegistry()}
	d.purgeMailboxWithNotice("sess/none", "已终结") // 不 panic 即通过
	if _, err := mb.Send(&mailbox.Message{From: "sess/x", To: "sess/none2", Type: mailbox.MsgInfo, Subject: "s"}); err != nil {
		t.Fatal(err)
	}
	d.purgeMailboxWithNotice("sess/none2", "已终结")
	if n := mb.Count("sess/x"); n != 0 {
		t.Fatalf("info 不应触发通知：%d", n)
	}
}
