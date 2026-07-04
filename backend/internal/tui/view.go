package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/reflow/wordwrap"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// View renders the entire TUI in single-column chat-focused layout.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.singleColumnView()
}

// singleColumnView v2.5 主布局：顶部状态栏 + 主内容区 + 输入栏 + 底部快捷键栏。
// 有活动会话且终端宽度充足时，主内容区左侧为对话区，右侧为计划/Agent 面板。
func (m Model) singleColumnView() string {
	contentH := m.mainContentHeight()

	var mainRow string
	if m.rightPanelVisible() {
		chatW := m.chatAreaWidth()
		rightW := m.rightPanelWidth()
		chatPanel := m.renderChat(chatW, contentH)
		rightPanel := m.renderRightPanels(rightW, contentH)
		mainRow = lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, rightPanel)
	} else {
		mainRow = m.renderChat(m.width, contentH)
	}

	view := lipgloss.JoinVertical(lipgloss.Top,
		m.renderTopBar(m.width),
		mainRow,
		m.renderInput(m.width),
		m.renderShortcutBar(m.width),
	)

	if m.overlay != overlayNone {
		overlayH := m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
		overlay := m.renderOverlay(m.width, overlayH)
		view = lipgloss.JoinVertical(lipgloss.Left, view, overlay)
	}
	return view
}

func (m Model) renderTopBar(w int) string {
	// 左侧：版本、模型、Memory、Skills、MCP、Workspace
	version := m.styles.TopBarLabel.Render("BlockMemoryAgent") + m.styles.TopBarSep.Render(" v0.8.0")
	modelLabel := m.styles.TopBarLabel.Render("Model") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render(m.modelName)
	memory := m.styles.TopBarLabel.Render("Memory") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarStatus.Render("On")
	skills := m.styles.TopBarLabel.Render("Skills") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("38")
	mcp := m.styles.TopBarLabel.Render("MCP") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("12")
	workspace := m.styles.TopBarLabel.Render("Workspace") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("~/demo")
	left := lipgloss.JoinHorizontal(lipgloss.Left, version, "  ", modelLabel, "  ", memory, "  ", skills, "  ", mcp, "  ", workspace)

	// 右侧：Session、Status、Time
	s := m.selectedSession()
	status := "idle"
	sessionID := "-"
	if s != nil {
		status = string(s.Status)
		sessionID = "#" + s.ID
	}
	statusColor := cStatusIdle
	switch status {
	case "running":
		statusColor = cStatusRun
	case "awaiting_clarify":
		statusColor = cStatusWait
	case "error":
		statusColor = cStatusErr
	}
	statusDot := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Render("●")
	sessionStr := m.styles.TopBarLabel.Render("Session") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render(sessionID)
	statusStr := m.styles.TopBarLabel.Render("Status") + m.styles.TopBarSep.Render(": ") + statusDot + " " + m.styles.TopBarValue.Render(status)
	timeStr := m.styles.TopBarLabel.Render("Time") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("00:00:00")
	right := lipgloss.JoinHorizontal(lipgloss.Left, sessionStr, "  ", statusStr, "  ", timeStr)

	line := lipgloss.JoinHorizontal(lipgloss.Top, left, lipgloss.NewStyle().Width(w-lipgloss.Width(left)-lipgloss.Width(right)).Render(""), right)
	if lipgloss.Width(line) > w {
		line = left
	}
	return m.styles.TopBar.Width(w).Height(1).Render(line)
}

func (m Model) renderWelcome(w, h int) string {
	title := m.styles.WelcomeTitle.Render("BlockMemoryAgent")
	subtitle := m.styles.WelcomeSub.Render("AI Agent for Code, Memory and More.")
	info := m.renderWelcomeInfo(w)
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", subtitle, "", info)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

// renderEmptyChat 当会话已创建但还没有任何聊天内容时展示的占位提示。
// 放在对话区顶部，提示用户输入消息，避免首屏空白导致"第一个问题不展示"的错觉。
func (m Model) renderEmptyChat(w, h int) string {
	title := m.styles.WelcomeTitle.Render("BlockMemoryAgent")
	hint := m.styles.WelcomeSub.Render("会话已启动，在底部输入栏发送第一条消息。")
	shortcuts := m.styles.Dim.Render("Enter 发送 · / 命令 · ? 帮助 · Ctrl+C 退出")
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", hint, "", shortcuts)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

func (m Model) renderWelcomeInfo(w int) string {
	pairs := []struct {
		icon  string
		label string
		value string
	}{
		{"🖥", "Model", m.modelName},
		{"📁", "Workspace", "~/demo"},
		{"🧠", "Memory", "Enabled"},
		{"#", "Session", "#12"},
		{"⚡", "Skills", "38"},
		{"🪟", "Context Window", "128K"},
		{"🔌", "MCP Servers", "12 Connected"},
		{"🐚", "Shell", "zsh"},
	}
	colW := (w - 6) / 2
	if colW < 20 {
		colW = 20
	}
	var left, right []string
	for i, p := range pairs {
		line := fmt.Sprintf("%s %s: %s", p.icon, m.styles.WelcomeLabel.Render(p.label), m.styles.WelcomeValue.Render(p.value))
		line = truncate(line, colW)
		if i%2 == 0 {
			left = append(left, line)
		} else {
			right = append(right, line)
		}
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		strings.Join(left, "\n"),
		lipgloss.NewStyle().Width(4).Render(""),
		strings.Join(right, "\n"),
	)
	return m.styles.WelcomeBox.Width(w).Render(body)
}

func (m Model) renderChat(w, h int) string {
	const scrollbarW = 1
	gap := 1
	contentW := w - scrollbarW - gap
	if contentW < 4 {
		contentW = w
	}

	s := m.selectedSession()
	if s == nil && m.pendingFirstMessage == "" {
		return m.renderWelcome(contentW, h)
	}

	// 有会话但暂无消息/事件，且无本地预展示消息时，在对话区顶部显示首页提示。
	if s != nil && len(chatItems(s, true)) == 0 && m.pendingFirstMessage == "" {
		return m.renderEmptyChat(contentW, h)
	}

	// viewport 尺寸在 WindowSizeMsg 中维护；渲染前再同步一次以防万一直接调用。
	m.chatVP.Width = contentW
	m.chatVP.Height = h

	content := lipgloss.NewStyle().Width(contentW).Height(h).Render(m.chatVP.View())
	totalLines := m.chatVP.TotalLineCount()
	viewportH := m.chatVP.VisibleLineCount()
	startLine := m.chatVP.YOffset
	bar := m.renderScrollbar(scrollbarW, h, viewportH, totalLines, startLine)
	return lipgloss.JoinHorizontal(lipgloss.Top, content, bar)
}

func (m Model) renderRightPanels(w, h int) string {
	if w < 20 {
		w = 20
	}
	showPlan := m.hasPlan()
	showAgents := len(m.agentsNodes) > 1
	if showPlan && showAgents {
		topH := h * 55 / 100
		if topH < 6 {
			topH = 6
		}
		bottomH := h - topH
		if bottomH < 6 {
			bottomH = 6
		}
		return lipgloss.JoinVertical(lipgloss.Top, m.renderPlanPanel(w, topH), m.renderAgentsPanel(w, bottomH))
	}
	if showPlan {
		return m.renderPlanPanel(w, h)
	}
	if showAgents {
		return m.renderAgentsPanel(w, h)
	}
	return ""
}

func (m Model) renderPlanPanel(w, h int) string {
	header := m.styles.PanelHeader.Width(w).Render("执行计划")
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}

	var lines []string
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		lines = append(lines, "(no plan)")
		body := strings.Join(lines, "\n")
		return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w).Height(h-1).Render(body))
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		lines = append(lines, "(no plan)")
		body := strings.Join(lines, "\n")
		return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w).Height(h-1).Render(body))
	}
	snap := b.Snapshot()
	done, total := 0, len(snap.Tasks)
	current := -1
	for i, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
		if current == -1 && (t.Status == board.TaskInProgress || t.Status == board.TaskBlocked) {
			current = i
		}
	}
	if current == -1 && total > 0 && done < total {
		current = done
	}

	lines = append(lines, m.styles.Dim.Render("Goal: ")+truncate(snap.Goal, innerW-6))
	pct := 0
	if total > 0 {
		pct = done * 100 / total
	}
	barW := innerW - 8
	if barW < 4 {
		barW = 4
	}
	filled := 0
	if total > 0 {
		filled = barW * done / total
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("─", barW-filled)
	lines = append(lines, fmt.Sprintf("%s %d%%", bar, pct))
	lines = append(lines, "")

	for i, t := range snap.Tasks {
		icon := statusIcon(string(t.Status))
		prefix := fmt.Sprintf("%d. ", i+1)
		line := prefix + icon + " " + t.Title
		if i == current {
			line = m.styles.StatValue.Render(truncate(line, innerW))
		} else if t.Status == board.TaskDone {
			line = m.styles.Dim.Render(truncate(line, innerW))
		} else {
			line = truncate(line, innerW)
		}
		lines = append(lines, line)
	}

	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w).Height(h-1).Render(body))
}

func (m Model) renderAgentsPanel(w, h int) string {
	header := m.styles.PanelHeader.Width(w).Render("Agent 编排")
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}

	var lines []string
	if len(m.agentsNodes) == 0 {
		lines = append(lines, "(no agents)")
	} else {
		for _, node := range m.agentsNodes {
			prefix := strings.Repeat("  ", node.depth)
			icon := statusIcon(string(node.status))
			name := node.name
			if node.goal != "" {
				name += " " + m.styles.Dim.Render(truncate(node.goal, innerW-lipgloss.Width(prefix)-lipgloss.Width(name)-4))
			}
			line := fmt.Sprintf("%s%s %s", prefix, icon, name)
			lines = append(lines, truncate(line, innerW))
		}
	}

	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w).Height(h-1).Render(body))
}

// buildChatContent 把当前会话的全部 chatItem 渲染成 viewport 可滚动的字符串，
// 并同步更新 m.chatItemOffsets。
func (m *Model) buildChatContent(width int) string {
	s := m.selectedSession()
	var items []chatItem
	if s != nil {
		items = chatItems(s, true)
	} else if m.pendingFirstMessage != "" {
		// 会话创建中，本地预显示首条用户消息，保证高亮样式与真实消息一致
		items = []chatItem{{
			title:     "> " + m.pendingFirstMessage,
			timestamp: time.Now(),
			isEvent:   false,
			role:      enums.ChatRoleUser,
		}}
	} else {
		m.chatItemOffsets = nil
		return ""
	}
	offsets := make([]int, len(items))
	var lines []string

	for i, item := range items {
		offsets[i] = len(lines)
		ts := item.timestamp.Format("15:04:05")
		tsStyled := m.styles.Dim.Render(ts)

		// 标题行：根据来源选择标签与样式
		var titleLines []string
		switch {
		case strings.HasPrefix(item.title, "> "):
			content := strings.TrimPrefix(item.title, "> ")
			label := m.styles.LogUser.Render("You")
			availW := width - lipgloss.Width(label) - lipgloss.Width(ts) - 3
			if availW < 4 {
				availW = 4
			}
			text := m.styles.LogUser.Render(truncate(content, availW))
			titleLines = append(titleLines, tsStyled+" "+label+" "+text)
		case strings.HasPrefix(item.title, "[●] "):
			titleLines = append(titleLines, tsStyled+" "+m.styles.LogWarn.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case strings.HasPrefix(item.title, "[✓] "):
			titleLines = append(titleLines, tsStyled+" "+m.styles.LogSuccess.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case strings.HasPrefix(item.title, "[✗] "):
			titleLines = append(titleLines, tsStyled+" "+m.styles.LogError.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case strings.HasPrefix(item.title, "🧠 recalled: "):
			titleLines = append(titleLines, tsStyled+" "+m.styles.Dim.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case strings.HasPrefix(item.title, "─── ") && strings.HasSuffix(item.title, " ───"):
			titleLines = append(titleLines, tsStyled+" "+m.styles.CallStack.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case strings.HasPrefix(item.title, "✗ Error"):
			titleLines = append(titleLines, tsStyled+" "+m.styles.LogError.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		case item.isEvent:
			// 事件标题已自带 Agent 名称（如 MetaAgent: ... / code_assistant 完成: ...）
			titleLines = append(titleLines, tsStyled+" "+m.styles.LogInfo.Render(truncate(item.title, width-lipgloss.Width(ts)-1)))
		default:
			label := m.styles.LogAssistant.Render("Assistant")
			availW := width - lipgloss.Width(label) - lipgloss.Width(ts) - 3
			if availW < 4 {
				availW = 4
			}
			text := formatMarkdown(item.title)
			// 对 Assistant 长回答做自动换行，避免截断
			wrapped := wordwrap.String(text, availW)
			wrappedLines := strings.Split(wrapped, "\n")
			for j, wl := range wrappedLines {
				line := tsStyled + " " + label + " " + wl
				if j > 0 {
					// 续行去掉 timestamp/label，用空格对齐
					line = strings.Repeat(" ", lipgloss.Width(ts)+1) + "   " + wl
				}
				titleLines = append(titleLines, line)
			}
		}
		lines = append(lines, titleLines...)

		// detail 行统一缩进并截断/换行；空 detail 跳过避免标题与详情重复（non-tool 事件 detail 为空）
		if item.detail != "" {
			for _, l := range displayDetailLines(item.title, item.detail) {
				wrapped := wordwrap.String(l, width-2)
				for _, wl := range strings.Split(wrapped, "\n") {
					lines = append(lines, "  "+wl)
				}
			}
		}
	}

	m.chatItemOffsets = offsets
	return strings.Join(lines, "\n")
}

// renderScrollbar 绘制右侧垂直滚动条。
// w/h 为滚动条区域宽高；viewportH/totalLines/startLine 决定滑块位置与高度。
func (m Model) renderScrollbar(w, h, viewportH, totalLines, startLine int) string {
	if h < 1 {
		return ""
	}
	if w < 1 {
		w = 1
	}
	trackStyle := m.styles.ScrollbarTrack
	thumbStyle := m.styles.ScrollbarThumb

	rows := make([]string, h)
	for i := range rows {
		rows[i] = trackStyle.Render("│")
	}

	if totalLines > viewportH && viewportH > 0 {
		scrollable := totalLines - viewportH
		if scrollable < 1 {
			scrollable = 1
		}
		// 滑块高度按视口占比缩放，最小 1 行，最大不超过轨道。
		thumbH := h * viewportH / totalLines
		if thumbH < 1 {
			thumbH = 1
		}
		if thumbH > h {
			thumbH = h
		}
		thumbPos := startLine * (h - thumbH) / scrollable
		if thumbPos < 0 {
			thumbPos = 0
		}
		if thumbPos+thumbH > h {
			thumbPos = h - thumbH
		}
		for i := thumbPos; i < thumbPos+thumbH; i++ {
			rows[i] = thumbStyle.Render("█")
		}
	}
	return lipgloss.NewStyle().Width(w).Height(h).Render(strings.Join(rows, "\n"))
}

// clipChat applies a fixed Width/Height style so the borderless chat area
// never grows beyond its allocated space and pushes the input bar down.
func (m Model) clipChat(content string, w, h int) string {
	return lipgloss.NewStyle().Width(w).Height(h).Render(content)
}

func (m Model) renderPlanBar(w int) string {
	var snap board.Snapshot
	var toolLabel string
	if s := m.selectedSession(); s != nil {
		toolLabel = lastToolLabel(s)
		if m.rt != nil && m.rt.Boards != nil {
			if b := m.rt.Boards.Get(s.ID); b != nil {
				snap = b.Snapshot()
			}
		}
	}
	line := formatPlanBar(m.styles, snap, toolLabel)
	return lipgloss.NewStyle().Width(w).Height(1).Render(line)
}

func (m Model) renderInput(w int) string {
	prompt := ">"
	switch m.inputMode {
	case inputClarify:
		prompt = "/clarify>"
	case inputInterrupt:
		prompt = "/interrupt>"
	case inputEnqueue:
		prompt = "/enqueue>"
	}
	left := m.styles.InputPrompt.Render(" " + prompt + " ")
	cursor := " "
	if m.focus == panelInput {
		cursor = "▌"
	}

	var text string
	if len(m.inputRunes) == 0 {
		text = m.styles.InputHint.Render("Type your message... (Enter to send, / for commands)")
	} else {
		text = string(m.inputRunes[:m.inputCursor]) + cursor + string(m.inputRunes[m.inputCursor:])
	}

	// 持锁读 flash（T2 修复：后台 HTTP goroutine 可能并发写）
	m.flashMu.Lock()
	curFlash := m.flash
	m.flashMu.Unlock()
	flash := ""
	if curFlash != "" {
		flash = "  " + m.styles.LogError.Render(curFlash)
	}

	border := m.styles.BlurBorder
	if m.focus == panelInput {
		border = m.styles.FocusBorder
	}
	content := lipgloss.JoinVertical(lipgloss.Left, left+" "+text+flash, "")
	return border.Width(w).Height(3).Render(content)
}

func (m Model) renderShortcutBar(w int) string {
	shortcuts := []struct {
		key   string
		label string
	}{
		{"K", "Command Palette"},
		{"P", "Plan"},
		{"A", "Agents"},
		{"L", "Logs"},
		{"M", "Memory"},
		{"G", "Git Diff"},
		{"S", "Settings"},
		{"?", "Help"},
		{"Ctrl+C", "Exit"},
	}
	var parts []string
	for _, s := range shortcuts {
		parts = append(parts, "["+m.styles.ShortcutKey.Render(s.key)+"] "+m.styles.ShortcutLabel.Render(s.label))
	}
	return m.styles.ShortcutBar.Width(w).Height(1).Render(strings.Join(parts, "  "))
}

func (m Model) renderOverlay(w, h int) string {
	if m.overlay == overlayHelp {
		m.overlayTitle = "Help"
		m.overlayLines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
	if m.overlay == overlayPlan {
		m.overlayLines = m.buildPlanLines()
	} else if m.overlay == overlayAgents {
		m.overlayLines = m.buildAgentsLines()
	} else if m.overlay == overlayLog {
		m.overlayLines = m.buildTranscriptLines()
	}

	maxLines := h - 4
	if maxLines < 3 {
		maxLines = 3
	}
	if m.overlayCursor < 0 {
		m.overlayCursor = 0
	}
	if m.overlayCursor >= len(m.overlayLines) {
		m.overlayCursor = len(m.overlayLines) - 1
	}
	start := m.overlayCursor - maxLines/2
	if start < 0 {
		start = 0
	}
	end := start + maxLines
	if end > len(m.overlayLines) {
		end = len(m.overlayLines)
		start = end - maxLines
		if start < 0 {
			start = 0
		}
	}
	visible := m.overlayLines[start:end]
	content := strings.Join(visible, "\n")

	boxW := w * 4 / 5
	if boxW < 40 {
		boxW = w - 4
	}
	boxH := h - 2

	header := m.styles.Header.Render(m.overlayTitle)
	hint := m.styles.Dim.Render("  [Esc close · j/k scroll · enter detail]")
	body := lipgloss.JoinVertical(lipgloss.Left, header+hint, content)
	box := m.styles.Overlay.Width(boxW).Height(boxH).Render(body)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// displayDetailLines returns detail lines as they will be rendered.
// Tool output (🔧 prefix) is truncated to 5 lines + "    ..." to keep the TUI compact.
func displayDetailLines(title, detail string) []string {
	lines := strings.Split(detail, "\n")
	if strings.HasPrefix(title, "🔧") && len(lines) > 5 {
		lines = lines[:5]
		lines = append(lines, "    ...")
	}
	return lines
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	// T11 修复：CJK 字符终端占 2 列。原按 rune 计数会让 CJK 标题超宽错位。
	// 用 runewidth 按显示宽度截断。
	w := runewidth.StringWidth(s)
	if w <= n {
		return s
	}
	var b strings.Builder
	cur := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if cur+rw > n-1 { // 留 1 列给省略号
			break
		}
		b.WriteRune(r)
		cur += rw
	}
	b.WriteString("…")
	return b.String()
}
