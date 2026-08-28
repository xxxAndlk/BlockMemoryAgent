package agent

// task_ledger_context_test.go 验证任务台账注入包装器（2026-08-28 旧需求重派事故根治）：
// MetaAgent 每轮上下文末尾追加【任务台账】段；未接线/无台账零变化。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestWrapMetaMemoryWithLedger_AppendsLedger 台账已接线且有内容：Assemble 输出末尾追加台账段。
func TestWrapMetaMemoryWithLedger_AppendsLedger(t *testing.T) {
	p := wrapMetaMemoryWithLedger(NopMemoryPipeline{}, func(sessionID string) string {
		if sessionID != "s1" {
			return ""
		}
		return "【任务台账】\n#1 [完成] 游戏渲染逻辑 · 任务A（耗时 5m）｜修改: js/game.js"
	}, "s1")
	out := p.Assemble(types.RoleDefinition{}, "s1", []ReactMessage{{Role: "user", Content: "hi"}})
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (history + ledger), got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "任务台账") || !strings.Contains(last.Content, "[完成]") {
		t.Fatalf("expected ledger message at end, got role=%s content=%q", last.Role, last.Content)
	}
}

// TestWrapMetaMemoryWithLedger_EmptySkips 台账渲染空串（新会话未派发）：零变化不注入。
func TestWrapMetaMemoryWithLedger_EmptySkips(t *testing.T) {
	p := wrapMetaMemoryWithLedger(NopMemoryPipeline{}, func(string) string { return "" }, "s1")
	out := p.Assemble(types.RoleDefinition{}, "s1", []ReactMessage{{Role: "user", Content: "hi"}})
	if len(out) != 1 {
		t.Fatalf("empty ledger should not inject, got %d messages", len(out))
	}
}

// TestWrapMetaMemoryWithLedger_NoFn 未接线 ledgerFn：返回原流水线（零行为变化）。
func TestWrapMetaMemoryWithLedger_NoFn(t *testing.T) {
	p := wrapMetaMemoryWithLedger(NopMemoryPipeline{}, nil, "s1")
	if _, ok := p.(NopMemoryPipeline); !ok {
		t.Fatalf("no ledgerFn should return inner pipeline unchanged, got %T", p)
	}
}

// TestWrapMetaMemoryWithLedger_WriteDelegates Write 委托内层。
func TestWrapMetaMemoryWithLedger_WriteDelegates(t *testing.T) {
	p := wrapMetaMemoryWithLedger(NopMemoryPipeline{}, func(string) string { return "x" }, "s1")
	if err := p.Write("s1", MemoryEvent{Type: "answer", AgentID: "s1", Content: "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
}
