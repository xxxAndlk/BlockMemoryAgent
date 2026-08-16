package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// maxClarifyQuestionLines 问题全文在问答面板内最多展示的行数（超出截断并追加省略号）。
const maxClarifyQuestionLines = 4

// pendingClarify 返回当前选中会话待答复的澄清请求；非等待状态或无请求时返回 nil。
func (m *Model) pendingClarify() *types.ClarifyRequest {
	s := m.selectedSession()
	if s == nil || s.Status != enums.SessionStatusAwaitingClarify || s.State == nil {
		return nil
	}
	return s.State.PendingClarify
}

// clarifyNavActive 判断选项导航（↑/↓/空格/回车）是否接管按键：
// 输入栏处于澄清模式且当前澄清请求带结构化选项。
func (m *Model) clarifyNavActive() bool {
	pc := m.pendingClarify()
	return m.inputBar.mode == inputClarify && pc != nil && len(pc.Options) > 0
}

// clarifyCursorClamped 返回钳制在选项范围内的当前光标下标。
func (m *Model) clarifyCursorClamped(pc *types.ClarifyRequest) int {
	if m.clarifyCursor < 0 {
		return 0
	}
	if m.clarifyCursor >= len(pc.Options) {
		return len(pc.Options) - 1
	}
	return m.clarifyCursor
}

// clarifySelected 判断某选项 ID 是否在多选选择集中。
func (m *Model) clarifySelected(id string) bool {
	for _, s := range m.clarifySel {
		if s == id {
			return true
		}
	}
	return false
}

// clarifyPanelHeight 返回问答面板占用的行数（含标题行）：
// 无待答复澄清时为 0（面板不渲染）；否则为 1 标题行 + 内容行数。
// 行数取自 clarifyPanelLines 的实际构建结果，保证与渲染严格一致。
func (m *Model) clarifyPanelHeight() int {
	pc := m.pendingClarify()
	if pc == nil {
		return 0
	}
	return 1 + len(m.clarifyPanelLines(pc, m.width))
}

// renderClarifyPanel 渲染主内容区下方的问答面板：
// 标题行（❓ Agent 提问 / ⚠️ 操作确认）+ 问题全文 + 选项列表（高亮光标行）+ 按键提示。
// 无待答复澄清时返回空串。
func (m *Model) renderClarifyPanel(w int) string {
	pc := m.pendingClarify()
	if pc == nil {
		return ""
	}
	title := "❓ Agent 提问"
	if pc.Kind == "confirm" {
		title = "⚠️ 操作确认"
	}
	header := m.styles.PanelHeader.Width(w).Render(title)
	return lipgloss.JoinVertical(lipgloss.Top, header, strings.Join(m.clarifyPanelLines(pc, w), "\n"))
}

// clarifyPanelLines 构建问答面板内容行（不含标题行）：
// 问题全文（按宽度换行，超 maxClarifyQuestionLines 截断）+ 选项列表 + 按键提示。
// 选项行格式：光标行 "❯ ● <label>"（单选）/ "❯ ☑ <label>"（多选勾选态），
// 其余行两空格缩进；单选高亮项即选中项，多选勾选态取 clarifySel。
func (m *Model) clarifyPanelLines(pc *types.ClarifyRequest, w int) []string {
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}

	// 问题全文：剥离后端前缀（与对话区 eventChatItem 的展示口径一致）。
	question := strings.TrimSpace(pc.Question)
	question = strings.TrimPrefix(question, "Agent 提问: ")
	qLines := wrapToWidth(question, innerW)
	if len(qLines) > maxClarifyQuestionLines {
		qLines = qLines[:maxClarifyQuestionLines]
		qLines[maxClarifyQuestionLines-1] = truncate(qLines[maxClarifyQuestionLines-1], innerW-1) + "…"
	}
	lines := append([]string{}, qLines...)

	if len(pc.Options) > 0 {
		lines = append(lines, "")
		cursor := m.clarifyCursorClamped(pc)
		highlight := lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)).Bold(true)
		for i, opt := range pc.Options {
			marker := "○"
			switch {
			case pc.MultiSelect && m.clarifySelected(opt.ID):
				marker = "☑"
			case pc.MultiSelect:
				marker = "☐"
			case i == cursor:
				marker = "●"
			}
			text := opt.Label
			if d := strings.TrimSpace(opt.Description); d != "" {
				text += " — " + d
			}
			row := marker + " " + truncate(text, innerW-4)
			if i == cursor {
				lines = append(lines, highlight.Render("❯ "+row))
			} else {
				lines = append(lines, "  "+row)
			}
		}
	}

	lines = append(lines, m.styles.Dim.Render(clarifyKeyHint(pc)))
	return lines
}

// clarifyKeyHint 返回问答面板的按键提示文案（按确认/单选/多选区分）。
func clarifyKeyHint(pc *types.ClarifyRequest) string {
	if len(pc.Options) == 0 {
		return "直接输入文字答复，回车提交"
	}
	switch {
	case pc.Kind == "confirm":
		return "↑/↓ 选择 · 回车提交 · y=确认 / n=拒绝 · 可直接输入文字"
	case pc.MultiSelect:
		return "↑/↓ 移动 · 空格勾选 · 回车提交 · 可直接输入文字"
	default:
		return "↑/↓ 选择 · 回车提交 · 可直接输入文字"
	}
}

// clarifySubmitAnswer 计算选项导航模式下的提交答案（纯计算，不发请求）：
// 单选返回高亮项 ID；多选返回勾选集 join ","（空勾选返回 ok=false，由调用方提示用户）。
// 无选项时 ok=false（走自由文本路径）。
func (m *Model) clarifySubmitAnswer(pc *types.ClarifyRequest) (answer string, ok bool) {
	if len(pc.Options) == 0 {
		return "", false
	}
	if pc.MultiSelect {
		if len(m.clarifySel) == 0 {
			return "", false
		}
		return strings.Join(m.clarifySel, ","), true
	}
	return pc.Options[m.clarifyCursorClamped(pc)].ID, true
}
