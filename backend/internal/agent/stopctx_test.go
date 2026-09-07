package agent

// stopctx_test.go 验证会话级中断传播（TODO 第10④）：
//   - createSession/restartSessionContext 同建/重建 stopCtx；
//   - Stop（软停）与 cancel（硬取消）取消 stopCtx——stop 窗口期新派发/深层孙代随会话终止；
//   - pauseSession（token/轮数触顶）不触碰 stopCtx——暂停可恢复、热驻 Idle 保留复用。

import (
	"context"
	"testing"
	"time"
)

// TestReactService_StopCancelsStopCtx 软停止取消 stopCtx；续跑前 restartSessionContext 重建新基底。
func TestReactService_StopCancelsStopCtx(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("停传测试", "")

	if sess.stopCtx == nil || sess.stopCancel == nil {
		t.Fatal("createSession should build stopCtx/stopCancel")
	}
	select {
	case <-sess.stopCtx.Done():
		t.Fatal("fresh stopCtx must not be canceled")
	default:
	}

	// 软停止：stopCtx 取消（stop 窗口期新派发即刻终止）。
	if err := svc.Stop(context.Background(), sess.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-sess.stopCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Stop must cancel session stopCtx")
	}

	// 续跑路径：restartSessionContext 重建全新 stopCtx（旧值已取消不能沿用，否则恢复后新派发秒死）。
	restartSessionContext(sess)
	select {
	case <-sess.stopCtx.Done():
		t.Fatal("restarted stopCtx must be alive after restartSessionContext")
	default:
	}
}

// TestReactService_CancelCancelsStopCtx 硬取消同步取消 stopCtx。
func TestReactService_CancelCancelsStopCtx(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("取消测试", "")

	if err := svc.cancel(context.Background(), sess.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-sess.stopCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("cancel must cancel session stopCtx")
	}
}

// TestReactService_PauseKeepsStopCtxAlive pauseSession（token/轮数触顶）不动 stopCtx：
// 暂停态在飞子 Agent 不被误杀，恢复续跑沿用同一 stopCtx 基底。
func TestReactService_PauseKeepsStopCtxAlive(t *testing.T) {
	svc := newReactServiceForTest(&mockReactModelProvider{}, t.TempDir())
	sess := svc.store.createSession("暂停测试", "")

	svc.pauseSession(sess, nil, PauseIterationLimit)

	select {
	case <-sess.stopCtx.Done():
		t.Fatal("pauseSession must NOT cancel stopCtx (pause does not propagate)")
	default:
	}
}
