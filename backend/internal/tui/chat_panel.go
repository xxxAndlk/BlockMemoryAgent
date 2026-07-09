package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// ChatPanel owns the chat viewport, cursor, scroll state, item offsets,
// follow-bottom logic and pending-scroll anchors for the TUI.
type ChatPanel struct {
	vp                  viewport.Model
	cursor              int
	followBottom        bool
	lastItems           int
	lastWidth           int
	itemOffsets         []int
	pendingScrollToUser bool
	anchorUser          bool
	pendingFirstMessage string
	scrollbarDragging   bool
	dragStartY          int
	dragStartOffset     int
}

// NewChatPanel constructs a ChatPanel with sensible defaults.
func NewChatPanel() ChatPanel {
	cp := ChatPanel{
		vp:           viewport.New(0, 0),
		followBottom: true,
	}
	cp.vp.SetContent("")
	return cp
}

// currentItem returns the chatItem index at the top of the viewport.
func (cp *ChatPanel) currentItem() int {
	if len(cp.itemOffsets) == 0 {
		return 0
	}
	offset := cp.vp.YOffset
	idx := 0
	for i := len(cp.itemOffsets) - 1; i >= 0; i-- {
		if cp.itemOffsets[i] <= offset {
			idx = i
			break
		}
	}
	return idx
}

func (cp *ChatPanel) scrollToItem(idx int) {
	if len(cp.itemOffsets) == 0 {
		return
	}
	idx = clamp(idx, 0, len(cp.itemOffsets)-1)
	cp.vp.SetYOffset(cp.itemOffsets[idx])
	cp.followBottom = idx == len(cp.itemOffsets)-1
}

// scrollToItemBottom scrolls so the item at idx is fully visible. If the item
// fits in the viewport its top is aligned to the viewport top; otherwise the
// item bottom is aligned to the viewport bottom.
func (cp *ChatPanel) scrollToItemBottom(idx, totalLines, visibleLines int) {
	if len(cp.itemOffsets) == 0 {
		return
	}
	idx = clamp(idx, 0, len(cp.itemOffsets)-1)
	startOffset := cp.itemOffsets[idx]
	var endOffset int
	if idx+1 < len(cp.itemOffsets) {
		endOffset = cp.itemOffsets[idx+1]
	} else {
		endOffset = totalLines
	}
	itemH := endOffset - startOffset
	var target int
	if itemH <= visibleLines {
		target = startOffset
	} else {
		target = endOffset - visibleLines
	}
	if target < 0 {
		target = 0
	}
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

func (cp *ChatPanel) itemUp() {
	cp.scrollToItem(cp.currentItem() - 1)
	cp.anchorUser = false
}

func (cp *ChatPanel) itemDown() {
	cp.scrollToItem(cp.currentItem() + 1)
	cp.anchorUser = false
}

func (cp *ChatPanel) gotoTop() {
	cp.scrollToItem(0)
	cp.anchorUser = false
}

func (cp *ChatPanel) gotoBottom() {
	n := len(cp.itemOffsets)
	if n == 0 {
		return
	}
	cp.scrollToItem(n - 1)
	cp.vp.GotoBottom()
	cp.anchorUser = false
}

// scrollbarArea returns the chat scrollbar screen bounds (x, y, w, h).
func (cp *ChatPanel) scrollbarArea(chatAreaWidth, contentH int) (x, y, w, h int) {
	x = chatAreaWidth - 2 // gap(1) + scrollbar width(1)
	if x < 0 {
		x = 0
	}
	y = 1 // top bar height
	w = 1
	h = contentH
	return
}

// thumbBounds returns the scrollbar thumb start/end row indices (inclusive).
// Returns (-1, -1) when no scrolling is needed.
func (cp *ChatPanel) thumbBounds(contentH int) (start, end int) {
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	h := contentH
	if totalLines <= viewportH || viewportH <= 0 || h < 1 {
		return -1, -1
	}
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
	thumbPos := cp.vp.YOffset * (h - thumbH) / scrollable
	if thumbPos < 0 {
		thumbPos = 0
	}
	if thumbPos+thumbH > h {
		thumbPos = h - thumbH
	}
	return thumbPos, thumbPos + thumbH - 1
}

// updateDrag updates the viewport offset based on the current mouse Y while
// dragging the scrollbar thumb.
func (cp *ChatPanel) updateDrag(mouseY, contentH int) {
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
	maxThumbTravel := h - thumbH
	if maxThumbTravel < 1 {
		maxThumbTravel = 1
	}
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

// scrollToThumbY aligns the scrollbar thumb center to the given Y coordinate
// (relative to the scrollbar top).
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

func (cp *ChatPanel) renderScrollbar(w, h, viewportH, totalLines, startLine int, styles *Styles) string {
	if h < 1 {
		return ""
	}
	if w < 1 {
		w = 1
	}
	trackStyle := styles.ScrollbarTrack
	thumbStyle := styles.ScrollbarThumb

	rows := make([]string, h)
	for i := range rows {
		rows[i] = trackStyle.Render("│")
	}

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

func (cp *ChatPanel) renderChat(w, h int, styles *Styles, session *server.Session, items []chatItem, modelName string) string {
	const scrollbarW = 1
	gap := 1
	contentW := w - scrollbarW - gap
	if contentW < 4 {
		contentW = w
	}

	if len(items) == 0 {
		if session == nil {
			return renderWelcome(styles, contentW, h, modelName)
		}
		return renderEmptyChat(styles, contentW, h)
	}

	cp.vp.Width = contentW
	cp.vp.Height = h

	content := lipgloss.NewStyle().Width(contentW).Height(h).Render(cp.vp.View())
	totalLines := cp.vp.TotalLineCount()
	viewportH := cp.vp.VisibleLineCount()
	startLine := cp.vp.YOffset
	bar := cp.renderScrollbar(scrollbarW, h, viewportH, totalLines, startLine, styles)
	return lipgloss.JoinHorizontal(lipgloss.Top, content, bar)
}

// collectItems gathers the chat items to display, including the local
// pending-first-message fallback when the server has not yet synced it.
func (cp *ChatPanel) collectItems(s *server.Session) []chatItem {
	var items []chatItem
	if s != nil {
		items = chatItems(s, true)
	}
	if cp.pendingFirstMessage != "" {
		already := false
		for _, it := range items {
			if strings.TrimPrefix(it.title, "> ") == cp.pendingFirstMessage {
				already = true
				break
			}
		}
		if !already {
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
	return items
}

func (cp *ChatPanel) rebuildContent(items []chatItem, styles *Styles, width int) {
	if len(items) == 0 {
		cp.vp.SetContent("")
		cp.lastItems = 0
		return
	}
	w := width
	if w < 4 {
		w = 4
	}
	content := cp.buildContent(items, styles, w)
	cp.vp.SetContent(content)
	cp.lastItems = len(items)
	cp.lastWidth = w
}

func (cp *ChatPanel) buildContent(items []chatItem, styles *Styles, width int) string {
	if len(items) == 0 {
		cp.itemOffsets = nil
		return ""
	}
	offsets := make([]int, len(items))
	var lines []string

	for i, item := range items {
		offsets[i] = len(lines)
		ts := item.timestamp.Format("15:04:05")
		tsStyled := styles.Dim.Render(ts)

		var titleLines []string
		switch {
		case strings.HasPrefix(item.title, "> "):
			content := strings.TrimPrefix(item.title, "> ")
			label := styles.LogUser.Render("You")
			prefix := tsStyled + " " + label + " "
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
		case strings.HasPrefix(item.title, "[●] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogWarn, width)...)
		case strings.HasPrefix(item.title, "[✓] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogSuccess, width)...)
		case strings.HasPrefix(item.title, "[✗] "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogError, width)...)
		case strings.HasPrefix(item.title, "🧠 recalled: "):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.Dim, width)...)
		case strings.HasPrefix(item.title, "─── ") && strings.HasSuffix(item.title, " ───"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.CallStack, width)...)
		case strings.HasPrefix(item.title, "✗ Error"):
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogError, width)...)
		case item.isEvent:
			titleLines = append(titleLines, wrapStyledLine(tsStyled, item.title, styles.LogInfo, width)...)
		default:
			label := styles.LogAssistant.Render("Assistant")
			availW := width - lipgloss.Width(label) - lipgloss.Width(ts) - 3
			if availW < 4 {
				availW = 4
			}
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

// renderWelcome renders the welcome screen shown when there is no session.
func renderWelcome(styles *Styles, w, h int, modelName string) string {
	title := styles.WelcomeTitle.Render("BlockMemoryAgent")
	subtitle := styles.WelcomeSub.Render("AI Agent for Code, Memory and More.")
	info := renderWelcomeInfo(styles, w, modelName)
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", subtitle, "", info)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

// renderEmptyChat renders the placeholder shown for a session with no messages.
func renderEmptyChat(styles *Styles, w, h int) string {
	title := styles.WelcomeTitle.Render("BlockMemoryAgent")
	hint := styles.WelcomeSub.Render("会话已启动，在底部输入栏发送第一条消息。")
	shortcuts := styles.Dim.Render("Enter 发送 · / 命令 · ? 帮助 · Ctrl+C 退出")
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", hint, "", shortcuts)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, content)
}

func renderWelcomeInfo(styles *Styles, w int, modelName string) string {
	pairs := []struct {
		icon  string
		label string
		value string
	}{
		{"🖥", "Model", modelName},
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

// Model wrappers so the rest of the package can keep a stable call site.

func (m *Model) collectChatItems() []chatItem {
	return m.chatPanel.collectItems(m.selectedSession())
}

func (m *Model) rebuildChatContent() {
	items := m.collectChatItems()
	m.chatPanel.rebuildContent(items, m.styles, m.chatContentWidth())
}

func (m Model) renderChat(w, h int) string {
	s := m.selectedSession()
	return m.chatPanel.renderChat(w, h, m.styles, s, m.chatPanel.collectItems(s), m.modelName)
}
