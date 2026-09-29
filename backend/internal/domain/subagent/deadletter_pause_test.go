package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// pauseExit 出口判定：暂停出口（token 触限 errPaused / 手动暂停 / 软停止 domain）
// 保留邮箱待续跑 drain；终结出口（成功/失败/硬取消/软停止叶子）照常清空回投死信。
func TestPauseExit(t *testing.T) {
	d := &Dispatcher{softStops: map[string]bool{}, pauseRequests: map[string]bool{}}
	d.MarkPauseNode("sess/pause-me")
	d.SetSoftStop("sess-stop")

	cases := []struct {
		name   string
		subID  string
		roleID string
		sid    string
		err    error
		want   bool
	}{
		{"成功出口=终结", "sess/a", "domain", "sess", nil, false},
		{"普通错误=终结", "sess/a", "domain", "sess", errors.New("boom"), false},
		{"errPaused=暂停", "sess/a", "domain", "sess", errPaused, true},
		{"errPaused 不因子 Agent ID 误判", "sess-stop/pause-me", "domain", "sess-stop", errPaused, true},
		{"硬取消=终结", "sess/a", "domain", "sess", context.Canceled, false},
		{"硬取消包装后=终结", "sess/a", "domain", "sess", fmt.Errorf("run: %w", context.Canceled), false},
		{"手动暂停标记+取消=暂停(domain)", "sess/pause-me", "domain", "sess", context.Canceled, true},
		{"手动暂停标记+取消=暂停(叶子)", "sess/pause-me", "code_assistant", "sess", fmt.Errorf("run: %w", context.Canceled), true},
		{"软停止+取消+domain=暂停", "sess-stop/d", "domain", "sess-stop", context.Canceled, true},
		{"软停止+取消+叶子=终结(部分回灌)", "sess-stop/leaf", "code_assistant", "sess-stop", context.Canceled, false},
		{"软停止标记仅其余会话=终结", "sess/a", "domain", "sess", context.Canceled, false},
		{"非取消错误+暂停标记=终结(标记只分流取消)", "sess/pause-me", "domain", "sess", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.pauseExit(tc.subID, tc.roleID, tc.sid, tc.err); got != tc.want {
				t.Fatalf("pauseExit(%q, %q, %q, %v) = %v, want %v", tc.subID, tc.roleID, tc.sid, tc.err, got, tc.want)
			}
		})
	}
}

// 收口语义组合（与 runSubAgentOnce/ResumePaused 的收尾 defer 同口径）：
// 暂停出口跳过 purge——收件箱保留、无死信、pending 不销账（超时升级仍有效）；
// 终结出口照常 purge——死信回投 + pending 销账。
func TestPauseExitSkipsPurgeKeepsMailbox(t *testing.T) {
	mb := mailbox.New()
	d := &Dispatcher{mailbox: mb, pendingReqs: newPendingRequestRegistry()}

	// asker 分别向"将被暂停"与"将终结"的两个 Agent 发出 request（各登记 pending）。
	idPause, err := mb.Send(&mailbox.Message{From: "sess/asker", To: "sess/pause-vic", Type: mailbox.MsgRequest, Subject: "问接口"})
	if err != nil {
		t.Fatal(err)
	}
	d.pendingReqs.add(&pendingRequest{MsgID: idPause, From: "sess/asker", To: "sess/pause-vic", Subject: "问接口"})
	idTerm, err := mb.Send(&mailbox.Message{From: "sess/asker", To: "sess/term-vic", Type: mailbox.MsgRequest, Subject: "问配置"})
	if err != nil {
		t.Fatal(err)
	}
	d.pendingReqs.add(&pendingRequest{MsgID: idTerm, From: "sess/asker", To: "sess/term-vic", Subject: "问配置"})

	// 暂停出口（errPaused）：同 defer 口径跳过 purge。
	if !d.pauseExit("sess/pause-vic", "domain", "sess", errPaused) {
		d.purgeMailboxWithNotice("sess/pause-vic", "已终结")
	}
	if n := mb.Count("sess/pause-vic"); n != 1 {
		t.Fatalf("暂停出口邮箱应保留（未读 1 条），实际 %d", n)
	}
	if n := mb.Count("sess/asker"); n != 0 {
		t.Fatalf("暂停出口不应产生死信，asker 收到 %d 条", n)
	}

	// 终结出口（err==nil 成功）：照常 purge。
	if !d.pauseExit("sess/term-vic", "domain", "sess", nil) {
		d.purgeMailboxWithNotice("sess/term-vic", "已终结")
	}
	if n := mb.Count("sess/term-vic"); n != 0 {
		t.Fatalf("终结出口邮箱应清空，实际 %d", n)
	}
	got := mb.Drain("sess/asker")
	if len(got) != 1 || !strings.Contains(got[0].Subject, "未送达") || !strings.Contains(got[0].Body, "sess/term-vic") {
		t.Fatalf("终结出口 asker 应收到且仅收到 term-vic 的死信：%+v", got)
	}

	// pending 对账：暂停的仍登记在册（complete 命中返回非 nil），
	// 终结的已被死信销账（重复 complete 返回 nil）。
	if r := d.pendingReqs.complete(idPause); r == nil {
		t.Fatal("暂停出口的 pending 应保留（等续跑后应答/超时升级），实际已被销账")
	}
	if r := d.pendingReqs.complete(idTerm); r != nil {
		t.Fatal("终结出口的 pending 应已被死信销账")
	}
}
