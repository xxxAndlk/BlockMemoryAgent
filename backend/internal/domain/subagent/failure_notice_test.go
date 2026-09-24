package subagent

// failure_notice_test.go 批二失败收口合并的锁定测试（TODO #24 批二②）：
// 状态三态映射共用，以及热驻失败回传相对 runSubAgent 的有意差异（防未来"顺手统一"回改）。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// TestFailureStatusTri 三态映射：缺验证证据→黄/未验证，其余 kind→红/失败。
func TestFailureStatusTri(t *testing.T) {
	for _, kind := range []FailureKind{FailureKindVerifyMissing, FailureKindUnverified} {
		if b, ts := failureStatusTri(kind); b != board.TaskUnverified || ts != orchestrator.StatusUnverified {
			t.Fatalf("kind=%s should map to unverified, got board=%v tree=%v", kind, b, ts)
		}
	}
	for _, kind := range []FailureKind{FailureKindError, FailureKindKilled, FailureKindTimeout, FailureKindLoopGuard, FailureKindSmokeFailed} {
		if b, ts := failureStatusTri(kind); b != board.TaskFailed || ts != orchestrator.StatusFailed {
			t.Fatalf("kind=%s should map to failed, got board=%v tree=%v", kind, b, ts)
		}
	}
}

// TestNotifyTerminal_OncePerRun 同一 run 终态回传至多一条（TODO #24 顺手修①）：
// 巡检杀与墙钟/失败分支可并发收口，此前父收到 killed/timeout 矛盾双消息；
// meta 缺失（resume/复用重建间隙）退化为直发，不静默吞消息。
func TestNotifyTerminal_OncePerRun(t *testing.T) {
	mb := mailbox.New()
	d := NewDispatcher(nil, nil, nil, mb, nil)
	d.subMeta.Store("s1/domain-1", &subAgentMeta{parentID: "s1", sessionID: "s1"})

	d.notifyTerminal("s1/domain-1", "s1", "第一条 killed", nil)
	d.notifyTerminal("s1/domain-1", "s1", "第二条 timeout", nil)

	msgs := mb.Drain("s1")
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 terminal notify, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Body, "第一条") {
		t.Fatalf("first notify should win, got: %s", msgs[0].Body)
	}

	// meta 缺失：退化为直发（既有语义保留）。
	d.notifyTerminal("s1/domain-9", "s1", "直发消息", nil)
	if msgs := mb.Drain("s1"); len(msgs) != 1 {
		t.Fatalf("meta-less notify should fall back to direct send, got %d", len(msgs))
	}
}

// TestHotResidentFailureNotice 热驻失败回传的有意差异锁定：
// retryable 恒 false；打捞摘要非空时追加、为空不追加标记。
func TestHotResidentFailureNotice(t *testing.T) {
	msg := hotResidentFailureNotice(FailureKindError, "失败文案", "打捞摘要")
	if !strings.Contains(msg, "retryable=false") {
		t.Fatalf("hot-resident failure must be retryable=false, got: %s", msg)
	}
	if !strings.Contains(msg, "失败文案") || !strings.Contains(msg, salvagePrefixMarker) {
		t.Fatalf("failText and salvage marker expected, got: %s", msg)
	}
	if m := hotResidentFailureNotice(FailureKindError, "失败文案", ""); strings.Contains(m, salvagePrefixMarker) {
		t.Fatalf("empty salvage must not append marker, got: %s", m)
	}
	// 缺验证证据信号不附产出全文（与 runSubAgent 的差异：domain 产出全量落看板）。
	m := hotResidentFailureNotice(FailureKindVerifyMissing, "缺验证证据", "")
	if strings.Contains(m, "产出(未验证)") {
		t.Fatalf("hot-resident notice must not embed unverified output, got: %s", m)
	}
}
