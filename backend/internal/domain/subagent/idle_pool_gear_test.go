package subagent

// idle_pool_gear_test.go 热驻槽档位守卫测试（TODO #14 T22）：
// enterIdle 固化会话档位；resolveIdleSiblingReuse 档位不符跳过（不隐式接管旧档
// 上下文）；槽 gear 为空 / 回调未接线 / 当前档位未知均按匹配放行（防误杀存量槽）。

import (
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// dispatchHotDomainTask 派发一个金融领域子任务并等它回传进 Idle，返回槽 agent_id。
func dispatchHotDomainTask(t *testing.T, toolsReg *tool.Registry, tr *orchestrator.Tree) string {
	t.Helper()
	res, err := toolsReg.Dispatch(dispatchCtx(), "call_sub_agent", map[string]any{
		"role_id":        "domain",
		"task":           "做某事",
		"domain":         "金融",
		"responsibility": "负责金融模块",
	})
	if err != nil || !res.Success {
		t.Fatalf("dispatch failed: err=%v res=%+v", err, res)
	}
	subID := subAgentIDOf(res)
	waitForCond(t, "tree idle", func() bool {
		n, ok := tr.Get(subID)
		return ok && n.Status == orchestrator.StatusIdle
	})
	return subID
}

func TestHotDomainSlotGearGuard(t *testing.T) {
	gears := map[string]string{"s1": "cluster"}
	provider := &scriptProvider{lines: []string{"domain result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	d.WithSessionGearResolver(func(sessionID string) string { return gears[sessionID] })

	subID := dispatchHotDomainTask(t, toolsReg, tr)

	slot := d.pool.slot("s1", subID)
	if slot == nil {
		t.Fatal("slot should be hot-resident after task done")
	}
	if got := slot.gearOf(); got != "cluster" {
		t.Fatalf("slot gear = %q, want cluster（enterIdle 固化）", got)
	}

	ctx := dispatchCtx()
	// 档位一致 → 命中隐式复用。
	if got := d.resolveIdleSiblingReuse(ctx, "s1", "金融"); got != subID {
		t.Fatalf("gear match: reuse = %q, want %q", got, subID)
	}
	// 档位不符（会话已切快速档）→ 跳过，不隐式接管旧档上下文。
	gears["s1"] = "fast"
	if got := d.resolveIdleSiblingReuse(ctx, "s1", "金融"); got != "" {
		t.Fatalf("gear mismatch: reuse = %q, want 空", got)
	}
	// 当前档位未知（回调返回空，如会话不存在）→ 放行防误杀。
	gears["s1"] = ""
	if got := d.resolveIdleSiblingReuse(ctx, "s1", "金融"); got != subID {
		t.Fatalf("empty current gear: reuse = %q, want %q", got, subID)
	}
}

func TestHotDomainSlotGearUnwired(t *testing.T) {
	provider := &scriptProvider{lines: []string{"domain result"}}
	d, _, tr, toolsReg := newIdleTestEnv(t, provider, time.Hour)
	// 不接 WithSessionGearResolver：槽 gear 空 + 守卫放行（存量槽/未接线兼容）。

	subID := dispatchHotDomainTask(t, toolsReg, tr)

	slot := d.pool.slot("s1", subID)
	if slot == nil {
		t.Fatal("slot should be hot-resident after task done")
	}
	if got := slot.gearOf(); got != "" {
		t.Fatalf("unwired slot gear = %q, want 空", got)
	}
	if got := d.resolveIdleSiblingReuse(dispatchCtx(), "s1", "金融"); got != subID {
		t.Fatalf("unwired callback: reuse = %q, want %q", got, subID)
	}
}
