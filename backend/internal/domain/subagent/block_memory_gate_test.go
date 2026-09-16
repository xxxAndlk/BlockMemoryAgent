package subagent

// block_memory_gate_test.go 覆盖块记忆沉淀触发门（2026-09-16 用户定向
// "只沉淀改动的关键逻辑与信息"）：纯只读/检查任务不落库，有写入或未知工具即放行。

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
)

func histWithTools(names ...string) []agent.ReactMessage {
	tcs := make([]agent.ToolCall, 0, len(names))
	for _, n := range names {
		tcs = append(tcs, agent.ToolCall{Name: n})
	}
	return []agent.ReactMessage{{Role: "assistant", ToolCalls: tcs}}
}

func TestHasSubstantiveChange(t *testing.T) {
	cases := []struct {
		name     string
		history  []agent.ReactMessage
		files    []string
		expected bool
	}{
		{"有文件改动轨迹", histWithTools("ReadFile"), []string{"a.js"}, true},
		{"纯只读检查", histWithTools("ReadFile", "SearchInFiles", "GitStatus"), nil, false},
		{"未知工具按有产出（插件/MCP）", histWithTools("od_image_generate"), nil, true},
		{"只读+问答", histWithTools("ReadFile", "ask_user", "send_message", "ListDir"), nil, false},
		{"无任何工具", nil, nil, false},
		{"写共享记忆算产出", histWithTools("WriteSharedMemory"), nil, true},
		{"RunCommand 算产出（可能写文件）", histWithTools("RunCommand"), nil, true},
		{"派发子 Agent 算产出", histWithTools("call_sub_agent"), nil, true},
	}
	for _, c := range cases {
		if got := hasSubstantiveChange(c.history, c.files); got != c.expected {
			t.Errorf("%s: hasSubstantiveChange = %v, want %v", c.name, got, c.expected)
		}
	}
}
