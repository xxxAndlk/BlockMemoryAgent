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

	// 验证折叠工具的 output 在 detail 中被隐藏，但在 rawDetail 中保留。
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

	// 验证 verbose 工具的 output 在 detail 中展开。
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
	if !strings.Contains(detailCompact, "line-0") || !strings.Contains(detailCompact, "line-4") {
		t.Fatalf("compact detail 应包含前 5 行，got: %s", detailCompact)
	}
	if strings.Contains(detailCompact, "line-5") {
		t.Fatalf("compact detail 不应包含第 5 行及以后，got: %s", detailCompact)
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

// titlesOf 从 chatItem 列表中提取标题，用于测试诊断输出。
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
		styles:    NewStyles(),
		chatPanel: ChatPanel{vp: viewport.New(80, 20)},
		width:     80,
		height:    24,
		httpAddr:  "http://127.0.0.1:1", // 让后台 createSession 快速失败，避免测试被网络阻塞
		flashMu:   &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")

	// 初始状态应展示欢迎页
	welcomeView := m.View()
	if !strings.Contains(welcomeView, "AI Agent for Code, Memory and More.") {
		t.Fatal("初始无会话时应展示欢迎页")
	}

	// 用户发送首条消息
	m.submitInput("hello world")

	if m.chatPanel.pendingFirstMessage != "hello world" {
		t.Fatalf("pendingFirstMessage 应被设为 %q，got %q", "hello world", m.chatPanel.pendingFirstMessage)
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

// TestFirstMessageFallbackWhenSessionMissingUserMessage 验证：当选中会话的 Messages
// 中尚未同步首条用户消息时，TUI 仍通过 pendingFirstMessage 兜底展示该消息，
// 并以 "You" 高亮，避免首条问题"消失"。
func TestFirstMessageFallbackWhenSessionMissingUserMessage(t *testing.T) {
	m := &Model{
		styles: NewStyles(),
		chatPanel: ChatPanel{
			vp:                  viewport.New(80, 20),
			pendingFirstMessage: "hello fallback",
		},
		width:          80,
		height:         24,
		sessionsCursor: 0,
		flashMu:        &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")
	// 构造一个已选中但 Messages 里暂时没有用户消息的会话
	m.sessions = []*server.Session{{
		ID:        "session-fallback",
		Goal:      "other",
		Status:    enums.SessionStatusRunning,
		StartedAt: time.Now(),
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleSystem, Content: "Goal: other", Timestamp: time.Now()},
		},
		Events: []server.SessionEvent{},
	}}

	m.rebuildChatContent()
	view := m.View()
	t.Logf("fallback view:\n%s", view)
	if !strings.Contains(view, "You") {
		t.Fatal("pendingFirstMessage 兜底展示时应带有 'You' 标签")
	}
	if !strings.Contains(view, "hello fallback") {
		t.Fatalf("pendingFirstMessage 内容应在对话区可见，got:\n%s", view)
	}
}

// TestChatItemsMergeToolCallPairs 验证相邻的 [●] 工具调用行与同工具的 [✓] 结果行
// 被合并为单条结果行；未等到结果的 [●] 行保留。
func TestChatItemsMergeToolCallPairs(t *testing.T) {
	now := time.Now()
	s := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "跑个命令", Timestamp: now},
		},
		Events: []server.SessionEvent{
			{Type: "progress", Kind: "tool_call", Tool: "RunCommand", ToolPath: "go build ./...", Message: "调用工具 RunCommand", Timestamp: now.Add(time.Second)},
			{Type: "tool_exec", Kind: "tool_result", Tool: "RunCommand", ToolPath: "go build ./...", Message: "工具结果 RunCommand", ToolOutput: "ok", Success: true, Timestamp: now.Add(2 * time.Second)},
			{Type: "progress", Kind: "tool_call", Tool: "WriteFile", ToolPath: "a.go", Message: "调用工具 WriteFile", Timestamp: now.Add(3 * time.Second)},
		},
	}
	items := chatItems(s, true)
	// 用户消息 + 合并后的 RunCommand 结果行 + 仍在执行的 WriteFile [●] 行
	if len(items) != 3 {
		t.Fatalf("合并后应有 3 条，got %d: %+v", len(items), titlesOf(items))
	}
	if !strings.HasPrefix(items[1].title, "[✓] RunCommand") {
		t.Fatalf("RunCommand 应合并为单条 [✓] 结果行，got: %s", items[1].title)
	}
	if !strings.Contains(items[1].detail, "ok") {
		t.Fatalf("合并后的结果行应保留输出详情，got: %s", items[1].detail)
	}
	if strings.Contains(items[1].detail, "工具结果") {
		t.Fatalf("合并后的结果行不应包含噪声文案，got: %s", items[1].detail)
	}
	if !strings.HasPrefix(items[2].title, "[●] WriteFile") {
		t.Fatalf("未等到结果的 WriteFile 应保留 [●] 行，got: %s", items[2].title)
	}
}

// TestChatItemsAgentDoneAsAssistant 验证 agent_done 事件转为 assistant 条目，
// 且与相邻重复 Assistant 消息去重（恢复历史会话场景）。
func TestChatItemsAgentDoneAsAssistant(t *testing.T) {
	now := time.Now()
	answer := "任务完成，报告已写入 workspace/report.md"
	s := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "写个报告", Timestamp: now},
			{Role: enums.ChatRoleAssistant, Content: answer, Timestamp: now.Add(time.Second)},
		},
		Events: []server.SessionEvent{
			{Type: "agent_done", Agent: "MetaAgent", Message: answer, Timestamp: now.Add(2 * time.Second)},
		},
	}
	items := chatItems(s, true)
	if len(items) != 2 {
		t.Fatalf("同一答复只应出现一次（用户 + assistant），got %d: %+v", len(items), titlesOf(items))
	}
	if items[1].isEvent || items[1].role != enums.ChatRoleAssistant {
		t.Fatalf("答复应以 assistant 条目展示，got: %+v", items[1])
	}

	// 无历史消息时（进行中的会话），agent_done 单独作为 assistant 条目出现。
	s2 := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "写个报告", Timestamp: now},
		},
		Events: []server.SessionEvent{
			{Type: "agent_done", Agent: "MetaAgent", Message: answer, Timestamp: now.Add(time.Second)},
		},
	}
	items2 := chatItems(s2, true)
	if len(items2) != 2 {
		t.Fatalf("agent_done 应转为 assistant 条目，got %d: %+v", len(items2), titlesOf(items2))
	}
	if items2[1].title != answer {
		t.Fatalf("assistant 条目应直接展示答复内容（无前缀），got: %s", items2[1].title)
	}
}

// TestChatItemsClarifyPrefixStripped 验证澄清答复的内部标记前缀不在对话区展示。
func TestChatItemsClarifyPrefixStripped(t *testing.T) {
	s := &server.Session{
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: "[澄清答复] 用 PostgreSQL", Timestamp: time.Now()},
		},
	}
	items := chatItems(s, true)
	if len(items) != 1 {
		t.Fatalf("应有 1 条，got %d", len(items))
	}
	if items[0].title != "> 用 PostgreSQL" {
		t.Fatalf("澄清答复前缀应被剥离，got: %s", items[0].title)
	}
}

// TestEventChatItemSystemPauseShown 验证"已达最大轮数"暂停系统事件以 ⏸ 前缀展示，
// 普通系统事件（会话启动等）仍被过滤。
func TestEventChatItemSystemPauseShown(t *testing.T) {
	ev := server.SessionEvent{Type: "system", Message: "已达最大轮数上限（50 轮），会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。", Timestamp: time.Now()}
	title, _, _, ok := eventChatItem(ev, true)
	if !ok {
		t.Fatal("暂停事件应被展示")
	}
	if !strings.HasPrefix(title, "⏸ ") {
		t.Fatalf("标题应有 ⏸ 前缀，got: %s", title)
	}
	ev2 := server.SessionEvent{Type: "system", Message: "会话启动", Timestamp: time.Now()}
	if _, _, _, ok := eventChatItem(ev2, true); ok {
		t.Fatal("会话启动事件不应展示")
	}
}
