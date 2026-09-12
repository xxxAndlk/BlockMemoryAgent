// Package agent ask_user 提问前的正文留存测试（2026-09-12 用户实证回归）。
//
// ask_user_report_test.go 覆盖"出现询问时吞掉上一段输出"：
// 模型常见「先流式输出正文、再调 ask_user」——正文只活在瞬时字段 StreamingText
// （流式增量按设计不落事件），前端在待澄清态只渲染问答卡，于是上一段正文整段消失，
// 用户只剩思考链。
//
// 契约：提问事件带 {"report_text": ...} 快照（ask_user 阻塞期间 StreamingText 仍保留
// 该正文，恢复才清），前端把它渲染在问答卡上方；历史回放/刷新/重启后仍在。
// 空正文不写 detail_json（不给事件加噪声）。
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/go-kratos/blades"
)

// reportThenAskProvider 在每次生成前把"提问前的答复正文"写进会话瞬时字段
// StreamingText——真实路径由 LiveEventLLMDelta 逐块累积写入，这里直接置值等价于
// 「模型已把正文流完、接着调 ask_user」。
type reportThenAskProvider struct {
	*askCaptureProvider
	svc  *ReactService
	text string
}

func (p *reportThenAskProvider) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	p.svc.store.mu.Lock()
	for _, s := range p.svc.store.sessions {
		s.StreamingText = p.text
	}
	p.svc.store.mu.Unlock()
	return p.askCaptureProvider.Generate(ctx, req)
}

// reportFromEvent 解出提问事件里挂的正文快照（未挂/解析失败返回空串）。
func reportFromEvent(t *testing.T, ev Event) string {
	t.Helper()
	if ev.DetailJSON == "" {
		return ""
	}
	var d struct {
		ReportText string `json:"report_text"`
	}
	if err := json.Unmarshal([]byte(ev.DetailJSON), &d); err != nil {
		t.Fatalf("提问事件 detail_json 应可解析, got %q", ev.DetailJSON)
	}
	return d.ReportText
}

// waitAskPending 等待单题 PendingClarify 挂出。
func waitAskPending(t *testing.T, svc *ReactService, id string) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(context.Background(), id)
		if sess != nil && sess.PendingClarify != nil {
			return sess
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected pending clarify within timeout")
	return nil
}

// TestAskUser_ReportAttachedToClarifyEvent 单题：提问前正文随提问事件留存，答复后仍可回看。
func TestAskUser_ReportAttachedToClarifyEvent(t *testing.T) {
	base := &askCaptureProvider{responses: []*blades.Message{
		askUserToolCall("游戏配色用深色还是浅色？"),
		blades.AssistantMessage("完成"),
	}}
	svc := newBatchTestService(t, base)
	llm := &reportThenAskProvider{askCaptureProvider: base, svc: svc, text: "本轮正文：已梳理三项决策点，先确认配色方向。"}
	svc.testProvider = llm
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "做原型"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := waitAskPending(t, svc, created.ID)

	got := ""
	for _, ev := range sess.Events {
		if ev.Type == eventkind.Clarify && strings.HasPrefix(ev.Message, "Agent 提问: ") {
			got = reportFromEvent(t, ev)
		}
	}
	if !strings.Contains(got, "已梳理三项决策点") {
		t.Fatalf("提问事件应带提问前正文快照, got %q", got)
	}

	// 答复后会话正常续跑（正文快照不影响答复通道）。
	if err := svc.answerClarify(ctx, created.ID, "深色", nil); err != nil {
		t.Fatalf("answerClarify: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, _ := svc.Get(ctx, created.ID)
		if s != nil && s.Status == "completed" {
			// 恢复即清瞬时缓冲，但事件里的快照仍在（前端靠它回看）。
			for _, ev := range s.Events {
				if strings.HasPrefix(ev.Message, "Agent 提问: ") && strings.Contains(reportFromEvent(t, ev), "已梳理三项决策点") {
					return
				}
			}
			t.Fatal("答复后提问事件里的正文快照应仍在（历史可回看）")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session did not complete after answer")
}

// TestAskUserBatch_ReportOnlyOnFirstQuestion 批量：正文只挂第一条提问事件（不逐题复制），
// 前端按"任一 clarify 事件带正文即渲染"取值。
func TestAskUserBatch_ReportOnlyOnFirstQuestion(t *testing.T) {
	base := &askCaptureProvider{responses: []*blades.Message{
		batchAskToolCall(),
		blades.AssistantMessage("完成"),
	}}
	svc := newBatchTestService(t, base)
	svc.testProvider = &reportThenAskProvider{askCaptureProvider: base, svc: svc, text: "批量提问前的正文"}

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "搭建服务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := waitBatchPending(t, svc, created.ID)

	reports := []string{}
	for _, ev := range sess.Events {
		if ev.Type == eventkind.Clarify && strings.HasPrefix(ev.Message, "Agent 提问: ") {
			reports = append(reports, reportFromEvent(t, ev))
		}
	}
	if len(reports) != 2 {
		t.Fatalf("应有 2 条提问事件, got %d", len(reports))
	}
	if reports[0] != "批量提问前的正文" {
		t.Fatalf("正文应挂在第一条提问事件, got %q", reports[0])
	}
	if reports[1] != "" {
		t.Fatalf("后续提问事件不应重复挂正文, got %q", reports[1])
	}
}

// TestAskUser_NoReportNoDetail 提问前没有正文（如直接工具调用）时不写 detail_json。
func TestAskUser_NoReportNoDetail(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		askUserToolCall("继续吗？"),
		blades.AssistantMessage("完成"),
	}}
	svc := newBatchTestService(t, llm)

	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "做原型"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := waitAskPending(t, svc, created.ID)

	for _, ev := range sess.Events {
		if ev.Type == eventkind.Clarify && strings.HasPrefix(ev.Message, "Agent 提问: ") {
			if ev.DetailJSON != "" {
				t.Fatalf("空正文不应写 detail_json, got %q", ev.DetailJSON)
			}
			return
		}
	}
	t.Fatal("未找到提问事件")
}
