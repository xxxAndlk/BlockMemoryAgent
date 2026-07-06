package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

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

	items := m.collectChatItems()
	if len(items) == 0 {
		if m.selectedSession() == nil {
			return m.renderWelcome(contentW, h)
		}
		// 有会话但暂无消息/事件，且无本地预展示消息时，在对话区顶部显示首页提示。
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
			topH = h*55/100
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
	if s != nil && m.rt != nil && m.rt.Boards != nil {
		if b := m.rt.Boards.Get(s.ID); b != nil {
			snap = b.Snapshot()
		}
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
		domainStatus := m.deriveDomainTaskStatuses()
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
		titlePart := prefix + t.Title
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
	for _, node := range m.agentsNodes {
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

func (m Model) renderAgentsPanel(w, h int) string {
	titleLeft := "Agent 编排"
	titleRight := "[A] 关闭"
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

	var lines []string
	if len(m.agentsNodes) == 0 {
		lines = append(lines, "(no agents)")
	} else {
		for _, node := range m.agentsNodes {
			// 过滤掉大量已完成且无目标的无意义临时助手，避免面板被刷屏。
			if node.depth >= 2 && node.goal == "" &&
				(node.status == enums.RoleStatusDone || node.status == enums.RoleStatusIdle) {
				continue
			}

			prefix := agentTreePrefix(node.depth)
			icon := statusIcon(string(node.status))
			badge := agentStatusBadge(m.styles, node.status)
			name := node.name
			if node.goal != "" {
				goal := strings.ReplaceAll(node.goal, "\n", " ")
				name += " " + m.styles.Dim.Render(truncate(goal, innerW-lipgloss.Width(prefix)-lipgloss.Width(name)-lipgloss.Width(badge)-4))
			}
			line := fmt.Sprintf("%s%s %s %s", prefix, icon, name, badge)
			lines = append(lines, truncate(line, innerW))
		}
	}

	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// collectChatItems 收集当前应展示的全部 chatItem，包含真实会话消息/事件，
// 以及尚未同步到服务端的本地预展示首条用户消息。
func (m *Model) collectChatItems() []chatItem {
	s := m.selectedSession()
	var items []chatItem
	if s != nil {
		items = chatItems(s, true)
	}
	// 本地预展示的首条用户消息：无会话时直接展示；有会话但服务端尚未同步该
	// 消息时，也作为兜底展示，避免用户输入"消失"。
	if m.pendingFirstMessage != "" {
		already := false
		for _, it := range items {
			if strings.TrimPrefix(it.title, "> ") == m.pendingFirstMessage {
				already = true
				break
			}
		}
		if !already {
			// 尽量使用会话开始时间作为时间戳，保证排序自然
			ts := time.Now()
			if s != nil {
				ts = s.StartedAt
			}
			items = append(items, chatItem{
				title:     "> " + m.pendingFirstMessage,
				timestamp: ts,
				isEvent:   false,
				role:      enums.ChatRoleUser,
			})
		}
	}
	return items
}

// buildChatContent 把当前会话的全部 chatItem 渲染成 viewport 可滚动的字符串，
// 并同步更新 m.chatItemOffsets。
func (m *Model) buildChatContent(width int) string {
	items := m.collectChatItems()
	if len(items) == 0 {
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
			prefix := tsStyled + " " + label + " "
			indent := strings.Repeat(" ", lipgloss.Width(ts)+1) + strings.Repeat(" ", lipgloss.Width(label)+1)
			availW := width - lipgloss.Width(prefix)
			if availW < 4 {
				availW = 4
			}
			wrappedLines := wrapToWidth(content, availW)
			for j, wl := range wrappedLines {
				styledWl := m.styles.LogUser.Render(wl)
				if j == 0 {
					titleLines = append(titleLines, prefix+styledWl)
				} else {
					titleLines = append(titleLines, indent+styledWl)
				}
			}
		case strings.HasPrefix(item.title, "[●] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.LogWarn, width)...)
		case strings.HasPrefix(item.title, "[✓] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.LogSuccess, width)...)
		case strings.HasPrefix(item.title, "[✗] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.LogError, width)...)
		case strings.HasPrefix(item.title, "🧠 recalled: "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.Dim, width)...)
		case strings.HasPrefix(item.title, "─── ") && strings.HasSuffix(item.title, " ───"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.CallStack, width)...)
		case strings.HasPrefix(item.title, "✗ Error"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.LogError, width)...)
		case item.isEvent:
			// 事件标题已自带 Agent 名称（如 MetaAgent: ... / code_assistant 完成: ...）
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, m.styles.LogInfo, width)...)
		default:
			label := m.styles.LogAssistant.Render("Assistant")
			availW := width - lipgloss.Width(label) - lipgloss.Width(ts) - 3
			if availW < 4 {
				availW = 4
			}
			text := formatMarkdown(item.title)
			// 对 Assistant 长回答做自动换行，避免截断；使用 ANSI 感知的按显示宽度换行，支持中文/长串。
			wrappedLines := wrapToWidth(text, availW)
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

		// detail 行统一缩进并换行；空 detail 跳过避免标题与详情重复（non-tool 事件 detail 为空）
		if item.detail != "" {
			for _, l := range displayDetailLines(item.title, item.detail) {
				wrappedLines := wrapToWidth(l, width-2)
				for _, wl := range wrappedLines {
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

// inputIsMultiline 判断当前输入是否包含换行（粘贴大段/多行内容）。
func (m *Model) inputIsMultiline() bool {
	return strings.ContainsRune(string(m.inputRunes), '\n')
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
	} else if m.inputIsMultiline() {
		// 多行内容（粘贴或 Alt+Enter）在输入栏折叠为占位提示，避免大段文本挤占界面。
		lines := strings.Count(string(m.inputRunes), "\n") + 1
		text = m.styles.InputText.Render(fmt.Sprintf("[%d行内容]%s", lines, cursor))
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
	// border 占 2 列，Width 设置的是内部宽度，因此请求 w-2 以保证总宽度为 w。
	return border.Width(w - 2).Height(3).Render(content)
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
