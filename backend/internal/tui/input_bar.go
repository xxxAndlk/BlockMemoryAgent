package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// InputBar 维护底部输入栏的状态，包括输入模式、当前字符、光标位置以及按会话保存的历史记录。
type InputBar struct {
	// mode 是输入栏当前模式，取值见 keys.go 中的 inputNormal/inputClarify 等常量。
	mode int
	// runes 是当前输入内容的 rune 切片，支持中文等多字节字符。
	runes []rune
	// cursor 是光标在 runes 中的位置（0 表示开头）。
	cursor int
	// history 按会话 ID 保存历史输入列表。
	history map[string][]string
	// histIdx 是当前在历史列表中的浏览位置，-1 表示未浏览历史。
	histIdx int
	// lastKeyTime 记录上一次按键时间，用于区分终端粘贴的快速 Enter 与手动回车。
	lastKeyTime time.Time
}

// NewInputBar 构造一个空的 InputBar，初始化历史记录 map 并将历史索引设为 -1。
func NewInputBar() InputBar {
	return InputBar{
		history: make(map[string][]string),
		histIdx: -1,
	}
}

// reset 清空输入栏内容、光标、模式与历史索引，用于发送后或取消输入时重置。
func (ib *InputBar) reset() {
	ib.runes = nil
	ib.cursor = 0
	ib.mode = inputNormal
	ib.histIdx = -1
}

// isMultiline 判断当前输入是否包含换行符，用于决定输入栏渲染为占位符还是展开文本。
func (ib *InputBar) isMultiline() bool {
	return strings.ContainsRune(string(ib.runes), '\n')
}

// insertRunes 在光标位置插入一段 rune，并向后移动光标。
func (ib *InputBar) insertRunes(runes []rune) {
	ib.runes = append(ib.runes[:ib.cursor], append(runes, ib.runes[ib.cursor:]...)...)
	ib.cursor += len(runes)
}

// backspace 删除光标前一个字符。多行输入时一次性清空全部内容。
func (ib *InputBar) backspace() {
	if ib.isMultiline() {
		// 多行内容一次性清空，避免逐字符删除长文本。
		ib.runes = nil
		ib.cursor = 0
	} else if ib.cursor > 0 {
		// 单行模式下删除光标前一个 rune。
		ib.runes = append(ib.runes[:ib.cursor-1], ib.runes[ib.cursor:]...)
		ib.cursor--
	}
}

// delete 删除光标后一个字符。多行输入时一次性清空全部内容。
func (ib *InputBar) delete() {
	if ib.isMultiline() {
		// 多行内容一次性清空。
		ib.runes = nil
		ib.cursor = 0
	} else if ib.cursor < len(ib.runes) {
		// 单行模式下删除光标后一个 rune。
		ib.runes = append(ib.runes[:ib.cursor], ib.runes[ib.cursor+1:]...)
	}
}

// moveLeft 将光标向左移动一个字符，已在开头则不移动。
func (ib *InputBar) moveLeft() {
	if ib.cursor > 0 {
		ib.cursor--
	}
}

// moveRight 将光标向右移动一个字符，已在末尾则不移动。
func (ib *InputBar) moveRight() {
	if ib.cursor < len(ib.runes) {
		ib.cursor++
	}
}

// moveHome 将光标移到输入内容开头。
func (ib *InputBar) moveHome() {
	ib.cursor = 0
}

// moveEnd 将光标移到输入内容末尾。
func (ib *InputBar) moveEnd() {
	ib.cursor = len(ib.runes)
}

// getHistory 返回指定会话 ID 对应的历史输入列表。
func (ib *InputBar) getHistory(sessionID string) []string {
	return ib.history[sessionID]
}

// pushHistory 将一条命令追加到指定会话的历史记录中。
func (ib *InputBar) pushHistory(sessionID, cmd string) {
	ib.history[sessionID] = append(ib.history[sessionID], cmd)
}

// historyUp 在历史记录中向上浏览（更早的命令），并将该命令填入输入栏。
func (ib *InputBar) historyUp(sessionID string) {
	h := ib.history[sessionID]
	// 没有历史记录时直接返回。
	if len(h) == 0 {
		return
	}
	// 第一次按上箭头时，将索引设为历史末尾（最新一条之后）。
	if ib.histIdx == -1 {
		ib.histIdx = len(h)
	}
	// 若还有更早的命令，则前移并填充输入栏。
	if ib.histIdx > 0 {
		ib.histIdx--
		ib.runes = []rune(h[ib.histIdx])
		ib.cursor = len(ib.runes)
	}
}

// historyDown 在历史记录中向下浏览（更新的命令），最后一条之后恢复空输入。
func (ib *InputBar) historyDown(sessionID string) {
	h := ib.history[sessionID]
	// 未处于历史浏览状态时直接返回。
	if ib.histIdx == -1 {
		return
	}
	// 若还有更新的命令，则后移并填充输入栏。
	if ib.histIdx < len(h)-1 {
		ib.histIdx++
		ib.runes = []rune(h[ib.histIdx])
		ib.cursor = len(ib.runes)
	} else {
		// 到达最新命令之后，恢复空输入并退出历史浏览模式。
		ib.histIdx = -1
		ib.runes = nil
		ib.cursor = 0
	}
}

// render 渲染输入栏，参数 w 为可用宽度，focused 表示是否获得焦点，flash 是顶部闪屏提示文本，
// hint 是第二行的引导提示（如待澄清时的操作引导），为空时不占行。
func (ib *InputBar) render(styles *Styles, w int, focused bool, flash, hint string) string {
	// 根据当前模式选择提示符。
	prompt := ">"
	switch ib.mode {
	case inputClarify:
		prompt = "/clarify>"
	case inputInterrupt:
		prompt = "/interrupt>"
	case inputEnqueue:
		prompt = "/enqueue>"
	}
	// 左侧固定显示提示符。
	left := styles.InputPrompt.Render(" " + prompt + " ")
	// 光标样式：获得焦点时显示竖线，否则显示空格。
	cursor := " "
	if focused {
		cursor = "▌"
	}

	// 根据输入状态生成文本内容。
	var text string
	if len(ib.runes) == 0 {
		// 空输入时展示提示文案。
		text = styles.InputHint.Render("Type your message... (Enter to send, / for commands)")
	} else if ib.isMultiline() {
		// 多行输入折叠为 [n行内容] 占位，保留原始内容用于发送。
		lines := strings.Count(string(ib.runes), "\n") + 1
		text = styles.InputText.Render(fmt.Sprintf("[%d行内容]%s", lines, cursor))
	} else {
		// 单行输入在光标位置插入光标符号。
		text = string(ib.runes[:ib.cursor]) + cursor + string(ib.runes[ib.cursor:])
	}

	// 根据焦点状态选择边框样式。
	border := styles.BlurBorder
	if focused {
		border = styles.FocusBorder
	}
	// 拼接提示符、文本和闪屏提示，并套入边框与固定高度。
	content := lipgloss.JoinVertical(lipgloss.Left, left+" "+text+flash, hint)
	return border.Width(w - 2).Height(3).Render(content)
}

// 以下是为 Model 提供的便捷封装，保持调用点稳定。

// inputIsMultiline 返回 Model 的输入栏当前是否处于多行状态。
func (m *Model) inputIsMultiline() bool {
	return m.inputBar.isMultiline()
}

// sessionHistory 返回当前选中会话的输入历史列表。
func (m *Model) sessionHistory() []string {
	s := m.selectedSession()
	if s == nil {
		return nil
	}
	return m.inputBar.getHistory(s.ID)
}

// pushHistory 将命令追加到当前选中会话的输入历史。
func (m *Model) pushHistory(cmd string) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	m.inputBar.pushHistory(s.ID, cmd)
}

// renderInput 为 Model 渲染输入栏，包括当前闪屏提示。
func (m Model) renderInput(w int) string {
	// 从指针共享状态读取 flash（含过期判断）；后台 goroutine 的写入经 sharedState
	// 对所有 Model 拷贝可见（#47 修复）。
	curFlash := m.shared.getFlash()
	// 若有闪屏信息，则在输入栏右侧以错误样式渲染。
	flash := ""
	if curFlash != "" {
		flash = "  " + m.styles.LogError.Render(curFlash)
	}
	// TODO #53：会话等待澄清时在输入栏第二行追加引导提示（按键说明见上方问答面板）。
	hint := ""
	if s := m.selectedSession(); s != nil && s.Status == enums.SessionStatusAwaitingClarify && s.State != nil && s.State.PendingClarify != nil {
		hint = m.styles.InputHint.Render("⏳ Agent 等待答复：上方面板选择选项，或直接输入文字回车提交")
		if len(s.State.PendingClarify.Options) > 0 {
			hint += "\n" + m.styles.InputHint.Render("（↑/↓ 选择 · 回车提交 · 数字键/y/n 快捷选择）")
		}
	}
	return m.inputBar.render(m.styles, w, m.focus == panelInput, flash, hint)
}
