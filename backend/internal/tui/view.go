package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/board"
)

// View renders the entire TUI.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	// 布局尺寸约定：每个带边框面板固定 Height(3)（上边框+内容+下边框），
	// tabs 单行高 1。contentH 为剩余高度，最小 4 防止终端过小时挤压为 0。
	topH := 3    // bordered top bar (border + content + border)
	inputH := 3  // bordered input bar
	tabsH := 1
	contentH := m.height - topH - inputH - tabsH
	if contentH < 4 {
		contentH = 4
	}

	mainRow := m.renderChat(m.width, contentH)

	if m.flash != "" && time.Now().After(m.flashUntil) {
		m.flash = ""
	}

	// Join rows with exact placement: top bar, chat area, input bar, tabs.
	// Each bordered row is forced to Height(3) so content never wraps and pushes
	// the chat below the input bar.
	view := lipgloss.JoinVertical(lipgloss.Top,
		m.renderTopBar(m.width),
		mainRow,
		m.renderInput(m.width),
		m.renderTabs(m.width),
	)
	if m.overlay != overlayNone {
		overlay := m.renderOverlay(m.width, m.height-2)
		view = lipgloss.JoinVertical(lipgloss.Left, view, overlay)
	}
	return view
}

func (m Model) renderTopBar(w int) string {
	s := m.selectedSession()
	status := "idle"
	goal := ""
	if s != nil {
		status = s.Status
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

	mem := "-"
	if m.rt != nil && m.rt.Watchdog != nil {
		if d := m.rt.Watchdog.LastNonOK(); d != nil {
			mem = fmt.Sprintf("%d", d.Tokens)
		}
	}

	modelName := m.modelName
	if modelName == "" {
		modelName = "mock"
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
		m.styles.StatLabel.Render("Mem:"), mem,
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
		// Pad to height.
		return m.clipChat(strings.Join(lines, "\n"), w, h)
	}

	items := chatItems(s)
	cursor := m.chatCursor
	if m.chatFollowBottom {
		cursor = len(items) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {
		cursor = len(items) - 1
	}

	visibleCount := h
	if visibleCount < 1 {
		visibleCount = 1
	}
	total := len(items)
	end := cursor + 1
	if end < visibleCount {
		end = visibleCount
	}
	if end > total {
		end = total
	}
	start := end - visibleCount
	if start < 0 {
		start = 0
	}
	visible := items[start:end]

	for i, item := range visible {
		globalIdx := start + i
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
		}
		marker := "  "
		if m.focus == panelChat && globalIdx == cursor {
			marker = m.styles.TreeActive.Render("▸ ")
		}
		lines = append(lines, fmt.Sprintf("%s%s %s", marker, m.styles.Dim.Render(icon), roleStyle.Render(item.title)))
		for _, l := range strings.Split(item.detail, "\n") {
			lines = append(lines, "    "+truncate(l, w-6))
		}
	}

	if len(items) == 0 {
		lines = append(lines, "  "+m.styles.Dim.Render("(empty — send a message below)"))
	}

	return m.clipChat(strings.Join(lines, "\n"), w, h)
}

// clipChat applies a fixed Width/Height style so the borderless chat area
// never grows beyond its allocated space and pushes the input bar down.
// 替代旧 padLines：lipgloss Height 会自动截断超出部分并补齐空白，比手工 pad/tail 更稳定。
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
	return border.Width(w).Height(3).Render(left + text + flash)
}

func (m Model) renderTabs(w int) string {
	return m.styles.HelpBar.Width(w).Render(" " + bottomTabs + " ")
}

func (m Model) renderOverlay(w, h int) string {
	if m.overlay == overlayHelp {
		m.overlayTitle = "Help"
		m.overlayLines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
	// Refresh dynamic popup content every render so plan/agents stay live.
	if m.overlay == overlayPlan {
		m.overlayLines = m.buildPlanLines()
	} else if m.overlay == overlayAgents {
		m.overlayLines = m.buildAgentsLines()
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

var _ = board.TaskDone
