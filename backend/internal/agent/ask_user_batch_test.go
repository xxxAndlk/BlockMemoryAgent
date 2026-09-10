// Package agent ask_user 批量模式全链路测试（任务 140）。
//
// ask_user_batch_test.go 验证批量问答协议：
//   - questions 数组一次挂出全部题目（PendingClarify.Questions + 顶层镜像第一题 + Detail）；
//   - detail 先于问题（1 条 clarify_detail 事件 + 逐题 question-only clarify 事件）；
//   - answers 数组统一提交（逐题解析回填、合并一条「提问答复」事件/一条消息）；
//   - 数量不一致/逐题空白 → 显式拒绝；批量挂起时 enqueue 拒绝；
//   - 会话硬取消 → 批量 hook 随 ctx 退出、槽位清理；
//   - 恢复即清 StreamingText（问题⑤回归：ask_user 阻塞期间旧流式文本不残留）。
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/go-kratos/blades"
)

// newBatchTestService 构造同时接线单题与批量 AskUser hook 的 ReactService。
func newBatchTestService(t *testing.T, provider ModelProvider) *ReactService {
	t.Helper()
	roleRegistry := role.NewRegistry(&pkgconfig.RoleConfigFile{})
	toolRegistry := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	s.testProvider = provider
	s.store.workDir = t.TempDir()
	toolRegistry.SetAskUserHook(s.AskUserHook())
	toolRegistry.SetAskUserBatchHook(s.AskUserBatchHook())
	return s
}

// batchAskToolCall 一次 ask_user 调用挂出 2 题批量 + detail 长上下文。
func batchAskToolCall() *blades.Message {
	return &blades.Message{
		Role: blades.RoleAssistant,
		Parts: []blades.Part{
			blades.ToolPart{Name: "ask_user", Request: string(mustJSON(map[string]any{
				"detail": "进度盘点：模块 A 已完成，模块 B 需要定案方向。",
				"questions": []any{
					map[string]any{"question": "数据库选哪个？", "options": []any{
						map[string]any{"id": "pg", "label": "PostgreSQL"},
						map[string]any{"id": "mysql", "label": "MySQL"},
					}},
					map[string]any{"question": "配置格式用哪种？"},
				},
			}))},
		},
	}
}

// waitBatchPending 等待批量 PendingClarify（2 题）挂出。
func waitBatchPending(t *testing.T, svc *ReactService, id string) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(context.Background(), id)
		if sess != nil && sess.PendingClarify != nil && len(sess.PendingClarify.Questions) == 2 {
			return sess
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected batch pending clarify within timeout")
	return nil
}

// TestAskUserBatchFlow 批量全链路：挂出（Questions/Detail/顶层镜像）→ 事件形态 →
// answers 统一提交 → 工具结果按题号入下一轮请求 → 合并答复事件/消息 → 恢复清流式缓冲。
func TestAskUserBatchFlow(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		batchAskToolCall(),
		blades.AssistantMessage("完成，按批量答复执行"),
	}}
	svc := newBatchTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "搭建服务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := waitBatchPending(t, svc, created.ID)

	// 挂出形态：Detail + Questions 全量 + 顶层镜像第一题（旧客户端兼容）。
	pc := sess.PendingClarify
	if !strings.Contains(pc.Detail, "进度盘点") {
		t.Fatalf("Detail 应承载长上下文, got %q", pc.Detail)
	}
	if pc.Question != "数据库选哪个？" || pc.Kind != "choice" {
		t.Fatalf("顶层应镜像第一题, question=%q kind=%q", pc.Question, pc.Kind)
	}
	if len(pc.Options) == 0 || pc.Options[0].ID != "pg" {
		t.Fatalf("顶层 Options 应镜像第一题选项, got %+v", pc.Options)
	}
	if sess.Status != string(enums.SessionStatusAwaitingClarify) {
		t.Fatalf("批量挂起应 awaiting_clarify, got %q", sess.Status)
	}

	// 事件形态：1 条 clarify_detail + 2 条 question-only（detail 不再拼进问题文本）。
	// Event.Type 是事件大类（clarify），Kind 是子类型（clarify_detail / 空）。
	detailCount, questionCount := 0, 0
	for _, ev := range sess.Events {
		switch {
		case ev.Kind == eventkind.ClarifyDetail:
			detailCount++
			if !strings.Contains(ev.Message, "进度盘点") {
				t.Fatalf("clarify_detail 事件应承载盘点全文, got %q", ev.Message)
			}
		case ev.Type == eventkind.Clarify && strings.HasPrefix(ev.Message, "Agent 提问: "):
			questionCount++
			if strings.Contains(ev.Message, "进度盘点") {
				t.Fatalf("问题事件不应再拼接长上下文, got %q", ev.Message)
			}
		}
	}
	if detailCount != 1 || questionCount != 2 {
		t.Fatalf("事件应为 1 条 detail + 2 条 question-only, got detail=%d question=%d", detailCount, questionCount)
	}

	// 模拟提问前那轮流式缓冲残留（问题⑤），答复后必须被清零。
	svc.store.mu.Lock()
	svc.store.sessions[created.ID].StreamingText = "上一轮旧正文残留"
	svc.store.mu.Unlock()

	// answers 数组统一提交（web 批量路径）。
	if err := svc.answerClarify(ctx, created.ID, "", []string{"pg", "yaml"}); err != nil {
		t.Fatalf("answerClarify batch: %v", err)
	}

	// 会话完成；选项命中回传 Label，自由文本原样。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ = svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sess == nil || sess.Status != string(enums.SessionStatusCompleted) {
		t.Fatalf("session did not complete after batch answer, status=%v", sess.Status)
	}
	if !llm.requestContains("答复: PostgreSQL") || !llm.requestContains("答复: yaml") {
		t.Fatalf("batch tool result should carry per-question answers into next request")
	}

	// 合并为一条「提问答复」User 事件 + 一条 [澄清答复] 消息（web 每 user_message
	// 开新 turn，逐题拆开会把答复区打成 N 段）。
	answerEvents := 0
	for _, ev := range sess.Events {
		if ev.Type == eventkind.Clarify && ev.Agent == "User" && strings.HasPrefix(ev.Message, "提问答复: ") {
			answerEvents++
			if !strings.Contains(ev.Message, "1. PostgreSQL") || !strings.Contains(ev.Message, "2. yaml") {
				t.Fatalf("合并事件应按题号汇总, got %q", ev.Message)
			}
		}
	}
	if answerEvents != 1 {
		t.Fatalf("批量答复应合并为 1 条事件, got %d", answerEvents)
	}
	foundMsg := false
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, "[澄清答复] 1. PostgreSQL") && strings.Contains(m.Content, "2. yaml") {
			foundMsg = true
		}
	}
	if !foundMsg {
		t.Fatalf("批量答复应合并为一条 [澄清答复] 消息, messages=%d", len(sess.Messages))
	}

	// 恢复即清流式缓冲（问题⑤回归）。
	svc.store.mu.Lock()
	st := svc.store.sessions[created.ID].StreamingText
	svc.store.mu.Unlock()
	if st != "" {
		t.Fatalf("批量恢复应清 StreamingText, got %q", st)
	}
}

// TestAskUserBatch_CountMismatchRejected 数量不一致 / 逐题空白 → 显式拒绝且会话仍挂起。
func TestAskUserBatch_CountMismatchRejected(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		batchAskToolCall(),
		blades.AssistantMessage("完成"),
	}}
	svc := newBatchTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "搭建服务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitBatchPending(t, svc, created.ID)

	// 1 题答复对 2 题挂出：数量校验拒绝。
	err = svc.answerClarify(ctx, created.ID, "", []string{"pg"})
	if err == nil || !strings.Contains(err.Error(), "答复数量与问题数不一致") {
		t.Fatalf("数量不一致应显式拒绝, err=%v", err)
	}
	// 逐题空白：第 N 题为空拒绝。
	err = svc.answerClarify(ctx, created.ID, "", []string{"pg", "   "})
	if err == nil || !strings.Contains(err.Error(), "第 2 题答复为空") {
		t.Fatalf("空白答复应逐题拒绝, err=%v", err)
	}
	// 会话仍处于待答复且槽位未被消费（可正常补交）。
	if err := svc.answerClarify(ctx, created.ID, "", []string{"pg", "yaml"}); err != nil {
		t.Fatalf("补交完整答复应成功, err=%v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusCompleted) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session did not complete after corrected batch answer")
}

// TestAskUserBatch_EnqueueRejected 批量挂起时 enqueue 拒绝并提示走问答面板。
func TestAskUserBatch_EnqueueRejected(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		batchAskToolCall(),
		blades.AssistantMessage("完成"),
	}}
	svc := newBatchTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "搭建服务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitBatchPending(t, svc, created.ID)

	err = svc.enqueue(ctx, created.ID, "随便补充一句")
	if err == nil || !strings.Contains(err.Error(), "批量") {
		t.Fatalf("批量挂起时 enqueue 应拒绝, err=%v", err)
	}
	// 槽位未被 enqueue 误路由：批量补交仍成功。
	if err := svc.answerClarify(ctx, created.ID, "", []string{"pg", "yaml"}); err != nil {
		t.Fatalf("enqueue 拒绝后批量答复应仍可提交, err=%v", err)
	}
}

// TestAskUserBatch_CancelMidBatch 会话硬取消：批量 hook 随 ctx 退出、槽位清理、
// 会话进入错误终态（Result=cancelled by user）而非被恢复重挂。
func TestAskUserBatch_CancelMidBatch(t *testing.T) {
	llm := &askCaptureProvider{responses: []*blades.Message{
		batchAskToolCall(),
		blades.AssistantMessage("不应到达"),
	}}
	svc := newBatchTestService(t, llm)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "搭建服务"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitBatchPending(t, svc, created.ID)

	if err := svc.cancel(ctx, created.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sess, _ := svc.Get(ctx, created.ID)
		if sess != nil && sess.Status == string(enums.SessionStatusError) {
			// 终态校验：cancel 直接落错误态 + cancelled by user。
			if sess.Result != "cancelled by user" {
				t.Fatalf("取消终态 Result 应为 cancelled by user, got %q", sess.Result)
			}
			// 槽位清理：批量答复在取消后必须被拒绝（非 awaiting_clarify），
			// 而非写入已取出的通道。
			if err := svc.answerClarify(ctx, created.ID, "", []string{"pg", "yaml"}); err == nil {
				t.Fatal("取消后批量答复应被拒绝")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session did not reach error status after cancel mid batch")
}
