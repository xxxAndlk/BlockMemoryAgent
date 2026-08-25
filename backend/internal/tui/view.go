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

// 注：看板快照通过 agent.Tree（权威 Agent 树）构建，TUI 不再需要直接依赖 *runtime.Runtime。

// View 渲染整个 TUI，采用单列以对话为主的布局。
func (m Model) View() string {
	// 终端尺寸未就绪时显示初始化提示。
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
	// 根据右侧面板可见性决定主内容区布局。
	if m.rightPanelVisible() {
		chatW := m.chatAreaWidth()
		rightW := m.rightPanelWidth()
		chatPanel := m.renderChat(chatW, contentH)
		rightPanel := m.renderRightPanels(rightW, contentH)
		mainRow = lipgloss.JoinHorizontal(lipgloss.Top, chatPanel, rightPanel)
	} else {
		mainRow = m.renderChat(m.width, contentH)
	}

	// 垂直拼接：顶栏、主内容区、问答面板（有待答复澄清时）、Token 统计栏、输入栏、快捷键栏。
	parts := []string{
		m.renderTopBar(m.width),
		mainRow,
	}
	if panel := m.renderClarifyPanel(m.width); panel != "" {
		parts = append(parts, panel)
	}
	parts = append(parts,
		m.renderTokenBar(m.width),
		m.renderInput(m.width),
		m.renderShortcutBar(m.width),
	)
	view := lipgloss.JoinVertical(lipgloss.Top, parts...)

	// 若有弹窗，则在底部追加覆盖层。
	if m.overlayPanel.mode != overlayNone {
		overlayH := m.height / 3
		if overlayH < 6 {
			overlayH = 6
		}
		overlay := m.renderOverlay(m.width, overlayH)
		view = lipgloss.JoinVertical(lipgloss.Left, view, overlay)
	}

	// 安全网：任何一行显示宽度超过终端都会物理折行，把后续内容整体顶下去
	// （实证：顶栏/快捷键栏超宽折行使整帧比终端高，输入栏被挤出可视区）。
	// 对最终帧逐行硬裁剪；总行数超高时由 fitFrameLines 压缩到终端高度。
	vlines := strings.Split(view, "\n")
	for i, l := range vlines {
		vlines[i] = hardClipLine(l, m.width)
	}
	return strings.Join(fitFrameLines(vlines, m.height), "\n")
}

// fitFrameLines 将整帧裁剪到终端高度：顶栏（第 0 行）常驻、底部输入栏/快捷键栏
// 优先可见，溢出行从中间对话区丢弃。行数不超高时原样返回。
// （原实现只保留底部 N 行，超高时顶栏与对话区首行被裁掉——顶栏此后常驻。）
func fitFrameLines(vlines []string, height int) []string {
	if height <= 0 || len(vlines) <= height {
		return vlines
	}
	kept := make([]string, 0, height)
	kept = append(kept, vlines[0])
	kept = append(kept, vlines[len(vlines)-height+1:]...)
	return kept
}

// renderTopBar 渲染顶部状态栏，展示版本、模型、会话、状态等信息。
func (m Model) renderTopBar(w int) string {
	// 左侧：版本、模型、Memory、Skills、MCP、Workspace
	version := m.styles.TopBarLabel.Render("BlockMemoryAgent") + m.styles.TopBarSep.Render(" v0.8.0")
	modelLabel := m.styles.TopBarLabel.Render("Model") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render(m.modelName)
	memory := m.styles.TopBarLabel.Render("Memory") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarStatus.Render("On")
	skills := m.styles.TopBarLabel.Render("Skills") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("38")
	mcp := m.styles.TopBarLabel.Render("MCP") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render("12")
	workspace := m.styles.TopBarLabel.Render("Workspace") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render(m.workDir)
	left := lipgloss.JoinHorizontal(lipgloss.Left, version, "  ", modelLabel, "  ", memory, "  ", skills, "  ", mcp, "  ", workspace)

	// 右侧：Session、Status、Time
	s := m.selectedSession()
	status := "idle"
	sessionID := "-"
	if s != nil {
		status = string(s.Status)
		sessionID = "#" + s.ID
	}
	// 根据状态选择颜色。
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
	// 软停止销毁倒计时（TODO #37）：DestroyAt 非 nil 时显示"销毁 MM:SS"，
	// tick 每帧重算剩余时间（100ms tick 刷新链路现成）。
	if s != nil && s.DestroyAt != nil {
		remaining := time.Until(*s.DestroyAt)
		if remaining < 0 {
			remaining = 0
		}
		mm := int(remaining.Minutes())
		ss := int(remaining.Seconds()) % 60
		statusStr += m.styles.TopBarSep.Render(" | ") + m.styles.TopBarLabel.Render("销毁") + m.styles.TopBarValue.Render(fmt.Sprintf(" %02d:%02d", mm, ss))
	}
	// 运行时长：从会话开始时间实时计算，已结束的会话冻结在结束时刻。
	elapsed := "00:00:00"
	if s != nil && !s.StartedAt.IsZero() {
		end := time.Now()
		if s.EndedAt != nil {
			end = *s.EndedAt
		}
		elapsed = formatDurationHMS(end.Sub(s.StartedAt))
	}
	timeStr := m.styles.TopBarLabel.Render("Time") + m.styles.TopBarSep.Render(": ") + m.styles.TopBarValue.Render(elapsed)
	right := lipgloss.JoinHorizontal(lipgloss.Left, sessionStr, "  ", statusStr, "  ", timeStr)

	// 左右分栏，中间用空白填充。
	line := lipgloss.JoinHorizontal(lipgloss.Top, left, lipgloss.NewStyle().Width(w-lipgloss.Width(left)-lipgloss.Width(right)).Render(""), right)
	// 超出宽度时仅显示左侧，避免折行。
	if lipgloss.Width(line) > w {
		line = left
	}
	// 左侧自身也可能超宽（如深路径 Workspace），硬裁剪保证顶栏恒为 1 物理行：
	// 折行会让整帧比终端高，alt-screen 下底部输入栏被顶出屏幕。
	line = hardClipLine(line, w)
	return m.styles.TopBar.Width(w).Height(1).Render(line)
}

// rightPanelHeights 计算右侧上（计划）/下（Agent 编排）面板的高度分配。
// 参考"新TUI页.png"：Agent 编排为卡片式布局，需要更多纵向空间，计划面板占 45%、Agent 编排占 55%。
// 即使终端高度紧张，也优先保证两个面板都有可见区域（标题+至少一行内容），
// 避免计划栏被完全丢弃导致"内容没有显示"。
// 渲染（renderRightPanels）与鼠标滚轮命中测试（handleMouse）共用此函数，保证区域一致。
func rightPanelHeights(h int) (topH, bottomH int) {
	topH = h * 45 / 100
	if topH < 3 {
		topH = 3
	}
	bottomH = h - topH
	if bottomH < 3 {
		bottomH = 3
	}
	// 极小高度下两者之和可能超过 h，按 h 裁剪并确保计划面板至少 2 行。
	if topH+bottomH > h {
		if h >= 5 {
			topH = h * 45 / 100
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
	return topH, bottomH
}

// renderRightPanels 渲染右侧上下堆叠的计划面板与 Agent 编排面板。
func (m Model) renderRightPanels(w, h int) string {
	// 保证最小宽度。
	if w < 20 {
		w = 20
	}

	// 右侧面板始终同时展示计划与 Agent 编排两个面板。
	topH, bottomH := rightPanelHeights(h)
	// 严格按预算裁剪：终端高度紧张（如 h=12）时 topH/bottomH 只有 2~3 行，
	// 而面板最少渲染 4 行（标题+边框+1 行内容），不裁剪会把整帧撑高超终端、
	// 输入栏被挤出可视区。保留每块前 N 行（标题优先可见，箱底边被裁仅外观瑕疵）。
	plan := clipLinesTo(m.renderPlanPanel(w, topH), topH)
	agents := clipLinesTo(m.renderAgentsPanel(w, bottomH), bottomH)
	return lipgloss.JoinVertical(lipgloss.Top, plan, agents)
}

// clipLinesTo 保留 s 的前 n 行（不足 n 行原样返回）。
func clipLinesTo(s string, n int) string {
	if n < 0 {
		n = 0
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// renderPlanPanel 渲染右侧计划面板，展示目标、进度条与任务列表。
func (m Model) renderPlanPanel(w, h int) string {
	// 构建标题栏（参考"新TUI页.png"：带 📋 图标）。
	titleLeft := "📋 执行计划"
	titleRight := "[P] 关闭"
	titlePadding := w - lipgloss.Width(titleLeft) - lipgloss.Width(titleRight) - 2
	if titlePadding < 1 {
		titlePadding = 1
	}
	headerText := titleLeft + strings.Repeat(" ", titlePadding) + titleRight
	header := m.styles.PanelHeader.Width(w).Render(headerText)
	// 内容区可用宽度。
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}

	// 获取当前会话的看板快照。
	s := m.selectedSession()
	var snap board.Snapshot
	if s != nil {
		snap = m.boardSnapshot(s)
	}

	// 没有看板时（如 direct_tool 会话、Agent 树为空），用当前轮任务生成一个最小计划视图，
	// 避免右侧面板出现空白的 "(no plan)"。有子 Agent 派发时，boardSnapshot 已从权威
	// Agent 树构建任务列表（状态实时取自树节点），无需在此补充或二次覆盖。
	if len(snap.Tasks) == 0 {
		goal := ""
		status := board.TaskDone
		if s != nil {
			// 当前轮任务：取最后一条用户消息（多轮会话中反映最新任务），回退到会话 Goal。
			for i := len(s.Messages) - 1; i >= 0; i-- {
				if s.Messages[i].Role == enums.ChatRoleUser {
					goal = strings.TrimSpace(s.Messages[i].Content)
					break
				}
			}
			if goal == "" {
				goal = s.Goal
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

	// 渲染计划内容行。内容区高度受 PanelBox 限制（Height(h-3)），
	// 超长任务列表在 formatPlanSnapshot 内按 m.planScroll 滚动开窗（滚轮翻看，不再省略截断），
	// 底部"总体进度/计时行"固定可见。
	maxBody := h - 3
	if maxBody < 1 {
		maxBody = 1
	}
	lines := m.formatPlanSnapshot(innerW, snap, maxBody, m.planScroll)
	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// formatPlanSnapshot 把看板快照渲染成计划面板内的文本行。
// 布局参考"新TUI页.png"：
//   - 上方为编号任务列表：序号 + 标题 + 彩色状态单元格 + 右对齐 hh:mm:ss 时长；
//   - 底部固定为"总体进度"进度条与计时行（当前任务已执行时间 + TUI 总计时），内容不足时贴底显示；
//   - 任务数超出 maxLines 时按 scroll 偏移滚动开窗（滚轮悬停计划面板滚动，见 handleMouse），
//     窗口首/末行在还有未显示任务时替换为"↑ 上方还有 N 项"/"↓ 下方还有 N 项"提示。
func (m Model) formatPlanSnapshot(innerW int, snap board.Snapshot, maxLines, scroll int) []string {
	// 统计已完成数（用于总体进度条）。
	done, total := 0, len(snap.Tasks)
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
	}

	// 逐条渲染任务行。
	var taskLines []string
	for i, t := range snap.Tasks {
		taskLines = append(taskLines, m.planTaskLine(i, t, total, innerW))
	}

	// 底部统计区：空行 + 总体进度 + 计时行。
	footer := m.planFooterLines(innerW, snap, done, total)

	// 高度预算：footer 固定保留，任务列表按剩余空间滚动开窗。
	budget := maxLines - len(footer)
	if budget < 0 {
		budget = 0
	}
	if len(taskLines) > budget {
		if budget == 0 {
			taskLines = nil
		} else {
			// 滚动窗口：scroll 偏移钳制在合法范围内，超出列表长度时回退到末尾窗口。
			maxOff := len(taskLines) - budget
			scroll = clamp(scroll, 0, maxOff)
			visible := append([]string{}, taskLines[scroll:scroll+budget]...)
			if scroll > 0 {
				visible[0] = m.styles.Dim.Render(fmt.Sprintf("↑ 上方还有 %d 项（滚轮翻看）", scroll))
			}
			if below := len(taskLines) - scroll - budget; below > 0 {
				visible[len(visible)-1] = m.styles.Dim.Render(fmt.Sprintf("↓ 下方还有 %d 项（滚轮翻看）", below))
			}
			taskLines = visible
		}
	}

	lines := append([]string{}, taskLines...)
	// 内容不足一屏时插入空行，让底部统计贴底（对齐设计稿）。
	if pad := maxLines - len(lines) - len(footer); pad > 0 {
		for i := 0; i < pad; i++ {
			lines = append(lines, "")
		}
	}
	lines = append(lines, footer...)
	return lines
}

// planTaskLine 渲染单条计划任务：序号 + 标题 + 彩色状态单元格 + 右对齐时长。
func (m Model) planTaskLine(i int, t board.SubTask, total, innerW int) string {
	// 序号列宽按任务总数对齐（如 10 条以上占 2 列）。
	numW := len(fmt.Sprintf("%d", total))
	if numW < 1 {
		numW = 1
	}
	num := fmt.Sprintf("%-*d", numW, i+1)

	// 状态单元格：彩色图标 + 英文状态文本，固定 9 列（"● Running"）。
	icon := statusIcon(string(t.Status))
	color := statusColor(string(t.Status))
	statusText := strings.TrimSpace(planStatusText(t.Status))
	statusCell := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(fmt.Sprintf("%s %-7s", icon, statusText))

	// 时长列：待处理任务显示占位符，其余显示 hh:mm:ss。
	elapsed := "--:--:--"
	if t.Status != board.TaskPending {
		elapsed = taskElapsedHMS(t)
	}

	// 标题可用宽度 = 总宽 - 序号 - 状态 - 时长 - 3 个间隔空格。
	titleAvail := innerW - numW - 9 - 8 - 3
	if titleAvail < 6 {
		titleAvail = 6
	}
	// P3-3：过长标题先经 LLM 语义精简，再按显示宽度截断。
	title := truncate(m.summarizeTaskTitle(t.Title), titleAvail)
	pad := titleAvail - runewidth.StringWidth(title)
	if pad < 0 {
		pad = 0
	}
	titleStyled := title
	if t.Status == board.TaskDone {
		titleStyled = m.styles.Dim.Render(title)
	}
	return m.styles.Dim.Render(num) + " " + titleStyled + strings.Repeat(" ", pad) + " " + statusCell + " " + m.styles.Dim.Render(elapsed)
}

// planFooterLines 渲染计划面板底部的总体进度条与计时行：
// 计时行展示当前任务已执行时间（首个进行中/阻塞的任务）与 TUI 开启至今的总计时。
func (m Model) planFooterLines(innerW int, snap board.Snapshot, done, total int) []string {
	// 完成百分比。
	pct := 0
	if total > 0 {
		pct = done * 100 / total
	}
	// 进度条宽度 = 总宽 - 标签(8) - 百分比(4) - 2 个间隔空格。
	barW := innerW - 8 - 4 - 2
	if barW < 4 {
		barW = 4
	}
	filled := 0
	if total > 0 {
		filled = barW * done / total
	}
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)).Render(strings.Repeat("█", filled)) +
		m.styles.Dim.Render(strings.Repeat("░", barW-filled))
	progress := m.styles.Dim.Render("总体进度") + " " + bar + " " + m.styles.StatValue.Render(fmt.Sprintf("%d%%", pct))

	// 当前任务已执行时间：取首个进行中/阻塞的任务；无进行中的任务时显示占位符。
	cur := "--:--:--"
	for _, t := range snap.Tasks {
		if t.Status == board.TaskInProgress || t.Status == board.TaskBlocked {
			cur = taskElapsedHMS(t)
			break
		}
	}
	// 总计时：TUI 开启至今的累计时长；未记录启动时间（测试以字面量构造 Model）时显示占位符。
	uptime := "--:--:--"
	if !m.startedAt.IsZero() {
		uptime = formatDurationHMS(time.Since(m.startedAt))
	}
	timing := m.styles.Dim.Render("当前任务: ") + m.styles.StatValue.Render(cur) +
		m.styles.Dim.Render("  总计时: ") + m.styles.StatValue.Render(uptime)

	return []string{"", progress, timing}
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
		case enums.RoleStatusUnverified:
			st = board.TaskUnverified
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
// 优先级：Failed > Unverified > InProgress/Blocked > Done > Pending。
func strongerTaskStatus(a, b board.TaskStatus) board.TaskStatus {
	order := map[board.TaskStatus]int{
		board.TaskFailed:     4,
		board.TaskUnverified: 3,
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

// Agent 编排面板采用卡片式布局（见 agent_tree_panel.go renderAgentsPanel）：
// 状态色点 + 按角色着色的名称 + 状态文本 + 时间戳 + 任务描述。

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
	case string(enums.RoleStatusUnverified), string(board.TaskUnverified):
		return cStatusWait // 已交付未验证：黄（非红）
	default:
		return cStatusIdle
	}
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

// clipChat 给无边框对话区应用固定宽高样式，防止其超出分配空间并挤下输入栏。
func (m Model) clipChat(content string, w, h int) string {
	return lipgloss.NewStyle().Width(w).Height(h).Render(content)
}

// renderPlanBar 渲染旧版计划进度栏。
func (m Model) renderPlanBar(w int) string {
	var snap board.Snapshot
	var toolLabel string
	if s := m.selectedSession(); s != nil {
		toolLabel = lastToolLabel(s)
		snap = m.boardSnapshot(s)
	}
	line := formatPlanBar(m.styles, snap, toolLabel)
	return lipgloss.NewStyle().Width(w).Height(1).Render(line)
}

// renderTokenBar 渲染输入栏上方的 Token 用量栏，实时展示当前会话累计输入/输出 token
// 与缓存命中率（TODO #40 可观测；provider 未返回缓存字段时省略命中率）。
// ≥1000 时以 1.2k 形式显示，避免长数字挤占宽度。
func (m Model) renderTokenBar(w int) string {
	in, out := m.totalInputTokens, m.totalOutputTokens
	label := m.styles.Dim.Render("Token")
	inStr := fmtTokensK(in)
	outStr := fmtTokensK(out)
	inPart := m.styles.StatValue.Render("↑" + inStr)
	outPart := m.styles.StatValue.Render("↓" + outStr)
	left := lipgloss.JoinHorizontal(lipgloss.Left, label, " ", inPart, " ", outPart)
	// 缓存命中率：hit/(hit+miss)，无数据（provider 未解析或 mock）时不显示。
	if hit, miss := m.totalCacheHit, m.totalCacheMiss; hit+miss > 0 {
		rate := float64(hit) / float64(hit+miss) * 100
		cachePart := m.styles.StatValue.Render(fmt.Sprintf("缓存命中率 %.0f%%", rate))
		left = lipgloss.JoinHorizontal(lipgloss.Left, left, " ", cachePart)
	}
	// 右侧提示：会话运行中显示 cost warn 门槛说明，空会话显示占位。
	right := m.styles.Dim.Render("(本会话累计 · 每 1s 刷新)")
	line := lipgloss.JoinHorizontal(lipgloss.Top, left, lipgloss.NewStyle().Width(w-lipgloss.Width(left)-lipgloss.Width(right)).Render(""), right)
	if lipgloss.Width(line) > w {
		line = left
	}
	// 硬裁剪保证恒为 1 物理行（见 renderTopBar 注释）。
	line = hardClipLine(line, w)
	return lipgloss.NewStyle().Width(w).Height(1).Render(line)
}

// fmtTokensK 将 token 数格式化为短字符串：≥1000 显示为 1.2k，否则原样。
func fmtTokensK(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%.1fk", float64(n)/1000)
	// 去掉 .0 后缀，避免 1000 -> 1.0k -> 1k。
	if strings.HasSuffix(s, ".0k") {
		s = strings.TrimSuffix(s, ".0k") + "k"
	}
	return s
}

// renderShortcutBar 渲染底部快捷键栏，展示常用按键提示。
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
		{"Tab", "对话区"},
		{"Pg↑/Pg↓", "滚动"},
		{"?", "Help"},
		{"Ctrl+C", "Exit"},
	}
	var parts []string
	for _, s := range shortcuts {
		parts = append(parts, "["+m.styles.ShortcutKey.Render(s.key)+"] "+m.styles.ShortcutLabel.Render(s.label))
	}
	// 快捷键较多时一行放不下，硬裁剪保证恒为 1 物理行（折行会把输入栏顶出屏幕）。
	// 注意 ShortcutBar 样式带 Padding(0,1)，内容区只有 w-2 列。
	return m.styles.ShortcutBar.Width(w).Height(1).Render(hardClipLine(strings.Join(parts, "  "), w-2))
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
			// 进入 ANSI 转义序列。
			inAnsi = true
			b.WriteRune(r)
			continue
		}
		if inAnsi {
			b.WriteRune(r)
			// 字母表示 ANSI 序列结束。
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		if r == '\n' {
			// 保留原有换行。
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

// displayDetailLines 返回详情行的实际渲染内容。
// 工具结果详情（[✓]/[✗] 前缀的条目）在主对话区最多展示 8 行，超出折叠并追加提示，
// 完整内容可在弹窗 / Ctrl+L 完整记录中查看。
func displayDetailLines(title, detail string) []string {
	lines := strings.Split(detail, "\n")
	const maxLines = 8
	if (strings.HasPrefix(title, "[✓] ") || strings.HasPrefix(title, "[✗] ")) && len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, "    ...")
	}
	return lines
}

// hardClipLine ANSI 感知的硬裁剪：按显示宽度截断到 w 列（不加省略号），
// 保留行内 ANSI 序列；若行内含样式则末尾补 reset，防样式泄漏到后续行。
// 用于顶栏/快捷键栏等"必须恰好 1 物理行"的场景：物理折行会让整帧比终端高。
func hardClipLine(s string, w int) string {
	if w < 1 || lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	cur := 0
	inAnsi := false
	sawAnsi := false
	for _, r := range s {
		if r == '\x1b' {
			inAnsi = true
			sawAnsi = true
			b.WriteRune(r)
			continue
		}
		if inAnsi {
			b.WriteRune(r)
			// 字母表示 ANSI 序列结束。
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inAnsi = false
			}
			continue
		}
		rw := runewidth.RuneWidth(r)
		if cur+rw > w {
			break
		}
		b.WriteRune(r)
		cur += rw
	}
	if sawAnsi {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// truncate 按显示宽度截断字符串并在末尾追加省略号（占 1 列）。
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
