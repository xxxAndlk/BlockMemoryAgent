package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
