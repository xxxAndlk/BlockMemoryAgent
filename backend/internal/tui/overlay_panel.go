package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// OverlayPanel owns the overlay/popup state: mode, title, lines, cursor and kind.
type OverlayPanel struct {
	mode   int
	title  string
	lines  []string
	cursor int
	kind   int
}

// NewOverlayPanel constructs an empty OverlayPanel.
func NewOverlayPanel() OverlayPanel {
	return OverlayPanel{}
}

func (op *OverlayPanel) open(title string, lines []string) {
	op.mode = overlayDetail
	op.kind = overlayDetail
	op.title = title
	op.lines = lines
	op.cursor = 0
}

func (op *OverlayPanel) openHelp() {
	op.mode = overlayHelp
	op.kind = overlayHelp
	op.title = "Help"
	op.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	op.cursor = 0
}

func (op *OverlayPanel) close() {
	op.mode = overlayNone
}

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

func (op *OverlayPanel) render(styles *Styles, w, h int, planLines, agentsLines, transcriptLines []string) string {
	if op.mode == overlayHelp {
		op.title = "Help"
		op.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
	if op.mode == overlayPlan {
		op.lines = planLines
	} else if op.mode == overlayAgents {
		op.lines = agentsLines
	} else if op.mode == overlayLog {
		op.lines = transcriptLines
	}

	maxLines := h - 4
	if maxLines < 3 {
		maxLines = 3
	}
	if op.cursor < 0 {
		op.cursor = 0
	}
	if op.cursor >= len(op.lines) {
		op.cursor = len(op.lines) - 1
	}
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

	boxW := w * 4 / 5
	if boxW < 40 {
		boxW = w - 4
	}
	boxH := h - 2

	header := styles.Header.Render(op.title)
	hint := styles.Dim.Render("  [Esc close · j/k scroll · enter detail]")
	body := lipgloss.JoinVertical(lipgloss.Left, header+hint, content)
	box := styles.Overlay.Width(boxW).Height(boxH).Render(body)
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, box)
}

// Model-facing wrappers keep existing call sites stable.

func (m *Model) openHelpPopup() {
	m.overlayPanel.openHelp()
}

func (m *Model) togglePlanPopup() {
	if !m.hasPlan() {
		m.flashMsg("no plan available")
		return
	}
	if m.overlayPanel.mode == overlayPlan {
		m.overlayPanel.close()
		return
	}
	lines := m.buildPlanLines()
	m.overlayPanel.mode = overlayPlan
	m.overlayPanel.kind = overlayPlan
	m.overlayPanel.title = "Execution Plan"
	m.overlayPanel.lines = lines
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(lines)-1)
}

func (m *Model) toggleAgentsPopup() {
	if len(m.agentTreePanel.nodes) == 0 {
		m.flashMsg("no agents available")
		return
	}
	if m.overlayPanel.mode == overlayAgents {
		m.overlayPanel.close()
		return
	}
	lines := m.buildAgentsLines()
	m.overlayPanel.mode = overlayAgents
	m.overlayPanel.kind = overlayAgents
	m.overlayPanel.title = "Agent Topology"
	m.overlayPanel.lines = lines
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(lines)-1)
}

func (m *Model) toggleLogPopup() {
	s := m.selectedSession()
	if s == nil {
		m.flashMsg("no active session")
		return
	}
	if m.overlayPanel.mode == overlayLog {
		m.overlayPanel.close()
		return
	}
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

func (m *Model) openOverlay(title string, lines []string) {
	m.overlayPanel.open(title, lines)
}

func (m *Model) handleOverlayEnter() {
	switch m.overlayPanel.mode {
	case overlayPlan:
		m.showPlanDetailByIndex(m.overlayPanel.cursor)
	case overlayAgents:
		m.showAgentDetailByIndex(m.overlayPanel.cursor)
	}
}

func (m *Model) refreshOverlay() {
	switch m.overlayPanel.mode {
	case overlayPlan:
		m.overlayPanel.lines = m.buildPlanLines()
	case overlayAgents:
		m.overlayPanel.lines = m.buildAgentsLines()
	case overlayLog:
		wasAtEnd := len(m.overlayPanel.lines) > 0 && m.overlayPanel.cursor >= len(m.overlayPanel.lines)-1
		m.overlayPanel.lines = m.buildTranscriptLines()
		if wasAtEnd {
			m.overlayPanel.cursor = len(m.overlayPanel.lines) - 1
		}
	case overlayHelp:
		m.overlayPanel.lines = strings.Split(strings.Trim(fullHelpText, "\n"), "\n")
	}
}

func (m *Model) clampOverlayCursor() {
	m.overlayPanel.clampCursor()
}

func (m Model) renderOverlay(w, h int) string {
	return m.overlayPanel.render(m.styles, w, h, m.buildPlanLines(), m.agentTreePanel.buildLines(), m.buildTranscriptLines())
}

