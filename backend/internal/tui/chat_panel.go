package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// ChatPanel 维护对话视口、光标、滚动状态、item 偏移、自动跟随底部逻辑以及发送后锚定用户消息的状态。
type ChatPanel struct {
	// vp 是 bubbletea viewport 组件，负责对话内容的滚动展示。
	vp viewport.Model
	// cursor 是当前 viewport 顶部对应的 chatItem 索引。
	cursor int
	// followBottom 表示内容增长时是否自动滚动到底部。
	followBottom bool
	// lastItems 记录上一次重建内容时的 item 数量，用于判断是否需要重建。
	lastItems int
	// lastWidth 记录上一次重建内容时的宽度，用于判断是否需要因宽度变化重建。
	lastWidth int
	// itemOffsets 记录每个 chatItem 在渲染后文本中的起始行偏移。
	itemOffsets []int
	// pendingScrollToUser 表示发送后需要滚动到用户消息。
	pendingScrollToUser bool
	// anchorUser 表示已将用户消息锚定在视口内，避免后续输出将其顶出。
	anchorUser bool
	// pendingFirstMessage 是在会话创建前本地预展示的首条用户消息。
	pendingFirstMessage string
	// lastLiveTitle 是上次重建时最后一条 chatItem 的标题，
	// 用于检测流式文本增长/等待状态切换等"条目数不变但内容变化"的情况。
	lastLiveTitle string
	// scrollbarDragging 表示当前是否正在拖动滚动条滑块。
	scrollbarDragging bool
	// dragStartY 是开始拖动时鼠标的 Y 坐标。
	dragStartY int
	// dragStartOffset 是开始拖动时 viewport 的 Y 偏移。
	dragStartOffset int
}

// NewChatPanel 构造一个 ChatPanel，默认启用自动跟随底部并清空内容。
func NewChatPanel() ChatPanel {
	cp := ChatPanel{
		vp:           viewport.New(0, 0),
		followBottom: true,
	}
	cp.vp.SetContent("")
	return cp
}

// currentItem 返回当前 viewport 顶部对应的 chatItem 索引。
func (cp *ChatPanel) currentItem() int {
	if len(cp.itemOffsets) == 0 {
		return 0
	}
	offset := cp.vp.YOffset
	idx := 0
	// 从后向前找到第一个偏移量不超过当前 offset 的 item。
	for i := len(cp.itemOffsets) - 1; i >= 0; i-- {
		if cp.itemOffsets[i] <= offset {
			idx = i
			break
		}
	}
	return idx
}

// scrollToItem 滚动到指定 item 的顶部，并更新 followBottom 状态。
func (cp *ChatPanel) scrollToItem(idx int) {
	if len(cp.itemOffsets) == 0 {
		return
	}
	// 限制索引范围。
	idx = clamp(idx, 0, len(cp.itemOffsets)-1)
	cp.vp.SetYOffset(cp.itemOffsets[idx])
	// 若滚动到最后一个 item，则恢复跟随底部。
	cp.followBottom = idx == len(cp.itemOffsets)-1
}

// scrollToItemBottom 滚动到指定 item 完全可见。若 item 高度不超过视口高度，则顶部对齐；
// 否则底部对齐，以便查看最新部分，同时尽量保留上方历史。
func (cp *ChatPanel) scrollToItemBottom(idx, totalLines, visibleLines int) {
	if len(cp.itemOffsets) == 0 {
		return
	}
	idx = clamp(idx, 0, len(cp.itemOffsets)-1)
	startOffset := cp.itemOffsets[idx]
	var endOffset int
	// 计算该 item 的结束偏移。
	if idx+1 < len(cp.itemOffsets) {
		endOffset = cp.itemOffsets[idx+1]
	} else {
		endOffset = totalLines
	}
	itemH := endOffset - startOffset
	var target int
	if itemH <= visibleLines {
		// item 能完整放入视口：顶部对齐。
		target = startOffset
	} else {
		// item 超出视口：底部对齐。
		target = endOffset - visibleLines
	}
	if target < 0 {
		target = 0
	}
	// 限制最大偏移，防止越界。
	maxOffset := totalLines - visibleLines
	if maxOffset < 0 {
		maxOffset = 0
	}
	if target > maxOffset {
		target = maxOffset
	}
	cp.vp.SetYOffset(target)
	cp.followBottom = target >= maxOffset
}

// itemUp 滚动到上一个 chatItem，并解除用户锚定。
func (cp *ChatPanel) itemUp() {
	cp.scrollToItem(cp.currentItem() - 1)
	cp.anchorUser = false
}

// itemDown 滚动到下一个 chatItem，并解除用户锚定。
func (cp *ChatPanel) itemDown() {
	cp.scrollToItem(cp.currentItem() + 1)
	cp.anchorUser = false
}

// gotoTop 滚动到第一个 chatItem，并解除用户锚定。
func (cp *ChatPanel) gotoTop() {
	cp.scrollToItem(0)
	cp.anchorUser = false
}

// gotoBottom 滚动到最后一个 chatItem，并确保 viewport 位于底部。
func (cp *ChatPanel) gotoBottom() {
	n := len(cp.itemOffsets)
	if n == 0 {
		return
	}
	cp.scrollToItem(n - 1)
	cp.vp.GotoBottom()
	cp.anchorUser = false
}

// scrollbarArea 返回聊天区滚动条在屏幕上的范围（x, y, w, h）。
// 顶栏占 1 行，滚动条位于对话区最右侧，宽度 1。
func (cp *ChatPanel) scrollbarArea(chatAreaWidth, contentH int) (x, y, w, h int) {
	x = chatAreaWidth - 2 // 间隔 1 + 滚动条宽度 1
	if x < 0 {
		x = 0
	}
	y = 1 // 顶栏高度
	w = 1
	h = contentH
	return
}

// thumbBounds 返回滚动条滑块在滚动条区域内的起始/结束行索引（含）。
// 若内容无需滚动则返回 (-1, -1)。
func (cp *ChatPanel) thumbBounds(contentH int) (start, end int) {
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	h := contentH
	// 内容不足或区域无效时无需滚动条。
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return -1, -1
	}
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
	// 滑块位置按当前偏移占可滚动范围的比例计算。
	thumbPos := cp.vp.YOffset * (h - thumbH) / scrollable
	if thumbPos < 0 {
		thumbPos = 0
	}
	if thumbPos+thumbH > h {
		thumbPos = h - thumbH
	}
	return thumbPos, thumbPos + thumbH - 1
}

// updateDrag 在拖动滚动条滑块时，根据当前鼠标 Y 坐标更新 viewport 偏移。
// 以 dragStartY/dragStartOffset 为基准，按滑块可移动范围与内容可滚动范围的比率映射。
func (cp *ChatPanel) updateDrag(mouseY, contentH int) {
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	h := contentH
	// 无需滚动时不更新。
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return
	}
	scrollable := totalLines - viewportH
	// 滑块高度与位置计算与 thumbBounds 一致。
	thumbH := h * viewportH / totalLines
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > h {
		thumbH = h
	}
	maxThumbTravel := h - thumbH
	if maxThumbTravel < 1 {
		maxThumbTravel = 1
	}
	// 将鼠标 Y 位移映射为 viewport offset 位移。
	deltaY := mouseY - cp.dragStartY
	deltaOffset := deltaY * scrollable / maxThumbTravel
	newOffset := cp.dragStartOffset + deltaOffset
	if newOffset < 0 {
		newOffset = 0
	}
	if newOffset > scrollable {
		newOffset = scrollable
	}
	cp.vp.YOffset = newOffset
	cp.followBottom = cp.vp.AtBottom()
	cp.anchorUser = false
}

// scrollToThumbY 将滑块中心对齐到滚动条区域内的指定 Y 坐标（相对于滚动条顶部）。
func (cp *ChatPanel) scrollToThumbY(thumbCenterY, contentH int) {
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	h := contentH
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return
	}
	scrollable := totalLines - viewportH
	thumbH := h * viewportH / totalLines
	if thumbH < 1 {
		thumbH = 1
	}
	if thumbH > h {
		thumbH = h
	}
	maxPos := h - thumbH
	if maxPos < 1 {
		maxPos = 1
	}
	// 限制目标位置在有效范围内。
	pos := thumbCenterY
	if pos < 0 {
		pos = 0
	}
	if pos > maxPos {
		pos = maxPos
	}
	cp.vp.YOffset = pos * scrollable / maxPos
	if cp.vp.YOffset > scrollable {
		cp.vp.YOffset = scrollable
	}
	cp.followBottom = cp.vp.AtBottom()
	cp.anchorUser = false
}

// renderScrollbar 渲染右侧垂直滚动条，参数 w/h 为滚动条区域宽高；
// viewportH/totalLines/startLine 决定滑块位置与高度。
func (cp *ChatPanel) renderScrollbar(w, h, viewportH, totalLines, startLine int, styles *Styles) string {
	if h < 1 {
		return ""
	}
	if w < 1 {
		w = 1
	}
	trackStyle := styles.ScrollbarTrack
	thumbStyle := styles.ScrollbarThumb

	// 先填满轨道字符。
	rows := make([]string, h)
	for i := range rows {
		rows[i] = trackStyle.Render("│")
	}

	// 内容超出视口时绘制滑块。
	if totalLines > viewportH && viewportH > 0 {
		scrollable := totalLines - viewportH
		if scrollable < 1 {
			scrollable = 1
		}
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

// renderChat 渲染对话区，包括 viewport 内容与滚动条。
// hasItems 表示最近一次内容重建（rebuildContent）时是否有对话条目，仅用于选择
// 欢迎页/空会话占位；内容本体始终渲染 viewport 缓存，避免每帧重复全量 collectItems 计算。
func (cp *ChatPanel) renderChat(w, h int, styles *Styles, session *server.Session, hasItems bool, modelName, workDir string) string {
	const scrollbarW = 1
	gap := 1
	// 内容区宽度扣除滚动条与间隔。
	contentW := w - scrollbarW - gap
	if contentW < 4 {
		contentW = w
	}

	// 无 item 时展示欢迎页或空会话提示。
	if !hasItems {
		if session == nil {
			return renderWelcome(styles, contentW, h, modelName, workDir)
		}
		return renderEmptyChat(styles, contentW, h)
	}

	// 设置 viewport 尺寸并渲染内容。
	cp.vp.Width = contentW
	cp.vp.Height = h

	content := lipgloss.NewStyle().Width(contentW).Height(h).Render(cp.vp.View())
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	startLine := cp.vp.YOffset
	bar := cp.renderScrollbar(scrollbarW, h, viewportH, totalLines, startLine, styles)
	return lipgloss.JoinHorizontal(lipgloss.Top, content, bar)
}

// maxChatItems 是主对话区最多展示的条目数。长会话会产生数百条工具事件，
// 全部加载会让用户滚动时陷入历史记录、找不到最新内容；更早的记录不进对话区，
// 需要翻阅时通过 Ctrl+L 完整记录面板查看全部输出。
const maxChatItems = 50

// collectItems 收集当前应展示的全部 chatItem，包含本地预展示的首条用户消息兜底。
// 仅保留最近 maxChatItems 条，更早的条目以一条省略提示代替。
func (cp *ChatPanel) collectItems(s *server.Session) []chatItem {
	var items []chatItem
	if s != nil {
		items = chatItems(s, true)
	}
	// 若存在本地预展示消息，且服务端 items 中尚未包含该内容，则追加一条本地 item。
	if cp.pendingFirstMessage != "" {
		already := false
		for _, it := range items {
			// 两侧都去空白比较，避免尾随换行/空格差异导致同一条问题重复显示。
			if strings.TrimSpace(strings.TrimPrefix(it.title, "> ")) == strings.TrimSpace(cp.pendingFirstMessage) {
				already = true
				break
			}
		}
		if !already {
			// 时间戳优先使用会话启动时间，无会话则使用当前时间。
			ts := time.Now()
			if s != nil {
				ts = s.StartedAt
			}
			items = append(items, chatItem{
				title:     "> " + cp.pendingFirstMessage,
				timestamp: ts,
				isEvent:   false,
				role:      enums.ChatRoleUser,
			})
		}
	}
	// 裁剪到最近 maxChatItems 条：更早的记录不加载进对话区（完整内容见 Ctrl+L），
	// 顶部放一条省略提示，滚动到顶即止步于此，不会再深入历史。
	if len(items) > maxChatItems {
		omitted := len(items) - maxChatItems
		kept := make([]chatItem, 0, maxChatItems+1)
		kept = append(kept, chatItem{
			title:     fmt.Sprintf("─── 已省略 %d 条较早记录 · 按 Ctrl+L 查看完整记录 ───", omitted),
			timestamp: items[omitted].timestamp,
			isEvent:   true,
		})
		items = append(kept, items[omitted:]...)
	}
	return items
}

// rebuildContent 根据 item 列表重建 viewport 内容，并记录数量与宽度。
func (cp *ChatPanel) rebuildContent(items []chatItem, styles *Styles, width int) {
	if len(items) == 0 {
		cp.vp.SetContent("")
		cp.lastItems = 0
		cp.lastLiveTitle = ""
		return
	}
	w := width
	if w < 4 {
		w = 4
	}
	content := cp.buildContent(items, styles, w)
	cp.vp.SetContent(content)
	cp.lastItems = len(items)
	cp.lastLiveTitle = items[len(items)-1].title
	cp.lastWidth = w
}

// buildContent 将 chatItem 列表渲染为带样式的文本行，并记录每个 item 的行偏移。
func (cp *ChatPanel) buildContent(items []chatItem, styles *Styles, width int) string {
	if len(items) == 0 {
		cp.itemOffsets = nil
		return ""
	}
	offsets := make([]int, len(items))
	var lines []string

	for i, item := range items {
		// 记录当前 item 在总文本中的起始行号。
		offsets[i] = len(lines)
		// 时间戳样式。
		ts := item.timestamp.Format("15:04:05")
		tsStyled := styles.Dim.Render(ts)

		var titleLines []string
		switch {
		// 用户消息：前缀 "> "，使用 You 标签与域色。
		case strings.HasPrefix(item.title, "> "):
			content := strings.TrimPrefix(item.title, "> ")
			label := styles.LogUser.Render("You")
			prefix := tsStyled + " " + label + " "
			// 续行缩进需对齐到首行内容起始位置。
			indent := strings.Repeat(" ", lipgloss.Width(ts)+1) + strings.Repeat(" ", lipgloss.Width(label)+1)
			availW := width - lipgloss.Width(prefix)
			if availW < 4 {
				availW = 4
			}
			wrappedLines := wrapToWidth(content, availW)
			for j, wl := range wrappedLines {
				styledWl := styles.LogUser.Render(wl)
				if j == 0 {
					titleLines = append(titleLines, prefix+styledWl)
				} else {
					titleLines = append(titleLines, indent+styledWl)
				}
			}
		// 运行中工具事件：使用警告色。
		case strings.HasPrefix(item.title, "[●] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogWarn, width)...)
		// 成功工具事件：使用成功色。
		case strings.HasPrefix(item.title, "[✓] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogSuccess, width)...)
		// 失败工具事件：使用错误色。
		case strings.HasPrefix(item.title, "[✗] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogError, width)...)
		// 记忆召回事件：使用暗淡色。
		case strings.HasPrefix(item.title, "🧠 recalled: "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.Dim, width)...)
		// 思考过程（中间推理步骤）：使用暗淡色，模拟主流 Agent 的"临时思考"观感。
		case strings.HasPrefix(item.title, "💭 "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.Dim, width)...)
		// 话题切换分隔线。
		case strings.HasPrefix(item.title, "─── ") && strings.HasSuffix(item.title, " ───"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.CallStack, width)...)
		// 错误事件。
		case strings.HasPrefix(item.title, "✗ Error"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogError, width)...)
		// 其他事件：使用信息色。
		case item.isEvent:
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogInfo, width)...)
		// 默认作为助手消息处理。
		default:
			label := styles.LogAssistant.Render("Assistant")
			availW := width - lipgloss.Width(label) - lipgloss.Width(ts) - 3
			if availW < 4 {
				availW = 4
			}
			// 先对 Markdown 做轻量格式化，再按宽度换行。
			text := formatMarkdown(item.title)
			wrappedLines := wrapToWidth(text, availW)
			for j, wl := range wrappedLines {
				line := tsStyled + " " + label + " " + wl
				if j > 0 {
					line = strings.Repeat(" ", lipgloss.Width(ts)+1) + "   " + wl
				}
				titleLines = append(titleLines, line)
			}
		}
		lines = append(lines, titleLines...)

		// 追加详情行（如工具输出），并做缩进与换行。
		if item.detail != "" {
			for _, l := range displayDetailLines(item.title, item.detail) {
				wrappedLines := wrapToWidth(l, width-2)
				for _, wl := range wrappedLines {
					lines = append(lines, "  "+wl)
				}
			}
		}
	}

	cp.itemOffsets = offsets
	return strings.Join(lines, "\n")
}

// renderWelcome 渲染无会话时的欢迎页。
func renderWelcome(styles *Styles, w, h int, modelName, workDir string) string {
	title := styles.WelcomeTitle.Render("BlockMemoryAgent")
	subtitle := styles.WelcomeSub.Render("AI Agent for Code, Memory and More.")
	info := renderWelcomeInfo(styles, w, modelName, workDir)
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", subtitle, "", info)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

// renderEmptyChat 渲染会话已启动但无消息时的占位提示。
func renderEmptyChat(styles *Styles, w, h int) string {
	title := styles.WelcomeTitle.Render("BlockMemoryAgent")
	hint := styles.WelcomeSub.Render("会话已启动，在底部输入栏发送第一条消息。")
	shortcuts := styles.Dim.Render("Enter 发送 · / 命令 · ? 帮助 · Ctrl+C 退出")
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", hint, "", shortcuts)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

// renderWelcomeInfo 渲染欢迎页底部的信息框，分两列展示模型、工作区、记忆等信息。
func renderWelcomeInfo(styles *Styles, w int, modelName, workDir string) string {
	pairs := []struct {
		icon  string
		label string
		value string
	}{
		{"🖥", "Model", modelName},
		{"📁", "Workspace", workDir},
		{"🧠", "Memory", "Enabled"},
		{"#", "Session", "#12"},
		{"⚡", "Skills", "38"},
		{"🪟", "Context Window", "128K"},
		{"🔌", "MCP Servers", "12 Connected"},
		{"🐚", "Shell", "zsh"},
	}
	// 计算每列宽度，留出中间间隔。
	colW := (w - 6) / 2
	if colW < 20 {
		colW = 20
	}
	var left, right []string
	for i, p := range pairs {
		line := p.icon + " " + styles.WelcomeLabel.Render(p.label) + ": " + styles.WelcomeValue.Render(p.value)
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
	return styles.WelcomeBox.Width(w).Render(body)
}

// 以下是为 Model 提供的便捷封装。

// collectChatItems 返回当前选中会话应展示的 chatItem 列表。
func (m *Model) collectChatItems() []chatItem {
	return m.chatPanel.collectItems(m.selectedSession())
}

// rebuildChatContent 重建当前会话的对话内容。
func (m *Model) rebuildChatContent() {
	items := m.collectChatItems()
	m.chatPanel.rebuildContent(items, m.styles, m.chatContentWidth())
}

// renderChat 为 Model 渲染对话区。
// 是否有对话条目复用 rebuildContent 记录的 lastItems，不再每帧重算 collectItems：
// 条目状态由 tick/stream 驱动的 refreshView（或 selectSession/submitInput 的
// rebuildChatContent）负责更新，最多个别 0↔非0 切换延迟一个刷新周期（≤100ms）。
func (m Model) renderChat(w, h int) string {
	s := m.selectedSession()
	return m.chatPanel.renderChat(w, h, m.styles, s, m.chatPanel.lastItems > 0, m.modelName, m.workDir)
}
