package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/board"
)

// View renders the entire TUI in single-column chat-focused layout.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.singleColumnView()
}

// singleColumnView 窄终端单列布局（原始布局，向后兼容）。
func (m Model) singleColumnView() string {
	topH := 3
	inputH := 4
	tabsH := 1

	// 弹窗打开时预先扣减聊天区高度，确保总高度 ≤ m.height，输入框始终可见。
	overlayH := 0
	if m.overlay != overlayNone {
		overlayH = m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
	}

	contentH := m.height - topH - inputH - tabsH - overlayH
	if contentH < 4 {
		contentH = 4
	}

	mainRow := m.renderChat(m.width, contentH)

	view := lipgloss.JoinVertical(lipgloss.Top,
		m.renderTopBar(m.width),
		mainRow,
		m.renderInput(m.width),
		m.renderTabs(m.width),
	)
	if overlayH > 0 {
		overlay := m.renderOverlay(m.width, overlayH)
		view = lipgloss.JoinVertical(lipgloss.Left, view, overlay)
	}
	return view
}

func (m Model) renderTopBar(w int) string {
	s := m.selectedSession()
	status := "idle"
	goal := ""
	if s != nil {
		status = string(s.Status)
		goal = s.Goal
	}

	tasksDone, tasksTotal := 0, 0
	if s != nil && m.rt != nil && m.rt.Boards != nil {
		if b := m.rt.Boards.Get(s.ID); b != nil {
			snap := b.Snapshot()
			tasksTotal = len(snap.Tasks)
			for _, t := range snap.Tasks {
				if t.Status == board.TaskDone {
					tasksDone++
				}
			}
		}
	}

	modelName := m.modelName
	if modelName == "" {
		modelName = "mock"
	}

	tokenStr := "-"
	if m.totalInputTokens > 0 || m.totalOutputTokens > 0 {
		tokenStr = fmt.Sprintf("in %d out %d", m.totalInputTokens, m.totalOutputTokens)
	}

	// Build status segment with badges.
	planBadge := ""
	if tasksTotal > 0 {
		planBadge = " " + m.styles.Badge.Render(fmt.Sprintf("Plan %d/%d", tasksDone, tasksTotal))
	}
	agentsBadge := ""
	if n := len(m.agentsNodes); n > 0 {
		agentsBadge = " " + m.styles.Badge.Render(fmt.Sprintf("Agents %d", n))
	}
	if s != nil && s.State != nil && s.State.PendingClarify != nil {
		agentsBadge += " " + m.styles.BadgeWarn.Render("Clarify?")
	}

	text := fmt.Sprintf(" %s │ %s %s %s │ %s %s ",
		m.styles.Title.Render("BlockMemoryAgent"),
		m.styles.StatLabel.Render("Model:"), m.styles.StatValue.Render(modelName),
		m.styles.StatLabel.Render("Tokens:"), m.styles.StatValue.Render(tokenStr),
		m.styles.StatValue.Render(statusIcon(status)+" "+status),
	)
	if planBadge != "" || agentsBadge != "" {
		text = strings.TrimRight(text, " ") + planBadge + agentsBadge + " "
	}
	if goal != "" {
		text = strings.TrimRight(text, " ") + " │ " + m.styles.Dim.Render(truncate(goal, 40)) + " "
	}
	return m.styles.FocusBorder.Width(w).Height(3).Render(strings.TrimRight(text, " "))
}

func (m Model) renderChat(w, h int) string {
	var lines []string

	s := m.selectedSession()
	if s == nil {
		lines = append(lines, "")
		lines = append(lines, "  "+m.styles.Dim.Render("No active session."))
		lines = append(lines, "  "+m.styles.Dim.Render("Type /new <your goal> to start a conversation."))
		return m.clipChat(strings.Join(lines, "\n"), w, h)
	}

	items := chatItems(s)
	if len(items) == 0 {
		lines = append(lines, "  "+m.styles.Dim.Render("(empty — send a message below)"))
		return m.clipChat(strings.Join(lines, "\n"), w, h)
	}

	viewportH := h
	if viewportH < 1 {
		viewportH = 1
	}

	// Pre-compute item start lines to derive cursor from chatScrollLine.
	itemLineCount := make([]int, len(items))
	itemStartLine := make([]int, len(items))
	totalLines := 0
	for i, item := range items {
		itemStartLine[i] = totalLines
		n := 1 + len(displayDetailLines(item.title, item.detail))
		itemLineCount[i] = n
		totalLines += n
	}

	// Clamp scroll position.
	startLine := m.chatScrollLine
	if m.chatFollowBottom || startLine > totalLines-viewportH {
		startLine = totalLines - viewportH
	}
	if startLine < 0 {
		startLine = 0
	}
	// Derive highlighted cursor from viewport center line.
	centerLine := startLine + viewportH/3
	cursor := 0
	if m.chatFollowBottom {
		cursor = len(items) - 1
	} else {
		for i := len(items) - 1; i >= 0; i-- {
			if itemStartLine[i] <= centerLine {
				cursor = i
				break
			}
		}
	}

	// Render items.
	type renderedItem struct {
		lines []string
	}
	rendered := make([]renderedItem, len(items))
	for i, item := range items {
		roleStyle := m.styles.LogInfo
		icon := "·"
		switch {
		case strings.HasPrefix(item.title, "[user]"):
			roleStyle = m.styles.LogUser
			icon = ">"
		case strings.HasPrefix(item.title, "[assistant]"):
			roleStyle = m.styles.LogAssistant
			icon = "<"
		case strings.HasPrefix(item.title, "[system]"):
			roleStyle = m.styles.LogSystem
			icon = "#"
		case strings.HasPrefix(item.title, "🔧"):
			if strings.Contains(item.title, " ✗ ·") {
				roleStyle = m.styles.LogError
			} else {
				roleStyle = m.styles.LogSuccess
			}
			icon = ""
		case strings.HasPrefix(item.title, "💭"):
			roleStyle = m.styles.LogInfo
			icon = ""
		case strings.HasPrefix(item.title, "✗ Error"):
			roleStyle = m.styles.LogError
			icon = ""
		}
		marker := "  "
		if m.focus == panelChat && i == cursor {
			marker = m.styles.TreeActive.Render("▸ ")
		}
		var ls []string
		titleStr := truncate(item.title, w-8)
		ls = append(ls, fmt.Sprintf("%s%s %s", marker, m.styles.Dim.Render(icon), roleStyle.Render(titleStr)))
		detailLines := displayDetailLines(item.title, item.detail)
		for _, l := range detailLines {
			ls = append(ls, "    "+truncate(l, w-6))
		}
		rendered[i] = renderedItem{lines: ls}
	}
	endLine := startLine + viewportH
	if endLine > totalLines {
		endLine = totalLines
	}

	skip := startLine
	for i := range rendered {
		ls := rendered[i].lines
		if skip >= len(ls) {
			skip -= len(ls)
			continue
		}
		take := ls[skip:]
		remaining := endLine - (itemStartLine[i] + skip)
		if remaining < len(take) {
			if remaining < 0 {
				remaining = 0
			}
			take = take[:remaining]
		}
		lines = append(lines, take...)
		skip = 0
		if len(lines) >= viewportH {
			break
		}
	}

	if totalLines > viewportH {
		pct := 0
		if endLine >= totalLines {
			pct = 100
		} else if startLine > 0 {
			pct = startLine * 100 / totalLines
		}
		hint := m.styles.Dim.Render(fmt.Sprintf(" [%d%%] ↑↓/j/k 滚动 ", pct))
		if len(lines) > 0 {
			lines[0] = lines[0] + " " + hint
		}
	}

	return m.clipChat(strings.Join(lines, "\n"), w, h)
}

// clipChat applies a fixed Width/Height style so the borderless chat area
// never grows beyond its allocated space and pushes the input bar down.
func (m Model) clipChat(content string, w, h int) string {
	return lipgloss.NewStyle().Width(w).Height(h).Render(content)
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
	text := string(m.inputRunes[:m.inputCursor]) + cursor + string(m.inputRunes[m.inputCursor:])
	flash := ""
	if m.flash != "" {
		flash = "  " + m.styles.LogError.Render(m.flash)
	}
	border := m.styles.BlurBorder
	if m.focus == panelInput {
		border = m.styles.FocusBorder
	}
	hint := m.styles.Dim.Render("/new /cancel /interrupt /enqueue /clarify  ↑↓历史  esc返回")
	content := lipgloss.JoinVertical(lipgloss.Left, left+text+flash, " "+hint)
	return border.Width(w).Height(4).Render(content)
}

func (m Model) renderTabs(w int) string {
	return m.styles.HelpBar.Width(w).Render(" " + bottomTabs + " ")
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
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
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
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
