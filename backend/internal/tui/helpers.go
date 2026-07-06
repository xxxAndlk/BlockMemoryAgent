package tui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// ansiRegex 匹配 ANSI 转义序列（CSI/OSC 等）。T10 修复：
// tool_output 含 ANSI（如 colored log）会让 bubbletea 渲染错位。
var ansiRegex = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\-_]")

func stripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

func (m *Model) flashMsg(msg string) {
	// 加锁保护：postJSON/createSession 后台 goroutine 与主循环 View 并发读写 flash（T2 修复）
	m.flashMu.Lock()
	m.flash = msg
	m.flashUntil = time.Now().Add(2 * time.Second)
	m.flashMu.Unlock()
}

func (m *Model) hasPlan() bool {
	// 只要有激活会话就认为有计划可展示（包括 direct_tool 的合成计划），
	// 避免计划弹窗在大多数会话里被禁用。
	return m.selectedSession() != nil
}

// showChatDetail opens a popup with the full content of the selected chat item.
func (m *Model) showChatDetail() {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := chatItems(s, true)
	idx := m.chatCurrentItem()
	if idx < 0 || idx >= len(items) {
		return
	}
	item := items[idx]
	// 弹窗展示完整详情（rawDetail），不截断工具输出
	detail := item.rawDetail
	if detail == "" {
		detail = item.detail
	}
	m.openOverlay(item.title, strings.Split(detail, "\n"))
}

func (m *Model) showPlanDetailByIndex(idx int) {
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		return
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		return
	}
	snap := b.Snapshot()
	if idx < 0 || idx >= len(snap.Tasks) {
		return
	}
	t := snap.Tasks[idx]
	lines := []string{
		fmt.Sprintf("ID: %s", t.ID),
		fmt.Sprintf("Title: %s", t.Title),
		fmt.Sprintf("Status: %s", t.Status),
		fmt.Sprintf("Assignee: %s", t.Assignee),
	}
	if t.Result != "" {
		lines = append(lines, fmt.Sprintf("Result: %s", t.Result))
	}
	m.openOverlay("Plan Task", lines)
}

func (m *Model) showAgentDetailByIndex(idx int) {
	if idx < 0 || idx >= len(m.agentsNodes) {
		return
	}
	node := m.agentsNodes[idx]
	lines := []string{
		fmt.Sprintf("Instance: %s", node.instID),
		fmt.Sprintf("Name: %s", node.name),
		fmt.Sprintf("Type: %s", node.roleType),
		fmt.Sprintf("Status: %s", node.status),
	}
	if node.domain != "" {
		lines = append(lines, fmt.Sprintf("Domain: %s", node.domain))
	}
	if node.goal != "" {
		lines = append(lines, fmt.Sprintf("Goal: %s", node.goal))
	}
	m.openOverlay("Agent", lines)
}

type chatItem struct {
	title     string
	detail    string // 主对话区展示的 compact 详情（工具输出可能被截断）
	rawDetail string // 完整详情，用于弹窗/完整记录面板
	timestamp time.Time
	isEvent   bool           // true 表示来自 SessionEvent，false 表示来自 ChatMessage
	role      enums.ChatRole // 仅对 ChatMessage 有效
}

// buildPlanLines renders the current session's TaskBoard as flat lines for the popup.
func (m *Model) buildPlanLines() []string {
	s := m.selectedSession()
	var snap board.Snapshot
	if s != nil && m.rt != nil && m.rt.Boards != nil {
		if b := m.rt.Boards.Get(s.ID); b != nil {
			snap = b.Snapshot()
		}
	}

	// 没有看板时（如 direct_tool），用会话目标生成最小计划视图，
	// 避免弹窗只显示空白的 "(no plan)"。
	if len(snap.Tasks) == 0 {
		goal := ""
		status := board.TaskDone
		if s != nil {
			goal = s.Goal
			if goal == "" && len(s.Messages) > 0 {
				for _, msg := range s.Messages {
					if msg.Role == enums.ChatRoleUser {
						goal = strings.TrimSpace(msg.Content)
						break
					}
				}
			}
			switch s.Status {
			case enums.SessionStatusRunning:
				status = board.TaskInProgress
			case enums.SessionStatusError:
				status = board.TaskFailed
			}
		}
		if goal == "" {
			goal = "(no plan)"
		}
		snap = board.Snapshot{
			Goal:  goal,
			Tasks: []board.SubTask{{ID: "direct", Title: "直接执行", Status: status}},
		}
	}

	done, total := 0, len(snap.Tasks)
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
	}
	lines := []string{
		fmt.Sprintf("Goal: %s", snap.Goal),
		fmt.Sprintf("Progress: %d/%d", done, total),
		"",
	}
	for i, t := range snap.Tasks {
		marker := " "
		if i == m.overlayCursor {
			marker = "▸"
		}
		lines = append(lines, fmt.Sprintf("%s %s  %s", marker, statusIcon(string(t.Status)), t.Title))
	}
	return lines
}

// buildAgentsLines renders the agent topology as flat lines for the popup.
func (m *Model) buildAgentsLines() []string {
	if len(m.agentsNodes) == 0 {
		return []string{"(no agents)"}
	}
	var lines []string
	for i, node := range m.agentsNodes {
		prefix := strings.Repeat("  ", node.depth)
		var icon string
		switch node.roleType {
		case enums.RoleTypeMeta:
			icon = "◆"
		case enums.RoleTypeDomain:
			icon = "◆"
		case enums.RoleTypeSubDomain:
			icon = "◇"
		default:
			icon = "▸"
		}
		marker := " "
		if i == m.overlayCursor {
			marker = "▸"
		}
		name := node.name
		if node.goal != "" {
			name += " — " + truncate(node.goal, 40)
		}
		if node.isClarify {
			name = "Clarify pending"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", marker, prefix, icon, name, statusIcon(string(node.status))))
	}
	return lines
}

func chatItems(s *server.Session, compact bool) []chatItem {
	// TUI chat interleaves conversation messages with key Agent events
	// (tool calls, LLM/think output, errors), merged by timestamp so the ReAct
	// sequence (think → tool → result → next think) is visible. Pure-debug
	// events (token_usage/graph_step/agent_created/prompt/stats) are filtered
	// to keep the view readable.
	var items []chatItem
	for _, msg := range s.Messages {
		// v2.5：用户输入前缀 ">"，助手回复普通文本，
		// 初始 "Goal: ..." 系统提示作为噪声过滤。
		content := strings.TrimSpace(msg.Content)
		if msg.Role == enums.ChatRoleSystem && strings.HasPrefix(content, "Goal: ") {
			continue
		}

		var title string
		var detail string
		switch msg.Role {
		case enums.ChatRoleUser:
			title = "> " + content
		case enums.ChatRoleAssistant:
			title = content
			detail = ""
		default:
			// system / tool 等角色按原格式展示
			title = fmt.Sprintf("[%s] %s", msg.Role, msg.Timestamp.Format("15:04:05"))
			detail = formatMarkdown(content)
		}
		items = append(items, chatItem{
			title:     title,
			detail:    detail,
			timestamp: msg.Timestamp,
			isEvent:   false,
			role:      msg.Role,
		})
	}
	// P2-1（文档 §2.2）：内联 🧠 recalled 最多 2 条，避免淹没主对话。
	// 找出最后 2 条 memory_recall 事件的索引，其余在下面循环中跳过。
	recallKeep := map[int]bool{}
	var recallIdxs []int
	for i, ev := range s.Events {
		if ev.Kind == "memory_recall" && strings.TrimSpace(ev.Message) != "" {
			recallIdxs = append(recallIdxs, i)
		}
	}
	keepFrom := 0
	if len(recallIdxs) > 2 {
		keepFrom = len(recallIdxs) - 2 // 仅保留最后 2 条
	}
	for k := keepFrom; k < len(recallIdxs); k++ {
		recallKeep[recallIdxs[k]] = true
	}

	for i, ev := range s.Events {
		// 超出限额的 memory_recall 不内联展示（仍在右侧 Agent 面板的 Memory 区可见）
		if ev.Kind == "memory_recall" && !recallKeep[i] {
			continue
		}
		title, detail, rawDetail, ok := eventChatItem(ev, compact)
		if !ok {
			continue
		}
		items = append(items, chatItem{title: title, detail: detail, rawDetail: rawDetail, timestamp: ev.Timestamp, isEvent: true})
	}
	// 稳定排序：同时间戳时保持插入顺序（message 先于其后触发的事件）
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].timestamp.Before(items[j].timestamp)
	})

	// 聚合：把连续的无详情非 verbose 工具完成事件合并为 [✓] ToolName × N，
	// 大幅减少 ListDir/ReadFile 等高频工具在对话区的刷屏。
	if compact {
		items = aggregateToolEvents(items)
	}

	// 去重：删除所有与最后一条 Assistant 消息内容重复的 "MetaAgent 总结: 会话完成: ..."
	// 事件，避免直接回答等场景下同一段答案出现多次。
	if len(items) >= 2 {
		var lastAssistantIdx int = -1
		for i := len(items) - 1; i >= 0; i-- {
			if !items[i].isEvent && items[i].role == enums.ChatRoleAssistant {
				lastAssistantIdx = i
				break
			}
		}
		if lastAssistantIdx >= 0 {
			assistantText := normalizeChatText(items[lastAssistantIdx].title)
			// 从后往前删除所有重复总结事件（可能因 LLM/事件重复产生多条）
			for i := len(items) - 1; i > lastAssistantIdx; i-- {
				if !items[i].isEvent {
					continue
				}
				if strings.HasPrefix(items[i].title, "MetaAgent 总结: 会话完成: ") {
					summary := strings.TrimPrefix(items[i].title, "MetaAgent 总结: 会话完成: ")
					if normalizeChatText(summary) == assistantText {
						items = append(items[:i], items[i+1:]...)
					}
				}
			}
		}
	}

	return items
}

// normalizeChatText 把聊天文本归一化，用于去重比较：
// 移除 ANSI、Markdown 标记、列表符号，并压平空白。
func normalizeChatText(s string) string {
	s = stripANSI(s)
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "#", "")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// aggregateToolEvents 把连续的无详情非 verbose 工具完成事件聚合成一条
// [✓] ToolName × N，显著减少 ListDir/ReadFile 等高频工具刷屏。
func aggregateToolEvents(items []chatItem) []chatItem {
	if len(items) < 2 {
		return items
	}
	var out []chatItem
	for i := 0; i < len(items); {
		it := items[i]
		tool, ok := collapsibleToolTitle(it)
		if !ok {
			out = append(out, it)
			i++
			continue
		}
		paths := []string{}
		j := i
		for j < len(items) {
			nextTool, ok := collapsibleToolTitle(items[j])
			if !ok || nextTool != tool {
				break
			}
			path := strings.TrimSpace(strings.TrimPrefix(items[j].title, "[✓] "+tool+":"))
			paths = append(paths, path)
			j++
		}
		if len(paths) > 1 {
			aggTitle := fmt.Sprintf("[✓] %s × %d", tool, len(paths))
			out = append(out, chatItem{
				title:     aggTitle,
				detail:    aggregateToolPaths(paths),
				timestamp: it.timestamp,
				isEvent:   true,
			})
			i = j
			continue
		}
		out = append(out, it)
		i++
	}
	return out
}

// collapsibleToolTitle 判断 item 是否可被聚合：非 verbose 工具的 [✓] 完成事件且无 detail。
func collapsibleToolTitle(it chatItem) (tool string, ok bool) {
	if !it.isEvent || it.detail != "" {
		return "", false
	}
	if !strings.HasPrefix(it.title, "[✓] ") {
		return "", false
	}
	rest := strings.TrimPrefix(it.title, "[✓] ")
	idx := strings.Index(rest, ":")
	if idx <= 0 {
		return "", false
	}
	tool = strings.TrimSpace(rest[:idx])
	if verboseTools[tool] {
		return "", false
	}
	return tool, true
}

// aggregateToolPaths 把路径列表折叠成短 detail，最多展示前 5 条。
func aggregateToolPaths(paths []string) string {
	const maxShow = 5
	var b strings.Builder
	n := len(paths)
	if n <= maxShow {
		for _, p := range paths {
			b.WriteString("  ")
			b.WriteString(p)
			b.WriteByte('\n')
		}
		return strings.TrimRight(b.String(), "\n")
	}
	for _, p := range paths[:maxShow] {
		b.WriteString("  ")
		b.WriteString(p)
		b.WriteByte('\n')
	}
	b.WriteString(fmt.Sprintf("  ... 还有 %d 条", n-maxShow))
	return b.String()
}

// buildTranscriptLines 把整段对话（消息 + 关键事件）铺成可滚动弹窗用的扁平行列表。
// 每行按弹窗内宽截断，避免自动换行打乱 overlay 的行级滚动计数。
// 用途：Ctrl+L / 4 打开的"完整记录"面板，让用户不受窗口高度与输入栏焦点限制，
// 用 j/k 翻阅全部 LLM 输出 / 工具调用 / 思考过程。
func (m *Model) buildTranscriptLines() []string {
	s := m.selectedSession()
	if s == nil {
		return []string{"(no active session)"}
	}
	items := chatItems(s, false)
	if len(items) == 0 {
		return []string{"(empty — send a message below)"}
	}
	// 弹窗内容宽度：boxW = width*4/5，减去 padding(4) + border(2) + 余量
	maxW := m.width*4/5 - 8
	if maxW < 40 {
		maxW = 80
	}
	var out []string
	for _, it := range items {
		out = append(out, truncate(it.title, maxW))
		detail := it.rawDetail
		if detail == "" {
			detail = it.detail
		}
		for _, l := range strings.Split(detail, "\n") {
			out = append(out, "    "+truncate(l, maxW-4))
		}
		out = append(out, m.styles.Dim.Render("─"))
	}
	return out
}

// verboseTools 默认在 TUI 对话区展开完整 ToolOutput 的工具。
// 其余工具（ReadFile/SearchInFiles/ListDir/HTTPGet/HTTPPost）只显示工具名和路径，
// 隐藏 output 内容以减少视觉噪声（P2-3）。
var verboseTools = map[string]bool{
	"WriteFile":  true,
	"RunCommand": true,
}

// compactToolOutputLines 是 verbose 工具输出在主对话区最多展示的行数，
// 超出部分折叠，可在弹窗/Ctrl+L 完整记录中查看。
const compactToolOutputLines = 20

// truncateToolOutput 截断工具输出到指定行数，超出部分显示 "  ..." 提示。
func truncateToolOutput(output string, maxLines int) string {
	if maxLines <= 0 {
		return output
	}
	lines := strings.Split(output, "\n")
	if len(lines) <= maxLines {
		return output
	}
	return strings.Join(lines[:maxLines], "\n") + "\n  ..."
}

// eventChatItem 把一个 SessionEvent 映射为对话区的一行（title + detail + rawDetail）。
// compact=true 时会对 verbose 工具的长输出做截断，用于主对话区；
// compact=false 时 rawDetail 保留完整输出，用于弹窗/完整记录面板。
// 返回 ok=false 表示该事件类型不展示（调试噪声）。
func eventChatItem(ev server.SessionEvent, compact bool) (title, detail, rawDetail string, ok bool) {
	switch {
	case ev.Type == "tool_exec" || ev.Kind == "tool_call":
		tool := ev.Tool
		if tool == "" {
			tool = "tool"
		}
		// 非 verbose 工具只保留执行完成的事件，隐藏调用前 pending 事件，
		// 避免 ListDir/ReadFile/HTTPGet 等高频工具在对话区产生大量 [●] 行。
		if ev.Kind == "tool_call" && !verboseTools[tool] {
			return "", "", "", false
		}
		status := "✓"
		if ev.Kind == "tool_call" {
			status = "●"
		}
		if !ev.Success && ev.Type == "tool_exec" {
			status = "✗"
		}
		title = fmt.Sprintf("[%s] %s: %s", status, tool, ev.ToolPath)
		if title == fmt.Sprintf("[%s] %s: ", status, tool) {
			title = fmt.Sprintf("[%s] %s", status, tool)
		}
		// rawDetail 始终保留完整信息，用于弹窗/完整记录面板
		var full strings.Builder
		if ev.Message != "" {
			full.WriteString(ev.Message)
			full.WriteByte('\n')
		}
		if ev.ToolOutput != "" {
			full.WriteString("结果:\n")
			full.WriteString(stripANSI(ev.ToolOutput))
			full.WriteByte('\n')
		}
		if ev.ToolError != "" {
			full.WriteString("错误: ")
			full.WriteString(stripANSI(ev.ToolError))
			full.WriteByte('\n')
		}
		rawDetail = strings.TrimRight(full.String(), "\n")

		// compact detail：verbose 工具展示 Message + 截断 Output + Error；
		// 非 verbose 工具仅展示 Error，避免输出刷屏但保证错误可见。
		var compactDetail strings.Builder
		if verboseTools[tool] {
			if ev.Message != "" {
				compactDetail.WriteString(ev.Message)
				compactDetail.WriteByte('\n')
			}
			if ev.ToolOutput != "" {
				compactDetail.WriteString("结果:\n")
				compactDetail.WriteString(truncateToolOutput(stripANSI(ev.ToolOutput), compactToolOutputLines))
				if compactDetail.Len() > len("结果:\n") {
					compactDetail.WriteByte('\n')
				}
			}
		}
		if ev.ToolError != "" {
			compactDetail.WriteString("错误: ")
			compactDetail.WriteString(stripANSI(ev.ToolError))
			compactDetail.WriteByte('\n')
		}
		detail = strings.TrimRight(compactDetail.String(), "\n")
		if !compact {
			detail = rawDetail
		}
		return title, detail, rawDetail, true
	case ev.Kind == "memory_recall":
		title = "🧠 recalled: " + strings.TrimSpace(ev.Message)
		return title, "", title, true
	case ev.Kind == "topic_switch":
		msg := ev.Message
		if msg == "" {
			msg = "切换话题"
		}
		title = "─── " + msg + " ───"
		return title, "", title, true
	case ev.Kind == "llm_result" || ev.Kind == "think" || ev.Kind == "llm" || ev.Kind == "intend" || ev.Kind == "wait":
		agent := ev.Agent
		if agent == "" {
			agent = "Assistant"
		}
		title = agent + ": " + ev.Message
		return title, "", title, true
	case ev.Type == "agent_done":
		agent := ev.Agent
		if agent == "" {
			agent = "Agent"
		}
		title = agent + " 完成: " + ev.Message
		return title, "", title, true
	case ev.Type == "system":
		// 只展示会话完成总结，避免“会话启动/继续执行”等噪声淹没对话。
		if strings.Contains(ev.Message, "会话完成") {
			agent := ev.Agent
			if agent == "" {
				agent = "MetaAgent"
			}
			title = agent + " 总结: " + ev.Message
			return title, "", title, true
		}
		return "", "", "", false
	case ev.Kind == "error" || ev.Type == "error":
		title = "✗ Error: " + ev.Message
		return title, "", title, true
	}
	return "", "", "", false
}

// formatMarkdown applies light Markdown formatting for the chat view.
// Supports: headers, bold, italic, inline code, code blocks, lists, blockquotes.
// 行级状态机：按行扫描，遇 ``` 切换 inCodeBlock；代码块内原样累积，块外按行类型渲染。
func formatMarkdown(text string) string {
	var out []string
	var inCodeBlock bool
	var codeBlock []string

	// 预定义样式：避免在循环内反复构造，提升长文本渲染性能
	mdHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cHeader))
	mdBold := lipgloss.NewStyle().Bold(true)
	mdItalic := lipgloss.NewStyle().Italic(true)
	mdCode := lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue))
	mdCodeBlock := lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1)
	mdDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cDone))

	// flushCode 把累积的代码块行渲染为单个带背景的代码块并清空缓存
	flushCode := func() {
		if len(codeBlock) == 0 {
			return
		}
		out = append(out, mdCodeBlock.Render(strings.Join(codeBlock, "\n")))
		codeBlock = nil
	}

	for _, raw := range strings.Split(text, "\n") {
		line := raw
		trimmed := strings.TrimSpace(line)

		// Code fence：``` 切换代码块状态；行本身不输出
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				flushCode()
				inCodeBlock = false
			} else {
				inCodeBlock = true
			}
			continue
		}
		// 代码块内：原样累积，跳过其他 Markdown 解析，避免 ** 被误识别
		if inCodeBlock {
			codeBlock = append(codeBlock, line)
			continue
		}

		// Header：# ~ ###### 后跟空格或行尾；level 不限但渲染样式统一
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 0 && (level == len(trimmed) || trimmed[level] == ' ') {
				content := strings.TrimSpace(trimmed[level:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdHeader.Render(content))
				continue
			}
		}

		// Blockquote：> 前缀，用 ┃ 替换以适配等宽字体对齐
		if strings.HasPrefix(trimmed, ">") {
			content := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
			out = append(out, mdDim.Render("┃ "+content))
			continue
		}

		// List item：- / * / + 后跟空格才识别（避免把 *bold* 误判为列表项）
		if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "+") {
			if len(trimmed) > 1 && trimmed[1] == ' ' {
				content := strings.TrimSpace(trimmed[1:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdDim.Render("• ")+content)
				continue
			}
		}

		// Plain line：仅做内联格式化
		line = applyInlineMarkdown(line, mdBold, mdItalic, mdCode)
		out = append(out, line)
	}
	// 文本结束时若仍在代码块中，flush 兜底输出
	flushCode()

	return strings.Join(out, "\n")
}

// applyInlineMarkdown handles **bold**, *italic*, and `inline code`.
// 顺序很重要：先处理 `code`（避免其中 ** 被吃掉），再 **bold，最后 *italic。
// 这样 ** 会优先于 * 被匹配，避免 bold 内容被识别成 italic。
func applyInlineMarkdown(text string, bold, italic, code lipgloss.Style) string {
	// Inline code
	text = replacePairs(text, "`", func(s string) string { return code.Render(s) })
	// Bold
	text = replacePairs(text, "**", func(s string) string { return bold.Render(s) })
	// Italic (single asterisks not already consumed by bold)
	text = replacePairs(text, "*", func(s string) string { return italic.Render(s) })
	return text
}

// replacePairs replaces matching pairs of markers with the result of f(content).
// 简易成对替换：找到首个 marker 作为开标记，再找其后第一个 marker 作为闭标记，
// 把中间内容传给 f 渲染；循环到没有配对为止。不支持嵌套。
func replacePairs(text, marker string, f func(string) string) string {
	for {
		start := strings.Index(text, marker)
		if start == -1 {
			break
		}
		end := strings.Index(text[start+len(marker):], marker)
		if end == -1 {
			break // 只有开标记无闭标记：保持原样，避免吞字符
		}
		end += start + len(marker)
		content := text[start+len(marker) : end]
		text = text[:start] + f(content) + text[end+len(marker):]
	}
	return text
}

// planStatusText 把看板任务状态转换为短文本标签，用于右侧面板展示。
func planStatusText(status board.TaskStatus) string {
	switch status {
	case board.TaskDone:
		return " Done"
	case board.TaskInProgress:
		return " Running"
	case board.TaskBlocked:
		return " Blocked"
	case board.TaskFailed:
		return " Failed"
	default:
		return " Waiting"
	}
}

// taskElapsed 估算任务已用时长：已完成/失败用 UpdatedAt-CreatedAt，
// 进行中用 Now-CreatedAt，待处理返回 0。
func taskElapsed(t board.SubTask) time.Duration {
	if t.Status == board.TaskPending {
		return 0
	}
	end := t.UpdatedAt
	if t.Status == board.TaskInProgress || t.Status == board.TaskBlocked {
		end = time.Now()
	}
	if end.Before(t.CreatedAt) {
		end = t.CreatedAt
	}
	return end.Sub(t.CreatedAt)
}

// formatDurationShort 把时长格式化为 mm:ss 或 hh:mm:ss。
func formatDurationShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// agentTreePrefix 根据 Agent 在拓扑中的深度返回带缩进/连接符的前缀，
// 让右侧面板的 Agent 编排更像参考设计中的层级拓扑。
func agentTreePrefix(depth int) string {
	if depth <= 0 {
		return ""
	}
	return strings.Repeat("  ", depth)
}

func statusIcon(status string) string {
	switch status {
	case "running", string(enums.RoleStatusActive), string(board.TaskInProgress):
		return "●"
	case "awaiting_clarify", string(enums.RoleStatusWaiting), string(board.TaskBlocked):
		return "◐"
	case "completed", string(board.TaskDone):
		return "✓"
	case "error", string(board.TaskFailed):
		return "✗"
	case string(board.TaskPending), string(enums.RoleStatusIdle):
		return "◦"
	default:
		return "◦"
	}
}

// formatTopBar 按 v2.0 格式渲染顶部状态栏：
// BlockMemoryAgent > {sessionID}  ●running  {N} agents active  in:{in} out:{out}
func formatTopBar(styles *Styles, sessionID, status string, agentCount, inTokens, outTokens int) string {
	if sessionID == "" {
		sessionID = "-"
	}
	if status == "" {
		status = "idle"
	}
	icon := statusIcon(status)
	agents := ""
	if agentCount > 0 {
		agents = fmt.Sprintf("  %d agents active", agentCount)
	}
	tokenStr := ""
	if inTokens > 0 || outTokens > 0 {
		tokenStr = fmt.Sprintf("  in:%d out:%d", inTokens, outTokens)
	}
	left := styles.Title.Render("BlockMemoryAgent") + styles.StatLabel.Render(" > ") + styles.StatValue.Render(sessionID)
	mid := styles.StatValue.Render("  "+icon+status) + styles.StatValue.Render(agents)
	right := styles.StatLabel.Render(tokenStr)
	return left + mid + right
}

// formatPlanBar 渲染计划进度栏。
// 有任务看板时显示当前计划步骤与进度；无计划时显示 [direct] 直接回答。
func formatPlanBar(styles *Styles, snap board.Snapshot, toolLabel string) string {
	if len(snap.Tasks) == 0 {
		// 无 plan 时：若仍有工具在执行，展示工具状态（文档 §2.3 "plan 进度+工具状态"）
		if toolLabel != "" {
			return styles.Dim.Render("[tool] " + toolLabel + " [●]")
		}
		return styles.Dim.Render("[direct] 直接回答")
	}
	done := 0
	current := 0
	for i, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
		if t.Status == board.TaskInProgress || t.Status == board.TaskBlocked {
			current = i + 1
		}
	}
	if current == 0 && done < len(snap.Tasks) {
		current = done + 1
	}
	var steps []string
	for i, t := range snap.Tasks {
		step := fmt.Sprintf("%d.%s", i+1, t.Title)
		if i+1 == current {
			step = styles.StatValue.Render(step)
		} else if i+1 <= done {
			step = styles.Dim.Render(step)
		}
		steps = append(steps, step)
	}
	progressIcon := "●"
	if done == len(snap.Tasks) {
		progressIcon = "✓"
	}
	return fmt.Sprintf("[plan] %s   [%d/%d] %s",
		strings.Join(steps, " → "),
		current,
		len(snap.Tasks),
		styles.StatValue.Render(progressIcon),
	)
}

// lastToolLabel 返回当前会话中"进行中"的工具标签（"Name: path" 或 "Name"）。
// 判定：遍历事件，最后一个 tool_call（●）若未被后续 tool_exec（✓/✗）关闭，则视为进行中。
// 无进行中工具返回空串。用于 plan 栏展示工具状态（P2-1，文档 §2.3）。
func lastToolLabel(s *server.Session) string {
	if s == nil {
		return ""
	}
	inFlight := false
	var tool, path string
	for _, ev := range s.Events {
		if ev.Kind == "tool_call" {
			inFlight = true
			tool = ev.Tool
			path = ev.ToolPath
		} else if ev.Type == "tool_exec" {
			inFlight = false // 工具执行完成（成功或失败），关闭进行中标记
		}
	}
	if !inFlight {
		return ""
	}
	if tool == "" {
		tool = "tool"
	}
	if path != "" {
		return tool + ": " + path
	}
	return tool
}

// renderAgentPanel 渲染右侧 Agent 面板。
func renderAgentPanel(m *Model, w int) string {
	s := m.selectedSession()
	if s == nil {
		return m.styles.BlurBorder.Width(w).Render(m.styles.Dim.Render("No active session"))
	}

	var lines []string
	lines = append(lines, m.styles.Header.Render("Agents"))

	if len(m.agentsNodes) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (no agents)"))
	} else {
		for _, node := range m.agentsNodes {
			prefix := strings.Repeat("  ", node.depth)
			icon := statusIcon(string(node.status))
			name := node.name
			if node.goal != "" {
				name += " " + m.styles.Dim.Render(truncate(node.goal, w-12))
			}
			lines = append(lines, fmt.Sprintf("%s%s %s %s", prefix, icon, name, m.styles.Dim.Render("")))
		}
	}

	lines = append(lines, "", m.styles.Header.Render("Memory"))
	recalls := recentMemoryRecalls(s, 3)
	if len(recalls) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (none)"))
	} else {
		for _, r := range recalls {
			lines = append(lines, m.styles.Dim.Render("  • "+truncate(r, w-4)))
		}
	}

	lines = append(lines, "", m.styles.Header.Render("Tokens"))
	lines = append(lines, fmt.Sprintf("  in:%d out:%d", m.totalInputTokens, m.totalOutputTokens))

	content := strings.Join(lines, "\n")
	return m.styles.BlurBorder.Width(w).Height(m.height - 2).Render(content)
}

// recentMemoryRecalls 从会话事件中抽取最近召回的记忆片段。
func recentMemoryRecalls(s *server.Session, limit int) []string {
	if limit <= 0 {
		limit = 3
	}
	var out []string
	for i := len(s.Events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := s.Events[i]
		if ev.Kind == "memory_recall" && strings.TrimSpace(ev.Message) != "" {
			out = append([]string{strings.TrimSpace(ev.Message)}, out...)
		}
	}
	return out
}
