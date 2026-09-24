package subagent

// handover_test.go 测试结构化遗产清单：【遗产清单】结构化段组装。

import (
	"context"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
)

func TestRenderLegacyList(t *testing.T) {
	d := &Dispatcher{}
	// 构造看板：domain-2 两条在办步骤。
	bd := board.NewTaskBoard("sess-1", "g")
	id1 := bd.AddSubTask("搭骨架")
	id2 := bd.AddSubTask("写逻辑")
	_ = bd.SetPlan("", []board.PlanTask{
		{ID: id1, Title: "搭骨架", Domain: "domain-2"},
		{ID: id2, Title: "写逻辑", Domain: "domain-2"},
	})
	_ = bd.Assign(id1, "sub-1")
	d.boardFn = func(string) *board.TaskBoard { return bd }

	// 历史含成功写入 + 失败写入（只列成功）。
	hist := []agent.ReactMessage{
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "w1", Name: "WriteFile", Input: map[string]any{"path": "index.html"}}}},
		{Role: "tool", ToolCallID: "w1", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "WriteFile", Success: true})},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "w2", Name: "WriteFile", Input: map[string]any{"path": "game.js"}}}},
		{Role: "tool", ToolCallID: "w2", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "WriteFile", Success: true})},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "w3", Name: "WriteFile", Input: map[string]any{"path": "broken.js"}}}},
		{Role: "tool", ToolCallID: "w3", Content: agent.ToolResultJSON(agent.ToolResult{Tool: "WriteFile", Success: false, Error: "denied"})},
	}
	legacy := d.renderLegacyList(context.Background(), "sess-1/meta-1", "domain-2", agent.ReactResult{History: hist})
	if legacy == "" {
		t.Fatal("legacy list should not be empty")
	}
	for _, want := range []string{"【遗产清单】", "index.html", "game.js", "搭骨架", "写逻辑", "spec 骨架"} {
		if !strings.Contains(legacy, want) {
			t.Fatalf("legacy missing %q: %q", want, legacy)
		}
	}
	if strings.Contains(legacy, "broken.js") {
		t.Fatal("failed write must not be listed")
	}

	// 无写入且无在办步骤 -> 空段。
	d2 := &Dispatcher{}
	if got := d2.renderLegacyList(context.Background(), "p", "ui", agent.ReactResult{}); got != "" {
		t.Fatalf("empty legacy should return empty string, got %q", got)
	}
}
