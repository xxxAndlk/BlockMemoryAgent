package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// OverlayPanel 维护弹窗/浮层的状态，包括当前模式、标题、内容行、光标位置与类型。
type OverlayPanel struct {
	// mode 是当前弹窗模式，取值见 keys.go 中的 overlayNone/overlayHelp 等常量。
	mode int
	// title 是弹窗标题，如 Help/Execution Plan/Agent Topology 等。
	title string
	// lines 是弹窗内展示的内容行切片。
	lines []string
	// cursor 是当前选中的行索引，用于行级滚动与 Enter 查看详情。
	cursor int
	// kind 记录弹窗原始类型，用于渲染时区分帮助/计划/Agent/记录等。
	kind int
}

// NewOverlayPanel 构造一个空的弹窗状态对象。
func NewOverlayPanel() OverlayPanel {
	return OverlayPanel{}
}

// open 以详情模式打开弹窗，设置标题与内容行，并将光标重置到顶部。
func (op *OverlayPanel) open(title string, lines []string) {
	op.mode = overlayDetail
	op.kind = overlayDetail
	op.title = title
	op.lines = lines
	op.cursor = 0
}

// openHelp 打开帮助弹窗，内容来自 keys.go 中的 fullHelpText。
func (op *OverlayPanel) openHelp() {
	op.mode = overlayHelp
	op.kind = overlayHelp
	op.title = "Help"
	// 去掉首尾空行后按行拆分，方便行级滚动。
	op.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	op.cursor = 0
}

// close 关闭弹窗，将模式重置为 overlayNone。
func (op *OverlayPanel) close() {
	op.mode = overlayNone
}

// clampCursor 将光标限制在有效行范围内，空内容时设为 0。
func (op *OverlayPanel) clampCursor() {
	if len(op.lines) == 0 {
		op.cursor = 0
		return
	}
	if op.cursor < 0 {
		op.cursor = 0
	}
	if op.cursor >= len(op.lines) {
		op.cursor = len(op.lines) - 1
	}
}

// render 渲染弹窗内容，参数 w/h 为弹窗区域宽高；planLines/agentsLines/transcriptLines
// 分别提供计划/Agent/完整记录面板的实时内容。
func (op *OverlayPanel) render(styles *Styles, w, h int, planLines, agentsLines, transcriptLines []string) string {
	// 帮助模式：每次渲染都重新加载帮助文本，确保内容最新。
	if op.mode == overlayHelp {
		op.title = "Help"
		op.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
	// 根据弹窗类型切换内容源。
	if op.mode == overlayPlan {
		op.lines = planLines
	} else if op.mode == overlayAgents {
		op.lines = agentsLines
	} else if op.mode == overlayLog {
		op.lines = transcriptLines
	}

	// 计算可见行数，保留标题与提示行空间，至少展示 3 行。
	maxLines := h - 4
	if maxLines < 3 {
		maxLines = 3
	}
	// 再次限制光标范围，防止内容切换后越界。
	if op.cursor < 0 {
		op.cursor = 0
	}
	if op.cursor >= len(op.lines) {
		op.cursor = len(op.lines) - 1
	}
	// 以光标为中心展示窗口，实现平滑滚动。
	start := op.cursor - maxLines/2
	if start < 0 {
		start = 0
	}
	end := start + maxLines
	if end > len(op.lines) {
		end = len(op.lines)
		start = end - maxLines
		if start < 0 {
			start = 0
		}
	}
	visible := op.lines[start:end]
	content := strings.Join(visible, "\n")

	// 弹窗尺寸：宽度为终端的 4/5，最小 40；高度比给定区域少 2 行作为边距。
	boxW := w * 4 / 5
	if boxW < 40 {
		boxW = w - 4
	}
	boxH := h - 2

	// 拼接标题、操作提示与内容，居中放置。
	header := styles.Header.Render(op.title)
	hint := styles.Dim.Render("  [Esc close · j/k scroll · enter detail]")
	body := lipgloss.JoinVertical(lipgloss.Left, header+hint, content)
	box := styles.Overlay.Width(boxW).Height(boxH).Render(body)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// 以下是为 Model 提供的便捷封装，保持现有调用点不变。

// openHelpPopup 打开帮助弹窗。
func (m *Model) openHelpPopup() {
	m.overlayPanel.openHelp()
}

// togglePlanPopup 切换执行计划弹窗：当前未打开则打开，已打开则关闭。
func (m *Model) togglePlanPopup() {
	// 没有可展示的计划时给出提示。
	if !m.hasPlan() {
		m.flashMsg("no plan available")
		return
	}
	// 已处于计划弹窗模式则关闭。
	if m.overlayPanel.mode == overlayPlan {
		m.overlayPanel.close()
		return
	}
	// 构建计划行并设置弹窗状态。
	lines := m.buildPlanLines()
	m.overlayPanel.mode = overlayPlan
	m.overlayPanel.kind = overlayPlan
	m.overlayPanel.title = "Execution Plan"
	m.overlayPanel.lines = lines
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(lines)-1)
}

// toggleAgentsPopup 切换 Agent 编排弹窗。
func (m *Model) toggleAgentsPopup() {
	// 没有 Agent 时给出提示。
	if len(m.agentTreePanel.nodes) == 0 {
		m.flashMsg("no agents available")
		return
	}
	// 已处于 Agent 弹窗模式则关闭。
	if m.overlayPanel.mode == overlayAgents {
		m.overlayPanel.close()
		return
	}
	// 构建 Agent 行并设置弹窗状态。
	lines := m.buildAgentsLines()
	m.overlayPanel.mode = overlayAgents
	m.overlayPanel.kind = overlayAgents
	m.overlayPanel.title = "Agent Topology"
	m.overlayPanel.lines = lines
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(lines)-1)
}

// toggleLogPopup 切换完整记录弹窗，光标默认跳到末尾以便查看最新输出。
func (m *Model) toggleLogPopup() {
	s := m.selectedSession()
	// 没有激活会话时给出提示。
	if s == nil {
		m.flashMsg("no active session")
		return
	}
	// 已处于记录弹窗模式则关闭。
	if m.overlayPanel.mode == overlayLog {
		m.overlayPanel.close()
		return
	}
	// 构建完整记录行，光标默认置于末尾。
	lines := m.buildTranscriptLines()
	cursor := len(lines) - 1
	if cursor < 0 {
		cursor = 0
	}
	m.overlayPanel.mode = overlayLog
	m.overlayPanel.kind = overlayLog
	m.overlayPanel.title = "Full Transcript"
	m.overlayPanel.lines = lines
	m.overlayPanel.cursor = cursor
}

// openOverlay 打开一个通用详情弹窗。
func (m *Model) openOverlay(title string, lines []string) {
	m.overlayPanel.open(title, lines)
}

// handleOverlayEnter 在弹窗内按 Enter 时打开当前光标对应条目的详情。
func (m *Model) handleOverlayEnter() {
	switch m.overlayPanel.mode {
	case overlayPlan:
		m.showPlanDetailByIndex(m.overlayPanel.cursor)
	case overlayAgents:
		m.showAgentDetailByIndex(m.overlayPanel.cursor)
	}
}

// refreshOverlay 根据最新状态刷新当前弹窗的内容行，实现实时更新。
func (m *Model) refreshOverlay() {
	switch m.overlayPanel.mode {
	case overlayPlan:
		m.overlayPanel.lines = m.buildPlanLines()
	case overlayAgents:
		m.overlayPanel.lines = m.buildAgentsLines()
	case overlayLog:
		// 若光标原在末尾，刷新后仍保持在末尾以跟随新输出。
		wasAtEnd := len(m.overlayPanel.lines) > 0 && m.overlayPanel.cursor >= len(m.overlayPanel.lines)-1
		m.overlayPanel.lines = m.buildTranscriptLines()
		if wasAtEnd {
			m.overlayPanel.cursor = len(m.overlayPanel.lines) - 1
		}
	case overlayHelp:
		m.overlayPanel.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
}

// clampOverlayCursor 将弹窗光标限制在有效范围内。
func (m *Model) clampOverlayCursor() {
	m.overlayPanel.clampCursor()
}

// renderOverlay 为 Model 渲染当前弹窗。
func (m Model) renderOverlay(w, h int) string {
	return m.overlayPanel.render(m.styles, w, h, m.buildPlanLines(), m.agentTreePanel.buildLines(), m.buildTranscriptLines())
}
