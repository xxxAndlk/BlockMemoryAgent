package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// TestEventChatItemToolOutputCollapsed 验证 P2-3：ReadFile/SearchInFiles/ListDir/HTTPGet/HTTPPost
// 的 ToolOutput 默认被折叠（detail 不含 output），WriteFile/RunCommand 仍展开 output，
// 且所有工具的 ToolError 始终展示；rawDetail 始终保留完整输出。
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
		title, detail, rawDetail, ok := eventChatItem(ev, true)
		if !ok {
			t.Fatalf("%s 事件应该被展示", tool)
		}
		if !strings.Contains(title, tool) {
			t.Fatalf("title 应包含工具名 %s，got: %s", tool, title)
		}
		if strings.Contains(detail, "这是一段应该被折叠的output") {
			t.Fatalf("%s 的 detail 应折叠 output，got: %s", tool, detail)
		}
		if !strings.Contains(rawDetail, "这是一段应该被折叠的output") {
			t.Fatalf("%s 的 rawDetail 应保留完整 output，got: %s", tool, rawDetail)
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
		_, detail, rawDetail, ok := eventChatItem(ev, true)
		if !ok {
			t.Fatalf("%s 事件应该被展示", tool)
		}
		if !strings.Contains(detail, "这是一段应该被展示的output") {
			t.Fatalf("%s 的 detail 应展示 output，got: %s", tool, detail)
		}
		if !strings.Contains(rawDetail, "这是一段应该被展示的output") {
			t.Fatalf("%s 的 rawDetail 应保留完整 output，got: %s", tool, rawDetail)
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
		_, detail, _, ok := eventChatItem(ev, true)
		if !ok {
			t.Fatalf("%s 错误事件应该被展示", tool)
		}
		if !strings.Contains(detail, "something went wrong") {
			t.Fatalf("%s 的 detail 应展示错误信息，got: %s", tool, detail)
		}
	}
}

// TestEventChatItemToolCallCollapsed 验证非 verbose 工具的 tool_call（pending）事件
// 以及入参说明 Message 默认被折叠，只保留一行 [✓] 标题。
func TestEventChatItemToolCallCollapsed(t *testing.T) {
	collapsedTools := []string{"ReadFile", "SearchInFiles", "ListDir", "HTTPGet", "HTTPPost"}

	for _, tool := range collapsedTools {
		// tool_call pending 事件应被过滤
		callEv := server.SessionEvent{
			Type:      "progress",
			Kind:      "tool_call",
			Tool:      tool,
			Message:   "调用工具 " + tool,
			Timestamp: time.Now(),
		}
		if _, _, _, ok := eventChatItem(callEv, true); ok {
			t.Fatalf("%s 的 tool_call 事件应被折叠", tool)
		}

		// tool_exec 的 Message 详情应被折叠
		execEv := server.SessionEvent{
			Type:      "tool_exec",
			Kind:      "",
			Tool:      tool,
			ToolPath:  "path/to/target",
			Message:   "执行工具: " + tool + " 入参={\"path\":\"...\"}",
			Success:   true,
			Timestamp: time.Now(),
		}
		title, detail, _, ok := eventChatItem(execEv, true)
		if !ok {
			t.Fatalf("%s 的 tool_exec 事件应被展示", tool)
		}
		if !strings.Contains(title, tool) {
			t.Fatalf("title 应包含工具名 %s，got: %s", tool, title)
		}
		if strings.Contains(detail, "执行工具") || strings.Contains(detail, "调用工具") {
			t.Fatalf("%s 的 detail 应折叠 Message，got: %s", tool, detail)
		}
	}

	// verbose 工具的 tool_call 事件仍应保留
	for _, tool := range []string{"WriteFile", "RunCommand"} {
		callEv := server.SessionEvent{
			Type:      "progress",
			Kind:      "tool_call",
			Tool:      tool,
			Message:   "调用工具 " + tool,
			Timestamp: time.Now(),
		}
		if _, _, _, ok := eventChatItem(callEv, true); !ok {
			t.Fatalf("%s 的 tool_call 事件应被展示", tool)
		}
	}
}

// TestEventChatItemVerboseOutputTruncated 验证 compact=true 时 verbose 工具输出
// 在主对话区被截断，但 rawDetail 保留完整内容；compact=false 时 detail 不截断。
func TestEventChatItemVerboseOutputTruncated(t *testing.T) {
	var outputLines []string
	for i := 0; i < 30; i++ {
		outputLines = append(outputLines, fmt.Sprintf("line-%d", i))
	}
	output := strings.Join(outputLines, "\n")
	ev := server.SessionEvent{
		Type:       "tool_exec",
		Tool:       "RunCommand",
		ToolPath:   "D:\\project",
		ToolOutput: output,
		Success:    true,
		Timestamp:  time.Now(),
	}

	_, detailCompact, rawDetail, ok := eventChatItem(ev, true)
	if !ok {
		t.Fatal("RunCommand 事件应被展示")
	}
	if !strings.Contains(detailCompact, "line-0") || !strings.Contains(detailCompact, "line-19") {
		t.Fatalf("compact detail 应包含前 20 行，got: %s", detailCompact)
	}
	if strings.Contains(detailCompact, "line-20") {
		t.Fatalf("compact detail 不应包含第 20 行及以后，got: %s", detailCompact)
	}
	if !strings.Contains(detailCompact, "  ...") {
		t.Fatalf("compact detail 应有省略提示，got: %s", detailCompact)
	}
	if !strings.Contains(rawDetail, "line-29") {
		t.Fatalf("rawDetail 应保留完整 30 行，got: %s", rawDetail)
	}

	_, detailFull, rawDetail2, ok := eventChatItem(ev, false)
	if !ok {
		t.Fatal("RunCommand 事件应被展示")
	}
	if !strings.Contains(detailFull, "line-29") {
		t.Fatalf("compact=false 时 detail 应完整，got: %s", detailFull)
	}
	if detailFull != rawDetail2 {
		t.Fatal("compact=false 时 detail 应等于 rawDetail")
	}
}

// TestChatItemsCollapseToolEvents 验证连续的非 verbose 工具完成事件会被聚合成
// [✓] ToolName × N，verbose 工具、带 detail 或错误的事件不会被聚合。
func TestChatItemsCollapseToolEvents(t *testing.T) {
	s := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "看一下目录", Timestamp: time.Now()},
		},
		Events: []server.SessionEvent{
			{Type: "tool_exec", Tool: "ListDir", ToolPath: "D:\\a", Success: true, Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "ListDir", ToolPath: "D:\\b", Success: true, Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "ListDir", ToolPath: "D:\\c", Success: true, Timestamp: time.Now()},
			{Type: "progress", Kind: "llm_result", Message: "中间插入的 LLM 输出", Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "ReadFile", ToolPath: "D:\\x", Success: true, Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "ReadFile", ToolPath: "D:\\y", Success: true, Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "WriteFile", ToolPath: "D:\\z", Success: true, ToolOutput: "written", Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "WriteFile", ToolPath: "D:\\w", Success: true, ToolOutput: "written", Timestamp: time.Now()},
			{Type: "tool_exec", Tool: "ListDir", ToolPath: "D:\\d", Success: false, ToolError: "error", Timestamp: time.Now()},
		},
	}

	items := chatItems(s, true)
	if len(items) != 7 {
		t.Fatalf("聚合后应有 7 条（用户消息 + ListDir聚合 + LLM + ReadFile聚合 + WriteFile×2 + 错误ListDir），got %d: %+v", len(items), titlesOf(items))
	}
	if !strings.HasPrefix(items[1].title, "[✓] ListDir × 3") {
		t.Fatalf("3 条 ListDir 应聚合为 [✓] ListDir × 3，got: %s", items[1].title)
	}
	if !strings.Contains(items[1].detail, "D:\\a") || !strings.Contains(items[1].detail, "D:\\c") {
		t.Fatalf("ListDir 聚合详情应包含路径，got: %s", items[1].detail)
	}
	if !strings.HasPrefix(items[3].title, "[✓] ReadFile × 2") {
		t.Fatalf("2 条 ReadFile 应聚合为 [✓] ReadFile × 2，got: %s", items[3].title)
	}
	if strings.Contains(items[4].title, "×") {
		t.Fatalf("带 output 的 WriteFile 不应被聚合，got: %s", items[4].title)
	}
	if strings.Contains(items[5].title, "×") {
		t.Fatalf("带错误的 ListDir 不应被聚合，got: %s", items[5].title)
	}
}

// TestChatItemsDeduplicateSummary 验证与最后一条 Assistant 消息重复的
// "MetaAgent 总结: 会话完成: ..." 事件会被移除（包括 Markdown 格式差异）。
func TestChatItemsDeduplicateSummary(t *testing.T) {
	s := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "你是谁", Timestamp: time.Now()},
			{Role: enums.ChatRoleAssistant, Content: "你好！我是 **BlockMemoryAgent**，一个多 Agent 助手。\n\n- 能力 A\n- 能力 B", Timestamp: time.Now()},
		},
		Events: []server.SessionEvent{
			{Type: "system", Agent: "MetaAgent", Message: "会话完成: 你好！我是 **BlockMemoryAgent**，一个多 Agent 助手。\n\n- 能力 A\n- 能力 B", Timestamp: time.Now()},
			{Type: "system", Agent: "MetaAgent", Message: "会话完成: 你好！我是 **BlockMemoryAgent**，一个多 Agent 助手。\n\n- 能力 A\n- 能力 B", Timestamp: time.Now()},
		},
	}

	items := chatItems(s, true)
	if len(items) != 2 {
		t.Fatalf("去重后应只剩用户消息 + Assistant 消息，got %d: %+v", len(items), titlesOf(items))
	}
}

func titlesOf(items []chatItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.title
	}
	return out
}

// TestFirstMessagePendingDisplay 验证无会话时发送首条消息，TUI 立即切换到对话视图
// 并高亮展示用户输入，而不是继续显示欢迎页（修复“第一个问题未记录”的错觉）。
func TestFirstMessagePendingDisplay(t *testing.T) {
	m := &Model{
		styles:   NewStyles(),
		chatVP:   viewport.New(80, 20),
		width:    80,
		height:   24,
		httpAddr: "http://127.0.0.1:1", // 让后台 createSession 快速失败，避免测试被网络阻塞
		flashMu:  &sync.Mutex{},
	}
	m.chatVP.SetContent("")

	// 初始状态应展示欢迎页
	welcomeView := m.View()
	if !strings.Contains(welcomeView, "AI Agent for Code, Memory and More.") {
		t.Fatal("初始无会话时应展示欢迎页")
	}

	// 用户发送首条消息
	m.submitInput("hello world")

	if m.pendingFirstMessage != "hello world" {
		t.Fatalf("pendingFirstMessage 应被设为 %q，got %q", "hello world", m.pendingFirstMessage)
	}

	// 发送后应立刻切换到对话视图，不再展示欢迎页
	view := m.View()
	if strings.Contains(view, "AI Agent for Code, Memory and More.") {
		t.Fatal("发送首条消息后欢迎页应立即隐藏")
	}
	if !strings.Contains(view, "You") {
		t.Fatal("首条消息应以高亮 'You' 标签展示")
	}
	if !strings.Contains(view, "hello world") {
		t.Fatalf("首条消息内容应在对话区可见，got:\n%s", view)
	}
}
