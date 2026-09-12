package subagent

// dispatcher_revive_test.go 验证编排页用户直连的两个内核动作：
//  1. InjectUserMessage：投 From=user 的 MsgRequest 邮件 + pokeParent 唤醒 wait loop；
//  2. ReviveWithMessage：终态节点 Reopen 回 Running 同 ID 重跑，父收到"复活返工"通知。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// TestInjectUserMessage 验证：注入即投 From=user 的 MsgRequest 邮件，
// 且 pokeParent 唤醒目标 wait loop（等子中的 Agent 立刻重检邮箱而非等满周期）。
func TestInjectUserMessage(t *testing.T) {
	d, _, mb, _, _, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "ok"})
	target := "s1/code_assistant-1"

	// 制造未决计数 > 0：WaitForAnyChild 才会真正阻塞等待信号（计数 0 时立即返回 true）。
	d.trackChildStart(target)
	waited := make(chan bool, 1)
	go func() { waited <- d.WaitForAnyChild(target, 3*time.Second) }()
	time.Sleep(50 * time.Millisecond) // 等 goroutine 进入阻塞

	if err := d.InjectUserMessage(target, "看看进度"); err != nil {
		t.Fatalf("InjectUserMessage 失败: %v", err)
	}

	// 邮件形态：From=user + MsgRequest + Body 原文（编排页对话面板的识别口径）。
	msgs := mb.Peek(target)
	if len(msgs) != 1 {
		t.Fatalf("应投递 1 封邮件, got %d", len(msgs))
	}
	if msgs[0].From != "user" || msgs[0].Type != mailbox.MsgRequest || !strings.Contains(msgs[0].Body, "看看进度") {
		t.Fatalf("邮件形态不符: from=%s type=%s body=%q", msgs[0].From, msgs[0].Type, msgs[0].Body)
	}

	// poke 唤醒：不显式 poke 时要等满 3s wait 周期（mailbox.Send 本身不唤醒阻塞方）。
	select {
	case ok := <-waited:
		if !ok {
			t.Fatal("WaitForAnyChild 返回 false，poke 未生效")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poke 未唤醒等待方（超时）")
	}
}

// fakeReviveMsgLogger 记录 Clear 调用（复活须先清旧热层，防 seq 重编号撞旧游标）。
type fakeReviveMsgLogger struct {
	cleared []string
	logged  int
}

func (f *fakeReviveMsgLogger) Log(string, int, agent.ReactMessage) { f.logged++ }
func (f *fakeReviveMsgLogger) Clear(agentID string)                { f.cleared = append(f.cleared, agentID) }

// TestReviveWithMessage 验证：终态节点 Reopen 回 Running、同 ID 重跑、
// 父收到"复活返工"通知（提示词 Task 8 配套：父知悉勿重复派发同领域）。
func TestReviveWithMessage(t *testing.T) {
	d, _, mb, _, tr, _ := newPauseTestEnv(t, &tokenUsageProvider{text: "revived"})
	ml := &fakeReviveMsgLogger{}
	d.WithMessageLogger(ml)
	subID := "s1/domain-1"
	tr.Register(orchestrator.Node{
		ID: subID, ParentID: "s1", Role: "domain", Domain: "配置",
		Task: "实现 config.js", Status: orchestrator.StatusRunning,
	})
	tr.Finish(subID, "上一轮已写 config.js", nil)
	node, ok := tr.Get(subID)
	if !ok || node.Status != orchestrator.StatusDone {
		t.Fatalf("前置终态节点未就绪: %+v ok=%t", node, ok)
	}

	if err := d.ReviveWithMessage(dispatchCtx(), node, "改成 yaml 配置"); err != nil {
		t.Fatalf("ReviveWithMessage 失败: %v", err)
	}
	if n, _ := tr.Get(subID); n.Status != orchestrator.StatusRunning {
		t.Fatalf("复活后应 Running, got %s", n.Status)
	}

	// 父感知：复活返工通知必达（Subject 精确匹配）。
	waitForCond(t, "revive notice to parent", func() bool {
		for _, m := range mb.Peek("s1") {
			if strings.Contains(m.Subject, "复活返工") {
				return true
			}
		}
		return false
	})

	// 复活前清旧热层：新 run 的 seq 从 0 重编号，不清会让对话页增量游标永久取不到新消息。
	if len(ml.cleared) != 1 || ml.cleared[0] != subID {
		t.Fatalf("复活前应清热层一次: %+v", ml.cleared)
	}
}
