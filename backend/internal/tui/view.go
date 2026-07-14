package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// Note: board snapshot access now goes through agent.Query("board") so the TUI
// no longer needs *runtime.Runtime directly.

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

	if m.overlayPanel.mode != overlayNone {
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
	case "completed":
		statusColor = cStatusDone
	}
	// 完成态用 ✓ 替代 ●，让用户一眼看出会话已结束
	statusIcon := "●"
	if status == "completed" {
		statusIcon = "✓"
	}
	statusDot := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Render(statusIcon)
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

func (m Model) renderRightPanels(w, h int) string {
	if w < 20 {
		w = 20
	}

	// 右侧面板始终同时展示计划与 Agent 编排两个面板。
	// 即使终端高度紧张，也优先保证两个面板都有可见区域（标题+至少一行内容），
	// 避免计划栏被完全丢弃导致"内容没有显示"。
	topH := h * 55 / 100
	if topH < 3 {
		topH = 3
	}
	bottomH := h - topH
	if bottomH < 3 {
		bottomH = 3
	}
	// 极小高度下两者之和可能超过 h，按 h 裁剪并确保计划面板至少 2 行。
	if topH+bottomH > h {
		if h >= 5 {
			topH = h * 55 / 100
			if topH < 3 {
				topH = 3
			}
			bottomH = h - topH
			if bottomH < 2 {
				bottomH = 2
			}
		} else {
			topH = h/2 + h%2
			bottomH = h / 2
			if topH < 2 {
				topH = 2
			}
			if bottomH < 2 {
				bottomH = 2
			}
		}
	}
	return lipgloss.JoinVertical(lipgloss.Top, m.renderPlanPanel(w, topH), m.renderAgentsPanel(w, bottomH))
}

func (m Model) renderPlanPanel(w, h int) string {
	titleLeft := "执行计划"
	titleRight := "[P] 关闭"
	titlePadding := w - lipgloss.Width(titleLeft) - lipgloss.Width(titleRight) - 2
	if titlePadding < 1 {
		titlePadding = 1
	}
	headerText := titleLeft + strings.Repeat(" ", titlePadding) + titleRight
	header := m.styles.PanelHeader.Width(w).Render(headerText)
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}

	s := m.selectedSession()
	var snap board.Snapshot
	if s != nil {
		snap = m.boardSnapshot(s.ID)
	}

	// 没有看板时（如 direct_tool），用会话目标生成一个最小计划视图，
	// 避免右侧面板出现空白的 "(no plan)"。
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
	} else {
		// 看板子任务状态可能未被后端及时更新，用 Agent 实例的真实状态覆盖，
		// 这样进度条和状态才能反映实际完成情况。
		domainStatus := m.agentTreePanel.deriveDomainTaskStatuses()
		tasks := make([]board.SubTask, len(snap.Tasks))
		copy(tasks, snap.Tasks)
		for i := range tasks {
			domain := planTaskDomain(tasks[i].Title)
			if st, ok := domainStatus[domain]; ok {
				tasks[i].Status = st
			}
		}
		snap.Tasks = tasks
	}

	lines := m.formatPlanSnapshot(innerW, snap)
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// formatPlanSnapshot 把看板快照渲染成计划面板内的文本行。
func (m Model) formatPlanSnapshot(innerW int, snap board.Snapshot) []string {
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

	var lines []string
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
		statusText := planStatusText(t.Status)
		var elapsed string
		if t.Status == board.TaskPending {
			elapsed = "--:--"
		} else {
			elapsed = formatTaskElapsed(t)
		}
		meta := fmt.Sprintf("%s %s", icon+statusText, elapsed)
		// 保留序号+标题，右侧对齐状态与耗时
		// P3-3：过长标题先经 LLM 语义精简，再按显示宽度截断
		briefTitle := m.summarizeTaskTitle(t.Title)
		titlePart := prefix + briefTitle
		avail := innerW - lipgloss.Width(meta) - 1
		if avail < lipgloss.Width(prefix)+4 {
			avail = lipgloss.Width(prefix) + 4
		}
		titlePart = truncate(titlePart, avail)
		padding := innerW - lipgloss.Width(titlePart) - lipgloss.Width(meta)
		if padding < 1 {
			padding = 1
		}
		line := titlePart + strings.Repeat(" ", padding) + meta
		if i == current {
			line = m.styles.StatValue.Render(truncate(line, innerW))
		} else if t.Status == board.TaskDone {
			line = m.styles.Dim.Render(truncate(line, innerW))
		} else {
			line = truncate(line, innerW)
		}
		lines = append(lines, line)
	}
	return lines
}

// deriveDomainTaskStatuses 从 Agent 拓扑中汇总每个领域的实际状态，
// 用于覆盖 TaskBoard 中可能未被后端更新的子任务状态。
// 优先级：Failed > InProgress > Done > Pending。
func (m Model) deriveDomainTaskStatuses() map[string]board.TaskStatus {
	status := make(map[string]board.TaskStatus)
	for _, node := range m.agentTreePanel.nodes {
		if node.domain == "" {
			continue
		}
		var st board.TaskStatus
		switch node.status {
		case enums.RoleStatusError:
			st = board.TaskFailed
		case enums.RoleStatusActive:
			st = board.TaskInProgress
		case enums.RoleStatusDone:
			st = board.TaskDone
		default:
			st = board.TaskPending
		}
		cur := status[node.domain]
		status[node.domain] = strongerTaskStatus(cur, st)
	}
	return status
}

// strongerTaskStatus 返回两个任务状态中优先级更高的一个。
func strongerTaskStatus(a, b board.TaskStatus) board.TaskStatus {
	order := map[board.TaskStatus]int{
		board.TaskFailed:     3,
		board.TaskInProgress: 2,
		board.TaskBlocked:    2,
		board.TaskDone:       1,
		board.TaskPending:    0,
	}
	if order[b] > order[a] {
		return b
	}
	return a
}

// planTaskDomain 从看板子任务标题（格式 "领域名 - 目标"）中提取领域名。
func planTaskDomain(title string) string {
	parts := strings.SplitN(title, " - ", 2)
	if len(parts) == 0 {
		return title
	}
	return strings.TrimSpace(parts[0])
}

// agentCardLine 渲染单个 Agent 卡片行，返回可能占多行的字符串切片。
// 参考“新TUI页.png”设计：状态色点 + 按角色着色的名称 + 状态徽章 + 时间戳 + 任务描述。
// agentRoleColor 返回不同 Agent 类型的主题色。
// statusColor 返回状态对应的颜色。
func statusColor(status string) string {
	switch status {
	case "running", string(enums.RoleStatusActive), string(board.TaskInProgress):
		return cStatusRun
	case "awaiting_clarify", string(enums.RoleStatusWaiting), string(board.TaskBlocked):
		return cStatusWait
	case "completed", string(board.TaskDone):
		return cStatusDone
	case "error", string(board.TaskFailed):
		return cStatusErr
	default:
		return cStatusIdle
	}
}

// agentTreePrefix 根据节点在扁平树中的位置生成树状连接符前缀。
// collectChatItems 收集当前应展示的全部 chatItem，包含真实会话消息/事件，
// 以及尚未同步到服务端的本地预展示首条用户消息。
// buildChatContent 把当前会话的全部 chatItem 渲染成 viewport 可滚动的字符串，
// 并同步更新 m.chatPanel.itemOffsets。
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
		snap = m.boardSnapshot(s.ID)
	}
	line := formatPlanBar(m.styles, snap, toolLabel)
	return lipgloss.NewStyle().Width(w).Height(1).Render(line)
}

// inputIsMultiline 判断当前输入是否包含换行（粘贴大段/多行内容）。
func (m Model) renderShortcutBar(w int) string {
	shortcuts := []struct {
		key   string
		label string
	}{
		{"K", "Command Palette"},
		{"P", "Plan"},
		{"A", "Agents"},
		{"L", "Logs"},
		{"B", "Side Panel"},
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

// wrapStyledLine 将单行原始文本按宽度换行，并在首行保留时间戳前缀，续行保持对齐。
// style 为整行文本应用的颜色样式；width 为对话区总宽度。
func wrapStyledLine(tsStyled, text string, style lipgloss.Style, width int) []string {
	prefix := tsStyled + " "
	availW := width - lipgloss.Width(prefix)
	if availW < 4 {
		availW = 4
	}
	wrappedLines := wrapToWidth(text, availW)
	var out []string
	tsW := lipgloss.Width(tsStyled)
	for i, wl := range wrappedLines {
		styledWl := style.Render(wl)
		if i == 0 {
			out = append(out, prefix+styledWl)
		} else {
			out = append(out, strings.Repeat(" ", tsW+1)+styledWl)
		}
	}
	return out
}

// wrapToWidth 按显示宽度将字符串换行，保留 ANSI 转义序列与原有换行；
// 对中文、长 URL 等无空格内容也能在边界处正确折断，避免截断。
func wrapToWidth(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	var lines []string
	var b strings.Builder
	curW := 0
	inAnsi := false
	for _, r := range s {
		if r == '\x1b' {
			inAnsi = true
			b.WriteRune(r)
			continue
		}
		if inAnsi {
			b.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		if r == '\n' {
			lines = append(lines, b.String())
			b.Reset()
			curW = 0
			continue
		}
		rw := runewidth.RuneWidth(r)
		// 当前行已有内容且加入该 rune 会超宽时，先换行
		if curW > 0 && curW+rw > width {
			lines = append(lines, b.String())
			b.Reset()
			curW = 0
		}
		b.WriteRune(r)
		curW += rw
	}
	if b.Len() > 0 {
		lines = append(lines, b.String())
	}
	return lines
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
