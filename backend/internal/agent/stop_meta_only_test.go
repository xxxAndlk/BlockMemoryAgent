package agent

// stop_meta_only_test.go 「只停主 Agent」与级联暂停（2026-10-16 统一停止按钮）：
//   - StopMetaOnly 只取消 Meta 的 run ctx：stopCtx 存活（子 Agent 继续跑）、
//     不武装销毁倒计时、不置软停止标记；
//   - metaStopSeq 按运行代际消费：一次停止只消费一次，陈旧请求不得误停新 run；
//   - 停放落态 PauseMetaOnlyPark → awaiting_clarify + 如实文案；
//   - PauseAgent cascade=true 连带子树（先停后代后停本支路），false 只停本支路。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// fakePauseMarker 是 SoftStopMarker + MarkPauseNode/ClearPauseNode/IsSoftStop 的记忆实现。
type fakePauseMarker struct {
	marked map[string]bool
	soft   map[string]bool
}

func (f *fakePauseMarker) SetSoftStop(sessionID string)   { f.soft[sessionID] = true }
func (f *fakePauseMarker) ClearSoftStop(sessionID string) { delete(f.soft, sessionID) }
func (f *fakePauseMarker) IsSoftStop(sessionID string) bool {
	return f.soft[sessionID]
}
func (f *fakePauseMarker) MarkPauseNode(id string)  { f.marked[id] = true }
func (f *fakePauseMarker) ClearPauseNode(id string) { delete(f.marked, id) }

// TestReactService_StopMetaOnlyKeepsStopCtxAlive 只停 Meta：本 run 打断、stopCtx 存活、
// 无倒计时、无软停止标记，只登记 metaStopSeq。
func TestReactService_StopMetaOnlyKeepsStopCtxAlive(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("只停主 Agent 测试", "")

	if err := svc.StopMetaOnly(context.Background(), sess.ID); err != nil {
		t.Fatalf("StopMetaOnly: %v", err)
	}
	// Meta 的 run ctx 被取消（本 run 打断）。
	select {
	case <-sess.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("StopMetaOnly must cancel session ctx (Meta run)")
	}
	// stopCtx 不受影响：在飞子 Agent 继续执行。
	select {
	case <-sess.stopCtx.Done():
		t.Fatal("StopMetaOnly must NOT cancel stopCtx (children keep running)")
	default:
	}
	svc.store.mu.RLock()
	destroyAt, stopTimer, stopSeq := sess.destroyAt, sess.stopTimer, sess.metaStopSeq
	svc.store.mu.RUnlock()
	if destroyAt != nil || stopTimer != nil {
		t.Fatal("StopMetaOnly must not arm destroy countdown")
	}
	if stopSeq != sess.runSeq {
		t.Fatalf("metaStopSeq=%d want runSeq=%d", stopSeq, sess.runSeq)
	}
}

// TestStopMetaOnlyRejections 入参/状态校验：会话不存在 / 非 Running / 全停窗口互斥。
func TestStopMetaOnlyRejections(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	ctx := context.Background()

	if err := svc.StopMetaOnly(ctx, "session-none"); err == nil {
		t.Fatal("unknown session must be rejected")
	}
	// 非 Running（暂停停放态）拒绝——awaiting_clarify 若由子 Agent 提问挂起，
	// cancelFn 解除不了，该态必须走全停。
	sess := svc.store.createSession("停放拒停测试", "")
	svc.pauseSession(sess, nil, PauseIterationLimit)
	if err := svc.StopMetaOnly(ctx, sess.ID); err == nil {
		t.Fatal("non-running session must be rejected")
	}
	// 软停止窗口互斥：destroyAt 非空时拒绝（两套落定分支不得同 run 竞争）。
	sess2 := svc.store.createSession("窗口互斥测试", "")
	deadline := time.Now().Add(time.Minute)
	sess2.destroyAt = &deadline
	if err := svc.StopMetaOnly(ctx, sess2.ID); err == nil {
		t.Fatal("session in soft-stop window must be rejected")
	}
}

// TestConsumeMetaOnlyStopByRunSeq 代际消费语义：本 run 消费一次即清；
// 陈旧代际与非法状态不命中。
func TestConsumeMetaOnlyStopByRunSeq(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("代际消费测试", "")

	// 无请求：不消费。
	if svc.consumeMetaOnlyStop(sess, sess.runSeq) {
		t.Fatal("无停止请求不应消费")
	}
	// 发起停止：本 run 消费一次即清。
	if err := svc.StopMetaOnly(context.Background(), sess.ID); err != nil {
		t.Fatalf("StopMetaOnly: %v", err)
	}
	if !svc.consumeMetaOnlyStop(sess, sess.runSeq) {
		t.Fatal("停止请求应被本 run 消费")
	}
	if svc.consumeMetaOnlyStop(sess, sess.runSeq) {
		t.Fatal("一次停止只应消费一次")
	}
	// 陈旧代际（请求属于旧 run）：不命中且标记被清。
	svc.store.mu.Lock()
	sess.metaStopSeq = 5
	svc.store.mu.Unlock()
	if svc.consumeMetaOnlyStop(sess, 3) {
		t.Fatal("代际不符的陈旧请求不得命中")
	}
	svc.store.mu.RLock()
	stale := sess.metaStopSeq
	svc.store.mu.RUnlock()
	if stale != 0 {
		t.Fatalf("陈旧标记应被清: %d", stale)
	}
	// 非法状态（error 终态）不落停放。
	svc.store.mu.Lock()
	sess.metaStopSeq = sess.runSeq
	sess.Status = enums.SessionStatusError
	svc.store.mu.Unlock()
	if svc.consumeMetaOnlyStop(sess, sess.runSeq) {
		t.Fatal("error 终态不得被消费成停放")
	}
}

// TestPauseMetaOnlyParkLanding 停放落态：awaiting_clarify + 如实文案（不得提销毁倒计时）。
func TestPauseMetaOnlyParkLanding(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("停放落态测试", "")

	svc.pauseSession(sess, nil, PauseMetaOnlyPark)
	if sess.Status != enums.SessionStatusAwaitingClarify {
		t.Fatalf("status=%v want awaiting_clarify", sess.Status)
	}
	if !strings.Contains(sess.Result, "已停止主 Agent") {
		t.Fatalf("Result=%q 应含「已停止主 Agent」", sess.Result)
	}
	if strings.Contains(sess.Result, "销毁") {
		t.Fatalf("Result=%q 不得提销毁倒计时（未武装）", sess.Result)
	}
}

// TestPauseSessionClearsLiveText 回归（2026-10-17 用户实证：停止会话后再发送，页面复活
// 上轮回答文字）：停放落定必须与 StreamingText/ThinkingText 清零同窗原子——窗口期里
// SSE tick 能看到停放态但旧正文未清，live 帧会把旧文字重推给前端。
func TestPauseSessionClearsLiveText(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("停放清正文测试", "")
	svc.store.setStreamingText(sess, "上一轮流式正文残留")
	svc.store.setThinkingText(sess, "上一轮思考残留")

	svc.pauseSession(sess, nil, PauseUserStop)

	svc.store.mu.RLock()
	st, th, status := sess.StreamingText, sess.ThinkingText, sess.Status
	svc.store.mu.RUnlock()
	if st != "" || th != "" {
		t.Fatalf("停放必须同窗清空瞬时文本: stream=%q think=%q", st, th)
	}
	if status != enums.SessionStatusAwaitingClarify {
		t.Fatalf("停放态错误: %v", status)
	}
}

// TestPauseAgentCascadePausesDescendants 级联暂停：cascade=true 标记并取消整棵子树
// （先后代后本支路），cascade=false 只动本支路；无关支路不受影响。
func TestPauseAgentCascadePausesDescendants(t *testing.T) {
	newEnv := func(t *testing.T) (*ReactService, *fakePauseMarker, *orchestrator.Tree, string, map[string]bool) {
		t.Helper()
		svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
		sess := svc.store.createSession("级联暂停测试", "")
		marker := &fakePauseMarker{marked: map[string]bool{}, soft: map[string]bool{}}
		svc.SetSoftStopMarker(marker)
		tr := svc.TreeFor(sess.ID)
		cancelled := map[string]bool{}
		reg := func(id, parent, role string) {
			tr.Register(orchestrator.Node{ID: id, ParentID: parent, Role: role, Status: orchestrator.StatusRunning})
			tr.SetCancel(id, func() { cancelled[id] = true })
		}
		root := sess.ID + "/domain-1"
		reg(root, sess.ID, "domain")
		reg(root+"/leaf-1", root, "code_assistant")
		reg(root+"/leaf-1/grand-1", root+"/leaf-1", "code_assistant")
		reg(sess.ID+"/domain-2", sess.ID, "domain")
		return svc, marker, tr, root, cancelled
	}
	ctx := context.Background()

	t.Run("branch_only", func(t *testing.T) {
		svc, marker, _, root, cancelled := newEnv(t)
		if err := svc.PauseAgent(ctx, svc.store.sessionIDs()[0], root, false); err != nil {
			t.Fatalf("PauseAgent(false): %v", err)
		}
		if !marker.marked[root] || !cancelled[root] {
			t.Fatalf("非级联应标记并取消本支路: marked=%v cancelled=%v", marker.marked, cancelled)
		}
		if marker.marked[root+"/leaf-1"] || marker.marked[root+"/leaf-1/grand-1"] {
			t.Fatalf("非级联不得波及后代: %v", marker.marked)
		}
	})

	t.Run("cascade_subtree", func(t *testing.T) {
		svc, marker, _, root, cancelled := newEnv(t)
		if err := svc.PauseAgent(ctx, svc.store.sessionIDs()[0], root, true); err != nil {
			t.Fatalf("PauseAgent(true): %v", err)
		}
		for _, id := range []string{root, root + "/leaf-1", root + "/leaf-1/grand-1"} {
			if !marker.marked[id] || !cancelled[id] {
				t.Fatalf("级联应标记并取消 %s: marked=%v cancelled=%v", id, marker.marked, cancelled)
			}
		}
		if marker.marked[svc.store.sessionIDs()[0]+"/domain-2"] {
			t.Fatalf("无关支路不得受影响: %v", marker.marked)
		}
	})
}
