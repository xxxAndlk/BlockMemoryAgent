package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/board"
)

// View renders the entire TUI in single-column chat-focused layout.
func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}
	return m.singleColumnView()
}

// singleColumnView v2.0 主布局：顶部状态栏 + 主对话区 + 计划进度栏 + 输入栏 + 底部标签。
// 当 agentPanelVisible 为 true 时，主对话区与右侧 Agent 面板分两列显示。
func (m Model) singleColumnView() string {
	topH := 1
	inputH := 3
	tabsH := 1
	planH := 0
	if m.planBarVisible {
		planH = 1
	}

	// 弹窗打开时预先扣减聊天区高度，确保总高度 ≤ m.height，输入框始终可见。
	overlayH := 0
	if m.overlay != overlayNone {
		overlayH = m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
	}

	contentH := m.height - topH - inputH - tabsH - planH - overlayH
	if contentH < 4 {
		contentH = 4
	}

	var mainRow string
	if m.agentPanelVisible {
		// 文档 §2.5：Agent 面板展开时主对话区 80% → 60%，面板占 40%
		chatW := m.width * 3 / 5
		agentW := m.width - chatW
		if agentW < 24 {
			agentW = 24
			chatW = m.width - agentW
		}
		chatPanel := m.renderChat(chatW, contentH)
		agentPanel := renderAgentPanel(&m, agentW)
		mainRow = lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, agentPanel)
	} else {
		mainRow = m.renderChat(m.width, contentH)
	}

	view := lipgloss.JoinVertical(lipgloss.Top,
		m.renderTopBar(m.width),
		mainRow,
	)
	if m.planBarVisible {
		view = lipgloss.JoinVertical(lipgloss.Top, view, m.renderPlanBar(m.width))
	}
	view = lipgloss.JoinVertical(lipgloss.Top, view, m.renderInput(m.width), m.renderTabs(m.width))

	if overlayH > 0 {
		overlay := m.renderOverlay(m.width, overlayH)
		view = lipgloss.JoinVertical(lipgloss.Left, view, overlay)
	}
	return view
}

func (m Model) renderTopBar(w int) string {
	s := m.selectedSession()
	status := "idle"
	sessionID := ""
	if s != nil {
		status = string(s.Status)
		sessionID = s.ID
	}
	agentCount := len(m.agentsNodes)
	if s != nil && s.State != nil && s.State.PendingClarify != nil {
		agentCount++
	}
	line := formatTopBar(m.styles, sessionID, status, agentCount, m.totalInputTokens, m.totalOutputTokens)
	return lipgloss.NewStyle().Width(w).Height(1).Render(line)
}

func (m Model) renderChat(w, h int) string {
	const scrollbarW = 1
	contentW := w - scrollbarW - 1 // 1 列间隔
	if contentW < 4 {
		contentW = w
	}

	s := m.selectedSession()
	if s == nil {
		lines := []string{
			"",
			"  " + m.styles.Dim.Render("No active session."),
			"  " + m.styles.Dim.Render("Type /new <your goal> to start a conversation."),
		}
		content := m.clipChat(strings.Join(lines, "\n"), contentW, h)
		return lipgloss.JoinHorizontal(lipgloss.Top, content, m.renderScrollbar(w-contentW, h, 0, 0, 0))
	}

	items := chatItems(s)
	if len(items) == 0 {
		lines := []string{"  " + m.styles.Dim.Render("(empty — send a message below)")}
		content := m.clipChat(strings.Join(lines, "\n"), contentW, h)
		return lipgloss.JoinHorizontal(lipgloss.Top, content, m.renderScrollbar(w-contentW, h, 0, 0, 0))
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

	// Render items.
	type renderedItem struct {
		lines []string
	}
	rendered := make([]renderedItem, len(items))
	for i, item := range items {
		var ls []string
		style := m.styles.LogInfo
		switch {
		case strings.HasPrefix(item.title, "> "):
			// 用户输入：高亮前缀 >
			style = m.styles.LogUser
			ls = append(ls, style.Render(truncate(item.title, contentW-2)))
		case strings.HasPrefix(item.title, "[●] "):
			// 工具调用中：黄色
			ls = append(ls, m.styles.LogWarn.Render(truncate(item.title, contentW-2)))
		case strings.HasPrefix(item.title, "[✓] "):
			// 工具调用成功：绿色
			ls = append(ls, m.styles.LogSuccess.Render(truncate(item.title, contentW-2)))
		case strings.HasPrefix(item.title, "[✗] "):
			// 工具调用失败：红色
			ls = append(ls, m.styles.LogError.Render(truncate(item.title, contentW-2)))
		case strings.HasPrefix(item.title, "🧠 recalled: "):
			// 记忆召回：灰色
			ls = append(ls, m.styles.Dim.Render(truncate(item.title, contentW-2)))
		case strings.HasPrefix(item.title, "─── ") && strings.HasSuffix(item.title, " ───"):
			// 话题切换：蓝色/强调色，居中
			line := truncate(item.title, contentW-2)
			ls = append(ls, m.styles.CallStack.Render(line))
		case strings.HasPrefix(item.title, "✗ Error"):
			ls = append(ls, m.styles.LogError.Render(truncate(item.title, contentW-2)))
		default:
			// Agent 响应：普通文本（仍做轻量 Markdown 格式化）
			content := formatMarkdown(item.title)
			ls = append(ls, content)
		}
		detailLines := displayDetailLines(item.title, item.detail)
		for _, l := range detailLines {
			ls = append(ls, "  "+truncate(l, contentW-4))
		}
		rendered[i] = renderedItem{lines: ls}
	}
	endLine := startLine + viewportH
	if endLine > totalLines {
		endLine = totalLines
	}

	var lines []string
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
		scrollable := totalLines - viewportH
		if scrollable < 1 {
			scrollable = 1
		}
		if endLine >= totalLines {
			pct = 100
		} else {
			pct = startLine * 100 / scrollable
		}
		hint := m.styles.Dim.Render(fmt.Sprintf(" [%d%%] ↑↓/j/k 滚动 ", pct))
		if len(lines) > 0 {
			lines[0] = lines[0] + " " + hint
		}
	}

	content := m.clipChat(strings.Join(lines, "\n"), contentW, h)
	bar := m.renderScrollbar(w-contentW, h, viewportH, totalLines, startLine)
	return lipgloss.JoinHorizontal(lipgloss.Top, content, bar)
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
	text := string(m.inputRunes[:m.inputCursor]) + cursor + string(m.inputRunes[m.inputCursor:])
	flash := ""
	// 持锁读 flash（T2 修复：后台 HTTP goroutine 可能并发写）
	m.flashMu.Lock()
	curFlash := m.flash
	m.flashMu.Unlock()
	if curFlash != "" {
		flash = "  " + m.styles.LogError.Render(curFlash)
	}
	border := m.styles.BlurBorder
	if m.focus == panelInput {
		border = m.styles.FocusBorder
	}
	hint := m.styles.Dim.Render("Tab 面板  ↑↓历史  Alt+Enter 换行  /help  Q 退出")
	content := lipgloss.JoinVertical(lipgloss.Left, left+text+flash, " "+hint)
	return border.Width(w).Height(3).Render(content)
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
