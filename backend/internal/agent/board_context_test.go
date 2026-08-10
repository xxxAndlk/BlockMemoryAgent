package agent

// board_context_test.go 验证 TODO #35 Phase 0 任务看板注入：
// MetaAgent 每轮上下文末尾追加【任务看板】段；未接线/看板未创建零变化。

import (
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestWrapMetaMemory_AppendsBoard 看板已接线且已创建：Assemble 输出末尾追加看板段。
func TestWrapMetaMemory_AppendsBoard(t *testing.T) {
	b := board.NewTaskBoard("s1", "做塔防")
	if err := b.SetPlan("做塔防", []board.PlanTask{{ID: "t1", Title: "自检", Domain: "自检", Acceptance: []string{"a"}}}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	_ = b.MarkFailed("t1", "loop guard 三连败终止")

	p := wrapMetaMemory(NopMemoryPipeline{}, func(string) *board.TaskBoard { return b }, "s1")
	out := p.Assemble(types.RoleDefinition{}, "s1", []ReactMessage{{Role: "user", Content: "hi"}})
	if len(out) != 2 {
		t.Fatalf("expected 2 messages (history + board), got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != "system" || !strings.Contains(last.Content, "任务看板") {
		t.Fatalf("expected board message at end, got role=%s content=%q", last.Role, last.Content)
	}
	if !strings.Contains(last.Content, "自检") || !strings.Contains(last.Content, "failed") {
		t.Fatalf("board message should carry task status, got: %q", last.Content)
	}
}

// TestWrapMetaMemory_BoardNotCreated 看板未创建（write_plan 前）：零变化不注入。
func TestWrapMetaMemory_BoardNotCreated(t *testing.T) {
	p := wrapMetaMemory(NopMemoryPipeline{}, func(string) *board.TaskBoard { return nil }, "s1")
	out := p.Assemble(types.RoleDefinition{}, "s1", []ReactMessage{{Role: "user", Content: "hi"}})
	if len(out) != 1 {
		t.Fatalf("board not created should not inject, got %d messages", len(out))
	}
}

// TestWrapMetaMemory_NoBoardFn 未接线 boardFn：返回原流水线（零行为变化）。
func TestWrapMetaMemory_NoBoardFn(t *testing.T) {
	p := wrapMetaMemory(NopMemoryPipeline{}, nil, "s1")
	if _, ok := p.(NopMemoryPipeline); !ok {
		t.Fatalf("no boardFn should return inner pipeline unchanged, got %T", p)
	}
}

// TestWrapMetaMemory_WriteDelegates Write 委托内层。
func TestWrapMetaMemory_WriteDelegates(t *testing.T) {
	b := board.NewTaskBoard("s1", "g")
	p := wrapMetaMemory(NopMemoryPipeline{}, func(string) *board.TaskBoard { return b }, "s1")
	if err := p.Write("s1", MemoryEvent{Type: "answer", AgentID: "s1", Content: "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
}
