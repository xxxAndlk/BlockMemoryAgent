package subagent

// stopctx_test.go 验证中断传播的消费端（TODO 第10④）：
//   - dispatchOne 子 ctx 以父 ctx 携带的会话 stopCtx 为取消基底（缺省回退 Background）；
//   - 热驻 domain 槽任务 ctx 同以 stopCtx 为基底，会话 Stop 即刻取消执行中任务
//   （槽销毁走既有 Canceled 分支收尾）。

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestDispatchOne_StopCtxCancelsChild 端到端：父 ctx 携带 stopCtx -> 派发叶子
//（provider 尊重 ctx 取消）-> cancel stopCtx -> 叶子 ctx 取消退出、父未决计数归零
//（会话停止即全树终止，不再依赖树快照逐节点 StopRunning）。
func TestDispatchOne_StopCtxCancelsChild(t *testing.T) {
	d, _, _, toolsReg, _ := newSalvageTestEnv(t, &ctxCancelProvider{})
	d.RegisterControlTool(toolsReg)

	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	ctx := agent.WithAgentID(
		tool.WithStopContext(tool.WithSessionID(context.Background(), "s1"), stopCtx), "meta")

	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id": "code_assistant",
		"task":    "长任务",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}

	// 等叶子进入挂起的 LLM 调用后模拟会话 Stop。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e := d.activityEvidenceFor(res.Output); e != nil && e.llmInFlight.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCancel()

	// 叶子 ctx 取消退出：父未决计数归零 + activity 清理（5s 内，远小于任何墙钟）。
	// 注：ctx.Canceled 路径 runSubAgent 不向父重复 notify（取消方负责告知），故不查邮箱。
	dl := time.Now().Add(5 * time.Second)
	for time.Now().Before(dl) {
		if d.PendingChildren("meta") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d.PendingChildren("meta") != 0 {
		t.Fatalf("stopCtx cancel should terminate dispatched child, pending=%d", d.PendingChildren("meta"))
	}
	if _, ok := d.activity.Load(res.Output); ok {
		t.Fatal("activity entry should be cleaned after child termination")
	}
}

// TestDispatchHotDomain_StopCtxDestroysSlot 热驻 domain：stopCtx 取消传导到任务 ctx
//（slot.stopCtx 基底），引擎经 Canceled 分支返回 domainTaskDestroyed，槽被销毁。
func TestDispatchHotDomain_StopCtxDestroysSlot(t *testing.T) {
	d, _, _, toolsReg := newIdleTestEnv(t, &ctxCancelProvider{}, time.Hour)

	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	ctx := tool.WithStopContext(tool.WithSessionID(dispatchCtx(), "s1"), stopCtx)

	res, err := toolsReg.Dispatch(ctx, "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "挂起任务",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := res.Output

	// 等任务进入挂起的引擎 LLM 调用后模拟会话 Stop。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e := d.activityEvidenceFor(subID); e != nil && e.llmInFlight.Load() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCancel()

	// Canceled 分支销毁槽：会话停止即刻终止热驻执行中任务。
	dl := time.Now().Add(5 * time.Second)
	for time.Now().Before(dl) {
		if d.pool.slot("s1", subID) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := d.pool.slot("s1", subID); s != nil {
		t.Fatal("stopCtx cancel should destroy the running hot-domain slot")
	}
}
