package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// pressKey 触发一次按键并隔离粘贴判定：测试按键间隔 < pasteEnterThreshold 会被
// 当作多行粘贴插入换行而非提交。
func pressKey(m *Model, msg tea.KeyMsg) *Model {
	nm, _ := m.handleInputKey(msg)
	mm := nm.(*Model)
	mm.inputBar.lastKeyTime = time.Time{}
	return mm
}

// batchClarifyTestModel 构造带 3 题批量澄清请求的测试 Model（单会话、澄清输入模式）：
// 题1 单选（pg/mysql）、题2 多选（f1/f2）、题3 纯文本（无选项）。
func batchClarifyTestModel() (*Model, *types.ClarifyRequest) {
	pc := &types.ClarifyRequest{
		ID:       "ask-batch-1",
		Kind:     "choice",
		Detail:   "任务进度盘点：模块 A 已完成，模块 B 卡在依赖决策，需要定案方向。",
		Question: "数据库选哪个？",
		Options:  []types.ClarifyOption{{ID: "pg", Label: "PostgreSQL"}},
		Questions: []types.ClarifyQuestionItem{
			{Question: "数据库选哪个？", Options: []types.ClarifyOption{{ID: "pg", Label: "PostgreSQL"}, {ID: "mysql", Label: "MySQL"}}},
			{Question: "需要哪些功能？", MultiSelect: true, Options: []types.ClarifyOption{{ID: "f1", Label: "功能一"}, {ID: "f2", Label: "功能二"}}},
			{Question: "命名风格？"},
		},
	}
	m := clarifyTestModel(pc)
	m.inputBar.mode = inputClarify
	return m, pc
}

// TestBuildClarifyBatchAnswers 验证整组答案汇总纯函数：全答才 ok、文本优先于选项、空白即拒绝。
func TestBuildClarifyBatchAnswers(t *testing.T) {
	qs := []types.ClarifyQuestionItem{{Question: "q1"}, {Question: "q2"}, {Question: "q3"}}

	// 全部已答：按题序对齐。
	answers, ok := buildClarifyBatchAnswers(qs, map[int]clarifyBatchDraft{
		0: {sel: []string{"pg"}},
		1: {sel: []string{"f1", "f2"}},
		2: {text: "kebab"},
	})
	if !ok {
		t.Fatal("全部已答应返回 ok=true")
	}
	if len(answers) != 3 || answers[0] != "pg" || answers[1] != "f1,f2" || answers[2] != "kebab" {
		t.Fatalf("答案应对齐题序并按选项集 join，got %v", answers)
	}

	// 任一题空白：拒绝提交（必须全部作答）。
	if _, ok := buildClarifyBatchAnswers(qs, map[int]clarifyBatchDraft{
		0: {sel: []string{"pg"}},
		2: {text: "kebab"},
	}); ok {
		t.Fatal("存在未答题时应返回 ok=false")
	}

	// 文本草稿优先于选项选择（最近操作覆盖）。
	answers, _ = buildClarifyBatchAnswers(qs[:1], map[int]clarifyBatchDraft{
		0: {sel: []string{"pg"}, text: "换 SQLite"},
	})
	if answers[0] != "换 SQLite" {
		t.Fatalf("文本草稿应优先，got %q", answers[0])
	}

	// 纯空白文本视为未作答。
	if _, ok := buildClarifyBatchAnswers(qs[:1], map[int]clarifyBatchDraft{
		0: {text: "   "},
	}); ok {
		t.Fatal("空白文本应视为未作答")
	}
}

// TestClarifyBatchPagerKeys 验证 ←/→ 翻页：空输入翻页、越界夹逼、非空输入维持光标语义。
func TestClarifyBatchPagerKeys(t *testing.T) {
	m, _ := batchClarifyTestModel()

	// 右边界夹逼：连按 → 停在末页。
	for i := 0; i < 5; i++ {
		m = pressKey(m, tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.clarifyPage != 2 {
		t.Fatalf("连按 → 应夹逼到末页 2，got %d", m.clarifyPage)
	}
	// 左边界夹逼：连按 ← 停在首页。
	for i := 0; i < 5; i++ {
		m = pressKey(m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	if m.clarifyPage != 0 {
		t.Fatalf("连按 ← 应夹逼到首页 0，got %d", m.clarifyPage)
	}

	// 输入非空时 ←/→ 维持文本光标语义，不翻页。
	m2, _ := batchClarifyTestModel()
	m2.inputBar.runes = []rune("abc")
	m2.inputBar.cursor = 1
	m2 = pressKey(m2, tea.KeyMsg{Type: tea.KeyRight})
	if m2.clarifyPage != 0 || m2.inputBar.cursor != 2 {
		t.Fatalf("非空输入时 → 应移动文本光标（cursor=2, page=0），got cursor=%d page=%d", m2.inputBar.cursor, m2.clarifyPage)
	}
	m2 = pressKey(m2, tea.KeyMsg{Type: tea.KeyLeft})
	if m2.clarifyPage != 0 || m2.inputBar.cursor != 1 {
		t.Fatalf("非空输入时 ← 应移动文本光标，got cursor=%d page=%d", m2.inputBar.cursor, m2.clarifyPage)
	}
}

// TestClarifyBatchDraftsPerPage 验证逐题草稿：数字/空格写当前页草稿、多选切换、翻页后光标复位。
func TestClarifyBatchDraftsPerPage(t *testing.T) {
	m, _ := batchClarifyTestModel()

	// 题1（单选）：数字 1 → 记 pg 草稿（不提交，等待整组）。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if d := m.clarifyDrafts[0]; len(d.sel) != 1 || d.sel[0] != "pg" || d.answer() != "pg" {
		t.Fatalf("数字 1 应记 pg 草稿，got %+v", m.clarifyDrafts[0])
	}
	if m.inputBar.mode != inputClarify {
		t.Fatal("批量模式记草稿不应退出澄清模式")
	}

	// 回车：本题已答 → 自动跳下一未答题（题2）。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.clarifyPage != 1 {
		t.Fatalf("回车应跳到题2，got page=%d", m.clarifyPage)
	}

	// 题2（多选）：数字 2 → 切换 f2 勾选。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if d := m.clarifyDrafts[1]; !batchDraftSelected(d, "f2") || batchDraftSelected(d, "f1") {
		t.Fatalf("数字 2 应勾选 f2，got %+v", d)
	}
	// 再按 2 → 取消勾选。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if d := m.clarifyDrafts[1]; batchDraftSelected(d, "f2") {
		t.Fatalf("再次数字 2 应取消 f2，got %+v", d)
	}
	// 空格勾选高亮项 f1（光标复位后为 0）。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if d := m.clarifyDrafts[1]; !batchDraftSelected(d, "f1") {
		t.Fatalf("空格应勾选高亮项 f1，got %+v", d)
	}
	// 空格 + 多选 + 输入非空 → 普通空格插入，不消费。
	m2, _ := batchClarifyTestModel()
	m2.clarifyPage = 1
	m2.inputBar.runes = []rune("x")
	m2 = pressKey(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" ")})
	if len(m2.inputBar.runes) != 2 {
		t.Fatalf("非空输入时空格应插入文本，got runes=%q", string(m2.inputBar.runes))
	}
	if _, ok := m2.clarifyDrafts[1]; ok {
		t.Fatalf("非空输入时空格不应写草稿，got %+v", m2.clarifyDrafts[1])
	}

	// 回车跳到题3（文本题）。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.clarifyPage != 2 {
		t.Fatalf("回车应跳到题3，got page=%d", m.clarifyPage)
	}

	// 题3（文本题）：字符进入输入框（快捷键不消费），回车记文本草稿并整组提交。
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("kebab")})
	if string(m.inputBar.runes) != "kebab" {
		t.Fatalf("文本题字符应进输入框，got %q", string(m.inputBar.runes))
	}
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	// 前 1、2 题已答 + 本题文本落草稿 → 全部已答 → 整组提交：退出澄清模式 + 5s 确认闪屏。
	if m.inputBar.mode != inputNormal {
		t.Fatalf("整组提交后应退出澄清模式，got mode=%d", m.inputBar.mode)
	}
	if flash := m.ensureShared().getFlash(); !strings.Contains(flash, "已收到") || !strings.Contains(flash, "正在思考中") {
		t.Fatalf("整组提交应有「已收到，正在思考中…」确认，got %q", flash)
	}
	if m.clarifyDrafts != nil || m.clarifyPage != 0 {
		t.Fatalf("整组提交后应清草稿与页码，got drafts=%v page=%d", m.clarifyDrafts, m.clarifyPage)
	}

	// 文本题回车记草稿（其余题未答时不触发整组提交，跳回下一未答题）。
	m3, _ := batchClarifyTestModel()
	m3.clarifyPage = 2
	m3 = pressKey(m3, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("kebab")})
	m3 = pressKey(m3, tea.KeyMsg{Type: tea.KeyEnter})
	if d := m3.clarifyDrafts[2]; d.answer() != "kebab" {
		t.Fatalf("回车应记题3文本草稿，got %+v", d)
	}
	if m3.inputBar.mode != inputClarify {
		t.Fatalf("未全部作答不应退出澄清模式，got mode=%d", m3.inputBar.mode)
	}
	if m3.clarifyPage != 0 {
		t.Fatalf("记草稿后应跳到下一未答题（题1），got page=%d", m3.clarifyPage)
	}
}

// TestClarifyBatchEnterUnanswered 验证未作答回车的拦截：不提交、提示作答方式。
func TestClarifyBatchEnterUnanswered(t *testing.T) {
	m, _ := batchClarifyTestModel()
	m = pressKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.inputBar.mode != inputClarify {
		t.Fatal("未作答回车不应退出澄清模式")
	}
	if flash := m.ensureShared().getFlash(); !strings.Contains(flash, "未作答") {
		t.Fatalf("未作答回车应提示作答方式，got %q", flash)
	}
}

// TestClarifyBatchPanelRender 验证批量面板渲染：detail 块先于问题、进度行、高度一致。
func TestClarifyBatchPanelRender(t *testing.T) {
	m, _ := batchClarifyTestModel()
	out := m.renderClarifyPanel(100)
	if got := len(strings.Split(out, "\n")); got != m.clarifyPanelHeight() {
		t.Fatalf("渲染行数 %d 应与高度预算 %d 一致", got, m.clarifyPanelHeight())
	}
	for _, want := range []string{"❓ Agent 提问（批量）", "📋 上下文", "任务进度盘点", "题 1/3", "已答 0/3", "数据库选哪个？", "❯ ● PostgreSQL", "○ MySQL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("批量面板渲染应包含 %q，got:\n%s", want, out)
		}
	}
	// detail 先于问题出现。
	if strings.Index(out, "任务进度盘点") > strings.Index(out, "数据库选哪个？") {
		t.Fatal("上下文应展示在问题之前")
	}

	// 长上下文截断：detail 超 maxClarifyDetailLines 行时面板高度有界。
	pc := &types.ClarifyRequest{ID: "ask-d", Question: "q",
		Questions: []types.ClarifyQuestionItem{{Question: "q1"}, {Question: "q2"}},
		Detail:    strings.Repeat("很长的上下文行\n", 30)}
	m2 := clarifyTestModel(pc)
	m2.inputBar.mode = inputClarify
	detailLines := 0
	for _, line := range strings.Split(m2.renderClarifyPanel(100), "\n") {
		if strings.Contains(line, "很长的上下文行") {
			detailLines++
		}
	}
	if detailLines > maxClarifyDetailLines {
		t.Fatalf("上下文展示应不超过 %d 行，got %d", maxClarifyDetailLines, detailLines)
	}
}

// TestSyncInputModeBatchReset 验证批量状态随澄清请求变化重置：新问题 ID 清页码草稿、
// 离开待澄清清全部批量状态。
func TestSyncInputModeBatchReset(t *testing.T) {
	m, pc := batchClarifyTestModel()
	m.clarifyID = pc.ID
	m.clarifyPage = 2
	m.clarifyDrafts = map[int]clarifyBatchDraft{0: {sel: []string{"pg"}}}

	// 同一请求：保持状态。
	m.syncInputMode()
	if m.clarifyPage != 2 || len(m.clarifyDrafts) != 1 {
		t.Fatalf("同一澄清请求不应重置批量状态，got page=%d drafts=%v", m.clarifyPage, m.clarifyDrafts)
	}

	// 新请求（ID 变化）：重置页码与草稿。
	pc2 := &types.ClarifyRequest{ID: "ask-batch-2", Question: "新一批",
		Questions: []types.ClarifyQuestionItem{{Question: "a"}, {Question: "b"}}}
	m.sessions[0].State = &types.ThreeLayerState{PendingClarify: pc2}
	m.syncInputMode()
	if m.clarifyPage != 0 || m.clarifyDrafts != nil {
		t.Fatalf("新澄清请求应重置批量状态，got page=%d drafts=%v", m.clarifyPage, m.clarifyDrafts)
	}

	// 离开待澄清：清全部批量状态与输入模式。
	m.sessions[0].Status = enums.SessionStatusRunning
	m.syncInputMode()
	if m.inputBar.mode == inputClarify || m.clarifyPage != 0 || m.clarifyDrafts != nil {
		t.Fatalf("离开待澄清应清批量状态，got mode=%d page=%d drafts=%v", m.inputBar.mode, m.clarifyPage, m.clarifyDrafts)
	}
}
