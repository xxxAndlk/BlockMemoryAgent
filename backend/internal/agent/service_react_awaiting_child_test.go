package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/pkg/enums"

	"github.com/go-kratos/blades"
)

// stubPendingChildren 提供可运行时调整的未决子 Agent 计数。
type stubPendingChildren struct{ n atomic.Int32 }

func (s *stubPendingChildren) PendingChildren(_ string) int { return int(s.n.Load()) }
func (s *stubPendingChildren) WaitForAnyChild(_ string, _ time.Duration) bool {
	return false
}

// recordingSuspendProvider 记录每轮请求里最后一条 user 消息，用于验证 wakeInput 注入。
type recordingSuspendProvider struct {
	mu        sync.Mutex
	responses []*blades.Message
	calls     int
	lastUser  string
}

func (m *recordingSuspendProvider) Generate(_ context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == blades.RoleUser {
			m.lastUser = bladesText(req.Messages[i])
			break
		}
	}
	if m.calls >= len(m.responses) {
		return nil, fmt.Errorf("no more responses (call %d)", m.calls+1)
	}
	resp := m.responses[m.calls]
	m.calls++
	return &blades.ModelResponse{Message: resp}, nil
}

func (m *recordingSuspendProvider) Name() string { return "recording-suspend" }

func (m *recordingSuspendProvider) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *recordingSuspendProvider) lastUserMessage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastUser
}

func newAwaitingChildService(t *testing.T, llm *recordingSuspendProvider, pending *stubPendingChildren) (*ReactService, string) {
	t.Helper()
	svc := newReactServiceForTest(llm, t.TempDir())
	svc.SetPendingChildrenChecker(pending)
	created, err := svc.CreateSession(context.Background(), CreateRequest{Goal: "并行调研两个主题"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitForStatus(t, svc, created.ID, enums.SessionStatusAwaitingChild, "awaiting_child")
	return svc, created.ID
}

// Meta 无 tool_calls 且仍有子在跑时应挂起为 awaiting_child，而不是烧轮次轮询或正常完成。
func TestRunSessionSuspendOnChildWait(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发 2 个子 Agent，等待回传"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	snap, err := svc.Get(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if snap.Status != string(enums.SessionStatusAwaitingChild) {
		t.Fatalf("status=%s, want awaiting_child", snap.Status)
	}
	if !snap.EndedAt.IsZero() {
		t.Fatalf("EndedAt should stay zero for awaiting_child session")
	}
	if got := llm.callCount(); got != 1 {
		t.Fatalf("llm calls=%d, want 1 (挂起后不应继续烧轮次)", got)
	}
	foundRelay := false
	for _, ev := range snap.Events {
		if ev.Type == eventkind.Message && ev.Agent == "MetaAgent" && strings.Contains(ev.Message, "等待回传") {
			foundRelay = true
			break
		}
	}
	if !foundRelay {
		t.Fatalf("missing MetaAgent relay event")
	}
}

// 子任务完成回调应唤醒挂起会话，把固定提示作为新一轮输入继续跑。
func TestWakeOnChildDoneResumesSuspendedSession(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发 2 个子 Agent，等待回传"),
		blades.AssistantMessage("已整合子 Agent 结果，任务完成"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	pending.n.Store(0)
	svc.WakeOnChildDone(sessionID)
	waitForStatus(t, svc, sessionID, enums.SessionStatusCompleted, "completed")

	snap, _ := svc.Get(context.Background(), sessionID)
	if !strings.Contains(snap.Result, "已整合子 Agent 结果") {
		t.Fatalf("result=%q, want second response text", snap.Result)
	}
	if got := llm.callCount(); got != 2 {
		t.Fatalf("llm calls=%d, want 2", got)
	}
	if last := llm.lastUserMessage(); !strings.Contains(last, "子 Agent 完成回传") {
		t.Fatalf("wake input not used as next turn input, last user=%q", last)
	}
	svc.store.mu.RLock()
	remaining := svc.store.sessions[sessionID].wakeInput
	svc.store.mu.RUnlock()
	if remaining != "" {
		t.Fatalf("wakeInput should be consumed, got %q", remaining)
	}
}

// 对非挂起会话或不存在会话调用 WakeOnChildDone 应幂等空转。
func TestWakeOnChildDoneIdempotent(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发，等待回传"),
		blades.AssistantMessage("完成"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	// 不存在的会话：不应 panic。
	svc.WakeOnChildDone("sess-not-exists")

	// 先唤醒并完成，再对 completed 会话重复调用：状态与模型调用数均不应变化。
	pending.n.Store(0)
	svc.WakeOnChildDone(sessionID)
	waitForStatus(t, svc, sessionID, enums.SessionStatusCompleted, "completed")
	svc.WakeOnChildDone(sessionID)
	time.Sleep(300 * time.Millisecond)
	snap, _ := svc.Get(context.Background(), sessionID)
	if snap.Status != string(enums.SessionStatusCompleted) {
		t.Fatalf("status=%s after duplicate wake, want completed", snap.Status)
	}
	if got := llm.callCount(); got != 2 {
		t.Fatalf("llm calls=%d after duplicate wake, want 2", got)
	}
}

// WakeSuspended（邮箱请求路径：submit_plan 审批 / send_message request/escalate）
// 应以自定义提示唤醒挂起会话，提示作新一轮输入。
func TestWakeSuspendedResumesWithCustomHint(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发，等待回传"),
		blades.AssistantMessage("已审阅计划并答复"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	pending.n.Store(0)
	if ok := svc.WakeSuspended(sessionID, "【系统】子 Agent 已提交开工计划待你审批"); !ok {
		t.Fatal("WakeSuspended on awaiting_child session should return true")
	}
	waitForStatus(t, svc, sessionID, enums.SessionStatusCompleted, "completed")
	if last := llm.lastUserMessage(); !strings.Contains(last, "开工计划待你审批") {
		t.Fatalf("custom wake hint not used as next turn input, last user=%q", last)
	}
}

// 对不存在/非挂起会话调用 WakeSuspended 应幂等空转返回 false，不产生额外轮次。
func TestWakeSuspendedIdempotentOnNonSuspended(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发，等待回传"),
		blades.AssistantMessage("完成"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	if ok := svc.WakeSuspended("sess-not-exists", "hint"); ok {
		t.Fatal("WakeSuspended on missing session should return false")
	}

	pending.n.Store(0)
	svc.WakeOnChildDone(sessionID)
	waitForStatus(t, svc, sessionID, enums.SessionStatusCompleted, "completed")
	before := llm.callCount()
	if ok := svc.WakeSuspended(sessionID, "hint"); ok {
		t.Fatal("WakeSuspended on completed session should return false")
	}
	time.Sleep(300 * time.Millisecond)
	if got := llm.callCount(); got != before {
		t.Fatalf("llm calls=%d after no-op wake, want %d", got, before)
	}
}

// awaiting_child 会话应可被 cancel（手动取消）。
func TestCancelAwaitingChildSession(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发，等待回传"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)

	if err := svc.cancel(context.Background(), sessionID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	snap, _ := svc.Get(context.Background(), sessionID)
	if snap.Status != string(enums.SessionStatusError) {
		t.Fatalf("status=%s, want error", snap.Status)
	}
}

// 智能唤醒（C-3b）：仍有未决子且邮箱无未读时，子完成不翻态（省"收到，继续等"
// 空转轮）；pending 归零后同一回调必醒。
func TestWakeOnChildDoneSkipsWhenPendingAndMailboxEmpty(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发 2 个子 Agent，等待回传"),
		blades.AssistantMessage("已整合子 Agent 结果，任务完成"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)
	svc.mailbox = mailbox.New()

	// 中间完成：pending>0 且邮箱空 → 保持挂起，不烧轮次。
	svc.WakeOnChildDone(sessionID)
	time.Sleep(300 * time.Millisecond)
	snap, _ := svc.Get(context.Background(), sessionID)
	if snap.Status != string(enums.SessionStatusAwaitingChild) {
		t.Fatalf("status=%s after mid-wave completion, want still awaiting_child", snap.Status)
	}
	if got := llm.callCount(); got != 1 {
		t.Fatalf("llm calls=%d after skipped wake, want 1", got)
	}

	// 最后一个完成：pending==0 → 必醒并完成。
	pending.n.Store(0)
	svc.WakeOnChildDone(sessionID)
	waitForStatus(t, svc, sessionID, enums.SessionStatusCompleted, "completed")
}

// 智能唤醒：pending>0 但邮箱有未读（回传摘要/审批请求/直问）时仍唤醒——
// 有内容可消化，不是空转轮。
func TestWakeOnChildDoneWakesWhenMailboxHasUnread(t *testing.T) {
	llm := &recordingSuspendProvider{responses: []*blades.Message{
		blades.AssistantMessage("已派发 2 个子 Agent，等待回传"),
		blades.AssistantMessage("已消化邮箱回传，继续等其余子 Agent"),
	}}
	pending := &stubPendingChildren{}
	pending.n.Store(1)
	svc, sessionID := newAwaitingChildService(t, llm, pending)
	mb := mailbox.New()
	svc.mailbox = mb

	// 邮箱投一条未读消息（模拟子 Agent 回传摘要）。
	if _, err := mb.Send(&mailbox.Message{
		From: "s1/domain-1", To: sessionID, Type: mailbox.MsgInfo,
		Subject: "子 Agent 完成", Body: "回传摘要",
	}); err != nil {
		t.Fatalf("mailbox send: %v", err)
	}

	svc.WakeOnChildDone(sessionID)

	// 唤醒后第二轮 LLM 已跑（邮箱有内容即醒，非空转）。
	deadline := time.Now().Add(5 * time.Second)
	for llm.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := llm.callCount(); got != 2 {
		t.Fatalf("llm calls=%d after wake with unread mail, want 2", got)
	}
}
