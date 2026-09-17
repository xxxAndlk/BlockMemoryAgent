package subagent

// plan_test.go 验证 TODO #22 真·执行计划：
//   - write_plan 工具：写入/校验计划（环依赖/未知依赖拒绝），全量覆盖保留既有状态；
//   - 派发依赖门：depends_on 未完成拒绝派发，完成后续派放行；
//   - 完成/失败回写看板状态。

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// newPlanTestEnv 构造带 board 接线的 Dispatcher（write_plan 工具注册）。
func newPlanTestEnv(t *testing.T, provider agent.ModelProvider) (*Dispatcher, *mailbox.Mailbox, *board.Manager, *tool.Registry) {
	t.Helper()
	d, mb, _, toolsReg, _ := newSalvageTestEnv(t, provider)
	bm := board.NewManager()
	d.WithBoard(bm.Get, func(sid, goal string) *board.TaskBoard { return bm.GetOrCreate(sid, goal) })
	d.RegisterPlanTool(toolsReg)
	return d, mb, bm, toolsReg
}

// TestWritePlanTool_RoundTrip write_plan 写入 + 校验拒绝（环依赖）。
func TestWritePlanTool_RoundTrip(t *testing.T) {
	d, _, bm, toolsReg := newPlanTestEnv(t, &mockProvider{text: "ok"})
	_ = d

	res, err := toolsReg.Dispatch(dispatchCtx(), "write_plan", map[string]any{
		"goal": "实现塔防游戏",
		"tasks": []any{
			map[string]any{"id": "render", "title": "渲染引擎", "domain": "渲染", "acceptance": []any{"fps>30"}},
			map[string]any{"id": "config", "title": "配置模块", "domain": "配置", "depends_on": []any{"render"}},
		},
	})
	if err != nil || !res.Success {
		t.Fatalf("write_plan failed: err=%v res=%+v", err, res)
	}
	b := bm.Get("s1")
	if b == nil {
		t.Fatal("board not created")
	}
	snap := b.Snapshot()
	if len(snap.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(snap.Tasks))
	}
	if id, ok := b.FindByDomain("渲染"); !ok || id != "render" {
		t.Fatalf("FindByDomain mismatch: %q %v", id, ok)
	}
	if b.DependsDone("config") {
		t.Fatal("config depends on render (pending), DependsDone should be false")
	}

	// 环依赖拒绝。
	cycleRes, err := toolsReg.Dispatch(dispatchCtx(), "write_plan", map[string]any{
		"tasks": []any{
			map[string]any{"id": "x", "title": "X", "depends_on": []any{"y"}},
			map[string]any{"id": "y", "title": "Y", "depends_on": []any{"x"}},
		},
	})
	if err != nil {
		t.Fatalf("dispatch should not error: %v", err)
	}
	if cycleRes.Success || !strings.Contains(cycleRes.Error, "环依赖") && !strings.Contains(cycleRes.Error, "cycle") {
		t.Fatalf("cycle plan should be rejected, got: %+v", cycleRes)
	}
}

// TestDispatchDepGate 依赖门：依赖未完成拒绝派发；完成后放行。
func TestDispatchDepGate(t *testing.T) {
	_, _, bm, toolsReg := newPlanTestEnv(t, &mockProvider{text: "done"})
	_, err := toolsReg.Dispatch(dispatchCtx(), "write_plan", map[string]any{
		"goal": "实现塔防游戏",
		"tasks": []any{
			map[string]any{"id": "render", "title": "渲染引擎", "domain": "渲染"},
			map[string]any{"id": "config", "title": "配置模块", "domain": "配置", "depends_on": []any{"render"}},
		},
	})
	if err != nil {
		t.Fatalf("write_plan: %v", err)
	}

	// 依赖未完成：拒绝派发配置领域。
	blocked, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "配置",
		"task":           "实现配置",
		"responsibility": "负责配置",
	})
	if err != nil {
		t.Fatalf("dispatch should not error: %v", err)
	}
	if blocked.Success || !strings.Contains(blocked.Error, "依赖未满足") {
		t.Fatalf("dep gate should reject, got: %+v", blocked)
	}

	// 无依赖领域：放行。
	ok, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "渲染",
		"task":           "实现渲染",
		"responsibility": "负责渲染",
	})
	if err != nil || !ok.Success {
		t.Fatalf("independent domain should dispatch, err=%v res=%+v", err, ok)
	}

	// 等渲染领域完成（异步，mock 立即完成）-> 看板回写 Done -> 配置领域放行。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b := bm.Get("s1")
		if b != nil {
			if id, found := b.FindByDomain("渲染"); found {
				snap := b.Snapshot()
				for _, tk := range snap.Tasks {
					if tk.ID == id && tk.Status == board.TaskDone {
						goto depCleared
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("render task never marked Done in board")
depCleared:
	ok2, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "配置",
		"task":           "实现配置",
		"responsibility": "负责配置",
	})
	if err != nil || !ok2.Success {
		t.Fatalf("dispatch after dep cleared should succeed, err=%v res=%+v", err, ok2)
	}
}

// TestDepGateReadyNotify 依赖门就绪通知（轻量排队）：拒派登记 waiter；前置任务
// 全 done 后 boardUpdate 触发邮箱 MsgInfo（From=system）+ 唤醒回调；依赖未完成不通知。
func TestDepGateReadyNotify(t *testing.T) {
	d, mb, bm, toolsReg := newPlanTestEnv(t, &mockProvider{text: "done"})
	wake := make(chan string, 2)
	d.WithSessionWake(func(parentID, hint string) bool {
		wake <- parentID + "|" + hint
		return true
	})
	_, err := toolsReg.Dispatch(dispatchCtx(), "write_plan", map[string]any{
		"goal": "实现塔防游戏",
		"tasks": []any{
			map[string]any{"id": "render", "title": "渲染引擎", "domain": "渲染"},
			map[string]any{"id": "config", "title": "配置模块", "domain": "配置", "depends_on": []any{"render"}},
		},
	})
	if err != nil {
		t.Fatalf("write_plan: %v", err)
	}

	// 依赖未完成：拒派配置 + 登记 waiter + 文案含自动通知提示。
	blocked, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "domain", "domain": "配置", "task": "实现配置", "responsibility": "负责配置",
	})
	if err != nil {
		t.Fatalf("dispatch should not error: %v", err)
	}
	if blocked.Success || !strings.Contains(blocked.Error, "依赖未满足") || !strings.Contains(blocked.Error, "自动通知") {
		t.Fatalf("dep gate rejection missing notify hint: %+v", blocked)
	}

	// 派渲染（无依赖放行），异步完成回写 render=Done → 配置 waiter 依赖全就绪，
	// 应收到 From=system 的就绪通知 + 唤醒回调。
	if _, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id": "domain", "domain": "渲染", "task": "实现渲染", "responsibility": "负责渲染",
	}); err != nil {
		t.Fatalf("render dispatch: %v", err)
	}
	var gotNotify bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !gotNotify {
		for _, m := range mb.Peek("s1") {
			if m.From == "system" && strings.Contains(m.Body, "依赖就绪") && strings.Contains(m.Body, "配置") {
				gotNotify = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotNotify {
		t.Fatal("dep-ready notify not delivered to parent mailbox after render done")
	}
	select {
	case c := <-wake:
		if !strings.Contains(c, "s1|") || !strings.Contains(c, "依赖就绪") {
			t.Fatalf("wake unexpected: %s", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session wake not invoked on dep-ready notify")
	}

	// waiter 已删：重复 boardUpdate 不重复通知（数量仍为 1）。
	_ = bm.Get("s1").MarkDone("config", "done")
	d.notifyDepWaiters(dispatchCtx(), "s1")
	count := 0
	for _, m := range mb.Peek("s1") {
		if m.From == "system" && strings.Contains(m.Body, "依赖就绪") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dep-ready notify should fire once, got %d", count)
	}
}

// TestDispatchNoBoard_ZeroChange 无看板接线/无计划时派发零行为变化。
func TestDispatchNoBoard_ZeroChange(t *testing.T) {
	d, _, _, toolsReg, _ := newSalvageTestEnv(t, &mockProvider{text: "done"})
	_ = d
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"domain":         "配置",
		"task":           "实现配置",
		"responsibility": "负责配置",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch without board should be unaffected, err=%v res=%+v", err, res)
	}
}
