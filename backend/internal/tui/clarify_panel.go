package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// maxClarifyQuestionLines 问题全文在问答面板内最多展示的行数（超出截断并追加省略号）。
const maxClarifyQuestionLines = 4

// maxClarifyDetailLines 提问附带长上下文（任务 140 问题①）在问答面板内最多展示的行数：
// detail 先于问题展示，超长截断（完整内容在对话区 clarify_detail 事件与 web 端可见）。
const maxClarifyDetailLines = 6

// clarifyBatchDraft 是批量问答（任务 140）单题草稿：sel 为已选选项 ID 集
//（单选至多 1 个，多选按勾选顺序），text 为自由文本草稿（文本题 / 「其他」逃生 /
// 输入框覆盖输入）。answer() 文本优先——最近一次操作落定的形态生效。
type clarifyBatchDraft struct {
	sel  []string
	text string
}

// answer 计算本题草稿的答复文本：文本草稿优先（输入框输入覆盖选项选择），
// 否则选项集 join ","（多选逗号分隔，与后端 parseClarifyAnswer 口径一致）。
// 空白返回 ""（视为未作答）。
func (d clarifyBatchDraft) answer() string {
	if t := strings.TrimSpace(d.text); t != "" {
		return t
	}
	return strings.Join(d.sel, ",")
}

// buildClarifyBatchAnswers 汇总批量问答逐题草稿为整组提交的 answers（纯函数，便于单测）：
// 按题序对齐 PendingClarify.Questions；任一题答复为空白时 ok=false
//（问题③确认规则：必须全部作答才能提交）。
func buildClarifyBatchAnswers(questions []types.ClarifyQuestionItem, drafts map[int]clarifyBatchDraft) ([]string, bool) {
	answers := make([]string, 0, len(questions))
	for i := range questions {
		a := drafts[i].answer()
		if strings.TrimSpace(a) == "" {
			return nil, false
		}
		answers = append(answers, a)
	}
	return answers, true
}

// pendingClarify 返回当前选中会话待答复的澄清请求；非等待状态或无请求时返回 nil。
func (m *Model) pendingClarify() *types.ClarifyRequest {
	s := m.selectedSession()
	if s == nil || s.Status != enums.SessionStatusAwaitingClarify || s.State == nil {
		return nil
	}
	return s.State.PendingClarify
}

// batchClarifyActive 批量问答模式激活（任务 140）：PendingClarify 携带 >1 道题。
func (m *Model) batchClarifyActive() bool {
	pc := m.pendingClarify()
	return pc != nil && len(pc.Questions) > 1
}

// currentClarifyItem 返回批量模式下当前页的题目；非批量（或页码越界）返回 nil。
func (m *Model) currentClarifyItem(pc *types.ClarifyRequest) *types.ClarifyQuestionItem {
	if pc == nil || len(pc.Questions) <= 1 || m.clarifyPage < 0 || m.clarifyPage >= len(pc.Questions) {
		return nil
	}
	return &pc.Questions[m.clarifyPage]
}

// currentClarifyOptions 返回选项导航应作用的选项列表：批量=当前页题目选项，
// 单题=顶层 Options。
func (m *Model) currentClarifyOptions(pc *types.ClarifyRequest) []types.ClarifyOption {
	if item := m.currentClarifyItem(pc); item != nil {
		return item.Options
	}
	if pc == nil {
		return nil
	}
	return pc.Options
}

// clarifyNavActive 判断选项导航（↑/↓/空格/回车）是否接管按键：
// 输入栏处于澄清模式且当前澄清请求（批量=当前页题目）带结构化选项。
func (m *Model) clarifyNavActive() bool {
	pc := m.pendingClarify()
	if pc == nil || m.inputBar.mode != inputClarify {
		return false
	}
	return len(m.currentClarifyOptions(pc)) > 0
}

// clarifyCursorClamped 返回钳制在选项范围内的当前光标下标。
func (m *Model) clarifyCursorClamped(pc *types.ClarifyRequest) int {
	return m.clarifyCursorClampedFor(m.currentClarifyOptions(pc))
}

// clarifyCursorClampedFor 返回钳制在给定选项范围内的当前光标下标。
func (m *Model) clarifyCursorClampedFor(opts []types.ClarifyOption) int {
	if m.clarifyCursor < 0 {
		return 0
	}
	if m.clarifyCursor >= len(opts) {
		return len(opts) - 1
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
// 标题行（❓ Agent 提问 / ⚠️ 操作确认 / 批量标注）+ 上下文 + 问题全文 + 选项列表
//（高亮光标行）+ 按键提示。无待答复澄清时返回空串。
func (m *Model) renderClarifyPanel(w int) string {
	pc := m.pendingClarify()
	if pc == nil {
		return ""
	}
	title := "❓ Agent 提问"
	if pc.Kind == "confirm" {
		title = "⚠️ 操作确认"
	}
	if len(pc.Questions) > 1 {
		title = "❓ Agent 提问（批量）"
	}
	header := m.styles.PanelHeader.Width(w).Render(title)
	return lipgloss.JoinVertical(lipgloss.Top, header, strings.Join(m.clarifyPanelLines(pc, w), "\n"))
}

// clarifyPanelLines 构建问答面板内容行（不含标题行）：批量模式走分页构建，
// 单题维持原状（问题全文 + 选项列表 + 按键提示）。
func (m *Model) clarifyPanelLines(pc *types.ClarifyRequest, w int) []string {
	if len(pc.Questions) > 1 {
		return m.clarifyBatchPanelLines(pc, w)
	}
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}

	var lines []string
	// 长上下文（任务 140 问题①）：detail 先于问题展示，超 maxClarifyDetailLines 截断。
	lines = append(lines, m.clarifyDetailLines(pc.Detail, innerW)...)

	// 问题全文：剥离后端前缀（与对话区 eventChatItem 的展示口径一致）。
	question := strings.TrimSpace(pc.Question)
	question = strings.TrimPrefix(question, "Agent 提问: ")
	qLines := wrapToWidth(question, innerW)
	if len(qLines) > maxClarifyQuestionLines {
		qLines = qLines[:maxClarifyQuestionLines]
		qLines[maxClarifyQuestionLines-1] = truncate(qLines[maxClarifyQuestionLines-1], innerW-1) + "…"
	}
	lines = append(lines, qLines...)

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

// clarifyDetailLines 构建长上下文块行（任务 140 问题①）：detail 先于问题展示，
// 超 maxClarifyDetailLines 行截断；空 detail 返回 nil。
func (m *Model) clarifyDetailLines(detail string, innerW int) []string {
	d := strings.TrimSpace(detail)
	if d == "" {
		return nil
	}
	dLines := wrapToWidth("📋 上下文: "+d, innerW)
	if len(dLines) > maxClarifyDetailLines {
		dLines = dLines[:maxClarifyDetailLines]
		dLines[maxClarifyDetailLines-1] = truncate(dLines[maxClarifyDetailLines-1], innerW-1) + "…"
	}
	return append(dLines, "")
}

// clarifyBatchPanelLines 构建批量问答面板内容行（任务 140 问题③）：
// 上下文 → 当前进度（题 i/N · 已答 ●●○○）→ 当前题问题 → 当前题选项/文本草稿 → 按键提示。
// 每次只渲染当前页一题（同屏分页），←/→ 切题回改。
func (m *Model) clarifyBatchPanelLines(pc *types.ClarifyRequest, w int) []string {
	innerW := w - 4
	if innerW < 20 {
		innerW = 20
	}
	n := len(pc.Questions)
	page := m.clarifyPage
	if page < 0 || page >= n {
		page = 0
	}
	item := &pc.Questions[page]
	draft := m.clarifyDrafts[page]

	var lines []string
	// 长上下文先于问题（任务 140 问题①）
	lines = append(lines, m.clarifyDetailLines(pc.Detail, innerW)...)

	// 进度行：页码 + 已答圆点（● 已答 / ○ 未答）
	dots := make([]string, 0, n)
	answered := 0
	for i := range pc.Questions {
		if strings.TrimSpace(m.clarifyDrafts[i].answer()) != "" {
			dots = append(dots, "●")
			answered++
		} else {
			dots = append(dots, "○")
		}
	}
	lines = append(lines, m.styles.Dim.Render(
		fmt.Sprintf("题 %d/%d · 已答 %d/%d  %s", page+1, n, answered, n, strings.Join(dots, ""))))

	// 当前题问题全文（超行截断）
	qLines := wrapToWidth(strings.TrimSpace(item.Question), innerW)
	if len(qLines) > maxClarifyQuestionLines {
		qLines = qLines[:maxClarifyQuestionLines]
		qLines[maxClarifyQuestionLines-1] = truncate(qLines[maxClarifyQuestionLines-1], innerW-1) + "…"
	}
	lines = append(lines, qLines...)

	if len(item.Options) > 0 {
		lines = append(lines, "")
		cursor := m.clarifyCursorClampedFor(item.Options)
		highlight := lipgloss.NewStyle().Foreground(lipgloss.Color(cSub)).Bold(true)
		for i, opt := range item.Options {
			marker := "○"
			switch {
			case item.MultiSelect && batchDraftSelected(draft, opt.ID):
				marker = "☑"
			case item.MultiSelect:
				marker = "☐"
			case len(draft.sel) > 0 && draft.sel[0] == opt.ID:
				// 单选页已定草稿项与光标项冲突时以草稿为准（显示既定选择）
				marker = "●"
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
	} else if a := strings.TrimSpace(draft.answer()); a != "" {
		// 文本题：展示当前草稿预览
		lines = append(lines, m.styles.Dim.Render("草稿: "+truncate(a, innerW-4)))
	}

	lines = append(lines, m.styles.Dim.Render(clarifyBatchKeyHint(item)))
	return lines
}

// batchDraftSelected 判断选项 ID 是否在该题草稿选择集中。
func batchDraftSelected(d clarifyBatchDraft, id string) bool {
	for _, s := range d.sel {
		if s == id {
			return true
		}
	}
	return false
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

// clarifyBatchKeyHint 返回批量问答当前页的按键提示文案（按选项/文本题区分）。
func clarifyBatchKeyHint(item *types.ClarifyQuestionItem) string {
	if len(item.Options) == 0 {
		return "←/→ 切题 · 输入文字回车记入本题 · 全部作答后回车统一提交"
	}
	if item.MultiSelect {
		return "←/→ 切题 · ↑/↓ 移动 · 空格勾选 · 回车下一题 · 全部作答后回车统一提交"
	}
	return "←/→ 切题 · ↑/↓ 选择 · 空格/数字记入本题 · 回车下一题 · 全部作答后回车统一提交"
}

// clarifySubmitAnswer 计算选项导航模式下的提交答案（纯计算，不发请求）：
// 单选返回高亮项 ID；多选返回勾选集 join ","（空勾选返回 ok=false，由调用方提示用户）。
// 无选项时 ok=false（走自由文本路径）。
func (m *Model) clarifySubmitAnswer(pc *types.ClarifyRequest) (answer string, ok bool) {
	if len(m.currentClarifyOptions(pc)) == 0 {
		return "", false
	}
	if pc.MultiSelect {
		if len(m.clarifySel) == 0 {
			return "", false
		}
		return strings.Join(m.clarifySel, ","), true
	}
	return m.currentClarifyOptions(pc)[m.clarifyCursorClamped(pc)].ID, true
}
