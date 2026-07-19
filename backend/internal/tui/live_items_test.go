package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// TestAppendLiveItems 验证运行中会话的实时状态条目：
// 流式文本优先，其次是工具执行中，最后是思考中。
func TestAppendLiveItems(t *testing.T) {
	now := time.Now()

	// 情形一：有流式文本 → 助手条目展示累积文本。
	s := &server.Session{Status: enums.SessionStatusRunning, StreamingText: "部分输出"}
	items := appendLiveItems(nil, s)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].isEvent || items[0].role != enums.ChatRoleAssistant {
		t.Fatalf("流式文本应渲染为助手条目, got isEvent=%v role=%v", items[0].isEvent, items[0].role)
	}
	if !strings.Contains(items[0].title, "部分输出") {
		t.Fatalf("条目标题应包含流式文本, got %q", items[0].title)
	}

	// 情形二：无流式文本且有未完成的工具调用 → 工具执行中。
	s = &server.Session{
		Status: enums.SessionStatusRunning,
		Events: []server.SessionEvent{
			{Type: "tool_call", Kind: "tool_call", Tool: "ReadFile", Timestamp: now},
		},
	}
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "⚙") || !strings.Contains(items[0].title, "ReadFile") {
		t.Fatalf("应展示工具执行中, got %+v", items)
	}

	// 情形三：工具已完成（call 与 exec 配对）→ 回到思考中。
	s.Events = append(s.Events, server.SessionEvent{Type: "tool_exec", Tool: "ReadFile", Success: true, Timestamp: now})
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "⏳") {
		t.Fatalf("应展示思考中, got %+v", items)
	}

	// 情形四：无事件 → 思考中。
	s = &server.Session{Status: enums.SessionStatusRunning}
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "⏳") {
		t.Fatalf("应展示思考中, got %+v", items)
	}

	// 情形五：有思考过程文本 → 暗色 💭 瞬时展示（优先级低于答复流式、高于工具/等待）。
	s = &server.Session{Status: enums.SessionStatusRunning, ThinkingText: "用户在问身份，我应直接回答"}
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "💭") || !strings.Contains(items[0].title, "直接回答") {
		t.Fatalf("应展示思考过程, got %+v", items)
	}

	// 情形六：已派发子 Agent 未完成 → 等待子 Agent。
	s = &server.Session{
		Status: enums.SessionStatusRunning,
		Events: []server.SessionEvent{
			{Type: "tool_exec", Tool: "call_sub_agent", ToolPath: "session-1/code_assistant-1", Success: true, Timestamp: now},
		},
	}
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "等待子 Agent") || !strings.Contains(items[0].title, "code_assistant-1") {
		t.Fatalf("应展示等待子 Agent, got %+v", items)
	}

	// 情形七：子 Agent 已回传 → 回到思考中。
	s.Events = append(s.Events, server.SessionEvent{Type: "message", Kind: "sub_agent_done", Timestamp: now})
	items = appendLiveItems(nil, s)
	if len(items) != 1 || !strings.Contains(items[0].title, "⏳") {
		t.Fatalf("子 Agent 完成后应回到思考中, got %+v", items)
	}
}
