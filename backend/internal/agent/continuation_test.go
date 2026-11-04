package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

func contSvc(mode string, max, noProgress int) *ReactService {
	s := &ReactService{store: newReactSessionStore(nil)}
	s.SetAutoResume(mode, max, noProgress)
	return s
}

func contResult() ReactResult {
	out, _ := json.Marshal(ToolResult{Tool: "RunCommand", Success: true})
	return ReactResult{
		LimitReached: true, PauseCause: "context_threshold",
		History: []ReactMessage{
			{Role: "assistant", Content: "继续处理"},
			{Role: "tool", ToolCallID: "c1", Content: string(out)},
		},
	}
}

// TestSessionAutoResumeShadowOnly shadow 只记录，不接管暂停，不消耗预算。
func TestSessionAutoResumeShadowOnly(t *testing.T) {
	s := contSvc(autoResumeShadow, 2, 1)
	sess := &reactInternalSession{ID: "s1", Status: enums.SessionStatusRunning}
	if s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("shadow must not take over pause")
	}
	if got := s.sessionContinuationBudget("s1").load(); got != 0 {
		t.Fatalf("shadow must not consume budget: %d", got)
	}
}

// TestSessionAutoResumeBudgetFuse 任务累计次数耗尽后不接管。
func TestSessionAutoResumeBudgetFuse(t *testing.T) {
	s := contSvc(autoResumeEnforce, 1, 1)
	s.sessionContinuationBudget("s1").inc()
	sess := &reactInternalSession{ID: "s1", Status: enums.SessionStatusRunning}
	if s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("budget-exhausted session must not auto-resume")
	}
}

// TestSessionAutoResumeNoProgressFuse 相同进展两次后触发无进展熔断。
func TestSessionAutoResumeNoProgressFuse(t *testing.T) {
	s := contSvc(autoResumeShadow, 10, 1)
	sess := &reactInternalSession{ID: "s1", Status: enums.SessionStatusRunning}
	_ = s.tryScheduleSessionAutoResume(sess, contResult())
	// 同一 digest 第二次：noProgress 达到 1，应阻断（shadow 下仍返回 false，但记录状态）。
	if s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("no-progress must not auto-resume")
	}
	v, _ := s.sessionContinuations.Load("s1")
	st := v.(*sessionContinuationState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.noProgress < 1 {
		t.Fatalf("noProgress = %d, want >=1", st.noProgress)
	}
}

// TestSessionProgressDigest 新证据改变 digest，重复正文不变。
func TestSessionProgressDigest(t *testing.T) {
	a := contResult().History
	b := append([]ReactMessage{}, a...)
	if sessionProgressDigest(a) != sessionProgressDigest(b) {
		t.Fatal("digest must be deterministic")
	}
	withFile := append([]ReactMessage{}, a...)
	out, _ := json.Marshal(ToolResult{Tool: "WriteFile", Success: true})
	withFile = append(withFile, ReactMessage{Role: "tool", ToolCallID: "c2", Content: string(out)})
	if sessionProgressDigest(a) == sessionProgressDigest(withFile) {
		t.Fatal("new successful tool evidence must change digest")
	}
}

// TestSessionAutoResumeSingleFlight 重复触限只扣一次预算，收尾后只允许一个恢复代际。
func TestSessionAutoResumeSingleFlight(t *testing.T) {
	s := contSvc(autoResumeEnforce, 2, 10)
	sess := &reactInternalSession{ID: "s1", Status: enums.SessionStatusRunning, runSeq: 1, stopCtx: context.Background()}
	if !s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("first system pause must be taken over")
	}
	if !s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("duplicate trigger must remain taken over")
	}
	if got := s.sessionContinuationBudget("s1").load(); got != 1 {
		t.Fatalf("duplicate schedule consumed budget: %d", got)
	}
	seq, cause, ok := s.claimScheduledSessionContinuation(sess)
	if !ok || seq != 1 || cause != "context_threshold" {
		t.Fatalf("claim = seq %d cause %s ok %v", seq, cause, ok)
	}
	if sess.Status != enums.SessionStatusRunning || sess.runSeq != 2 || sess.wakeInput == "" {
		t.Fatalf("session not resumed exactly once: status=%s runSeq=%d", sess.Status, sess.runSeq)
	}
	if _, _, ok := s.claimScheduledSessionContinuation(sess); ok {
		t.Fatal("same checkpoint must not resume twice")
	}
}

// TestSessionAutoResumeStaleGenerationFailsClosed 用户已启动新 run 时，旧系统调度必须让位。
func TestSessionAutoResumeStaleGenerationFailsClosed(t *testing.T) {
	s := contSvc(autoResumeEnforce, 2, 10)
	sess := &reactInternalSession{ID: "s1", Status: enums.SessionStatusRunning, runSeq: 1, stopCtx: context.Background()}
	if !s.tryScheduleSessionAutoResume(sess, contResult()) {
		t.Fatal("schedule failed")
	}
	restartSessionContext(sess) // 模拟用户新消息／恢复先启动新代际。
	if _, _, ok := s.claimScheduledSessionContinuation(sess); ok {
		t.Fatal("stale system continuation must be fenced")
	}
	if sess.runSeq != 2 {
		t.Fatalf("stale continuation changed run generation: %d", sess.runSeq)
	}
}
