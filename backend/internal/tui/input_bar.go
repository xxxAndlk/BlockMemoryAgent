package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// InputBar owns the input state: mode, runes, cursor and per-session history.
type InputBar struct {
	mode        int
	runes       []rune
	cursor      int
	history     map[string][]string
	histIdx     int
	lastKeyTime time.Time
}

// NewInputBar constructs an InputBar with an empty history map.
func NewInputBar() InputBar {
	return InputBar{
		history: make(map[string][]string),
		histIdx: -1,
	}
}

func (ib *InputBar) reset() {
	ib.runes = nil
	ib.cursor = 0
	ib.mode = inputNormal
	ib.histIdx = -1
}

func (ib *InputBar) isMultiline() bool {
	return strings.ContainsRune(string(ib.runes), '\n')
}

func (ib *InputBar) insertRunes(runes []rune) {
	ib.runes = append(ib.runes[:ib.cursor], append(runes, ib.runes[ib.cursor:]...)...)
	ib.cursor += len(runes)
}

func (ib *InputBar) backspace() {
	if ib.isMultiline() {
		ib.runes = nil
		ib.cursor = 0
	} else if ib.cursor > 0 {
		ib.runes = append(ib.runes[:ib.cursor-1], ib.runes[ib.cursor:]...)
		ib.cursor--
	}
}

func (ib *InputBar) delete() {
	if ib.isMultiline() {
		ib.runes = nil
		ib.cursor = 0
	} else if ib.cursor < len(ib.runes) {
		ib.runes = append(ib.runes[:ib.cursor], ib.runes[ib.cursor+1:]...)
	}
}

func (ib *InputBar) moveLeft() {
	if ib.cursor > 0 {
		ib.cursor--
	}
}

func (ib *InputBar) moveRight() {
	if ib.cursor < len(ib.runes) {
		ib.cursor++
	}
}

func (ib *InputBar) moveHome() {
	ib.cursor = 0
}

func (ib *InputBar) moveEnd() {
	ib.cursor = len(ib.runes)
}

func (ib *InputBar) getHistory(sessionID string) []string {
	return ib.history[sessionID]
}

func (ib *InputBar) pushHistory(sessionID, cmd string) {
	ib.history[sessionID] = append(ib.history[sessionID], cmd)
}

func (ib *InputBar) historyUp(sessionID string) {
	h := ib.history[sessionID]
	if len(h) == 0 {
		return
	}
	if ib.histIdx == -1 {
		ib.histIdx = len(h)
	}
	if ib.histIdx > 0 {
		ib.histIdx--
		ib.runes = []rune(h[ib.histIdx])
		ib.cursor = len(ib.runes)
	}
}

func (ib *InputBar) historyDown(sessionID string) {
	h := ib.history[sessionID]
	if ib.histIdx == -1 {
		return
	}
	if ib.histIdx < len(h)-1 {
		ib.histIdx++
		ib.runes = []rune(h[ib.histIdx])
		ib.cursor = len(ib.runes)
	} else {
		ib.histIdx = -1
		ib.runes = nil
		ib.cursor = 0
	}
}

func (ib *InputBar) render(styles *Styles, w int, focused bool, flash string) string {
	prompt := ">"
	switch ib.mode {
	case inputClarify:
		prompt = "/clarify>"
	case inputInterrupt:
		prompt = "/interrupt>"
	case inputEnqueue:
		prompt = "/enqueue>"
	}
	left := styles.InputPrompt.Render(" " + prompt + " ")
	cursor := " "
	if focused {
		cursor = "▌"
	}

	var text string
	if len(ib.runes) == 0 {
		text = styles.InputHint.Render("Type your message... (Enter to send, / for commands)")
	} else if ib.isMultiline() {
		lines := strings.Count(string(ib.runes), "\n") + 1
		text = styles.InputText.Render(fmt.Sprintf("[%d行内容]%s", lines, cursor))
	} else {
		text = string(ib.runes[:ib.cursor]) + cursor + string(ib.runes[ib.cursor:])
	}

	border := styles.BlurBorder
	if focused {
		border = styles.FocusBorder
	}
	content := lipgloss.JoinVertical(lipgloss.Left, left+" "+text+flash, "")
	return border.Width(w - 2).Height(3).Render(content)
}

// Model-facing wrappers keep existing call sites stable.

func (m *Model) inputIsMultiline() bool {
	return m.inputBar.isMultiline()
}

func (m *Model) sessionHistory() []string {
	s := m.selectedSession()
	if s == nil {
		return nil
	}
	return m.inputBar.getHistory(s.ID)
}

func (m *Model) pushHistory(cmd string) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	m.inputBar.pushHistory(s.ID, cmd)
}

func (m Model) renderInput(w int) string {
	m.flashMu.Lock()
	curFlash := m.flash
	m.flashMu.Unlock()
	flash := ""
	if curFlash != "" {
		flash = "  " + m.styles.LogError.Render(curFlash)
	}
	return m.inputBar.render(m.styles, w, m.focus == panelInput, flash)
}

