package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
)

// TestEventChatItemToolOutputCollapsed 验证 P2-3：ReadFile/SearchInFiles/ListDir/HTTPGet/HTTPPost
// 的 ToolOutput 默认被折叠（detail 不含 output），WriteFile/RunCommand 仍展开 output，
// 且所有工具的 ToolError 始终展示。
func TestEventChatItemToolOutputCollapsed(t *testing.T) {
	collapsedTools := []string{"ReadFile", "SearchInFiles", "ListDir", "HTTPGet", "HTTPPost"}
	verboseTools := []string{"WriteFile", "RunCommand"}

	for _, tool := range collapsedTools {
		ev := server.SessionEvent{
			Type:       "tool_exec",
			Kind:       "",
			Tool:       tool,
			ToolPath:   "path/to/target",
			ToolOutput: "这是一段应该被折叠的output",
			ToolError:  "",
			Success:    true,
			Timestamp:  time.Now(),
		}
		title, detail, ok := eventChatItem(ev)
		if !ok {
			t.Fatalf("%s 事件应该被展示", tool)
		}
		if !strings.Contains(title, tool) {
			t.Fatalf("title 应包含工具名 %s，got: %s", tool, title)
		}
		if strings.Contains(detail, "这是一段应该被折叠的output") {
			t.Fatalf("%s 的 detail 应折叠 output，got: %s", tool, detail)
		}
	}

	for _, tool := range verboseTools {
		ev := server.SessionEvent{
			Type:       "tool_exec",
			Kind:       "",
			Tool:       tool,
			ToolPath:   "path/to/target",
			ToolOutput: "这是一段应该被展示的output",
			ToolError:  "",
			Success:    true,
			Timestamp:  time.Now(),
		}
		_, detail, ok := eventChatItem(ev)
		if !ok {
			t.Fatalf("%s 事件应该被展示", tool)
		}
		if !strings.Contains(detail, "这是一段应该被展示的output") {
			t.Fatalf("%s 的 detail 应展示 output，got: %s", tool, detail)
		}
	}

	// 所有工具的 ToolError 都应展示
	for _, tool := range append(collapsedTools, verboseTools...) {
		ev := server.SessionEvent{
			Type:       "tool_exec",
			Kind:       "",
			Tool:       tool,
			ToolPath:   "path/to/target",
			ToolOutput: "output",
			ToolError:  "something went wrong",
			Success:    false,
			Timestamp:  time.Now(),
		}
		_, detail, ok := eventChatItem(ev)
		if !ok {
			t.Fatalf("%s 错误事件应该被展示", tool)
		}
		if !strings.Contains(detail, "something went wrong") {
			t.Fatalf("%s 的 detail 应展示错误信息，got: %s", tool, detail)
		}
	}
}
