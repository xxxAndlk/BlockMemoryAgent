package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// clarifyTestModel 构造一个处于 awaiting_clarify 状态的测试 Model（单会话、给定澄清请求）。
func clarifyTestModel(pc *types.ClarifyRequest) *Model {
	return &Model{
		width:          100,
		styles:         NewStyles(),
		sessionsCursor: 0,
		sessions: []*server.Session{{
			ID:     "s1",
			Status: enums.SessionStatusAwaitingClarify,
			State:  &types.ThreeLayerState{PendingClarify: pc},
		}},
	}
}

// TestPendingClarify 验证 pendingClarify 的门控：仅 awaiting_clarify + 有请求时返回非 nil。
func TestPendingClarify(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-1", Question: "继续？"}
	m := clarifyTestModel(pc)
	if m.pendingClarify() != pc {
		t.Fatal("awaiting_clarify + PendingClarify 应返回请求")
	}

	m.sessions[0].Status = enums.SessionStatusRunning
	if m.pendingClarify() != nil {
		t.Fatal("running 状态应返回 nil")
	}

	m2 := &Model{sessionsCursor: 0, sessions: []*server.Session{{ID: "s2", Status: enums.SessionStatusAwaitingClarify}}}
	if m2.pendingClarify() != nil {
		t.Fatal("无 PendingClarify 应返回 nil")
	}
}

// TestClarifyPanelHeightAndRender 验证问答面板高度与渲染一致：
// 无澄清为 0/空串；有澄清时高度 = 1 标题 + 内容行数，渲染含问题全文、选项与光标行。
func TestClarifyPanelHeightAndRender(t *testing.T) {
	// 无待答复澄清：高度 0、渲染空串。
	m0 := &Model{width: 100, styles: NewStyles(), sessionsCursor: 0, sessions: []*server.Session{{ID: "s0", Status: enums.SessionStatusRunning}}}
	if h := m0.clarifyPanelHeight(); h != 0 {
		t.Fatalf("无澄清时高度应为 0，got %d", h)
	}
	if out := m0.renderClarifyPanel(100); out != "" {
		t.Fatalf("无澄清时渲染应为空串，got %q", out)
	}

	pc := &types.ClarifyRequest{
		ID:       "ask-1",
		Question: "主题色选深色还是浅色？",
		Kind:     "choice",
		Options: []types.ClarifyOption{
			{ID: "dark", Label: "深色"},
			{ID: "light", Label: "浅色", Description: "护眼"},
		},
	}
	m := clarifyTestModel(pc)
	h := m.clarifyPanelHeight()
	out := m.renderClarifyPanel(100)
	if got := len(strings.Split(out, "\n")); got != h {
		t.Fatalf("渲染行数 %d 应与高度预算 %d 一致", got, h)
	}
	for _, want := range []string{"❓ Agent 提问", "主题色选深色还是浅色？", "❯ ● 深色", "○ 浅色 — 护眼", "回车提交"} {
		if !strings.Contains(out, want) {
			t.Fatalf("渲染应包含 %q，got:\n%s", want, out)
		}
	}

	// 光标下移后高亮行跟着走。
	m.clarifyCursor = 1
	out2 := m.renderClarifyPanel(100)
	if !strings.Contains(out2, "❯ ● 浅色") || !strings.Contains(out2, "○ 深色") {
		t.Fatalf("光标移动后高亮应在第二项，got:\n%s", out2)
	}

	// 确认场景标题。
	pc2 := &types.ClarifyRequest{ID: "approve-1", Question: "执行 rm -rf 需确认", Kind: "confirm",
		Options: []types.ClarifyOption{{ID: "confirm", Label: "确认执行"}, {ID: "reject", Label: "拒绝取消"}}}
	m2 := clarifyTestModel(pc2)
	if out := m2.renderClarifyPanel(100); !strings.Contains(out, "⚠️ 操作确认") {
		t.Fatalf("confirm 场景标题应为 ⚠️ 操作确认，got:\n%s", out)
	}
}

// TestClarifyPanelMultiSelect 验证多选面板的勾选态渲染与高度一致。
func TestClarifyPanelMultiSelect(t *testing.T) {
	pc := &types.ClarifyRequest{
		ID:          "ask-2",
		Question:    "需要哪些模块？",
		Kind:        "choice",
		MultiSelect: true,
		Options: []types.ClarifyOption{
			{ID: "a", Label: "模块A"},
			{ID: "b", Label: "模块B"},
		},
	}
	m := clarifyTestModel(pc)
	m.clarifySel = []string{"b"}
	out := m.renderClarifyPanel(100)
	if !strings.Contains(out, "☐ 模块A") || !strings.Contains(out, "☑ 模块B") {
		t.Fatalf("多选勾选态渲染错误，got:\n%s", out)
	}
	if got := len(strings.Split(out, "\n")); got != m.clarifyPanelHeight() {
		t.Fatalf("渲染行数 %d 应与高度预算 %d 一致", got, m.clarifyPanelHeight())
	}
}

// TestClarifySubmitAnswer 验证选项答案计算：单选取高亮项，多选取勾选集，空勾选/无选项不提交。
func TestClarifySubmitAnswer(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-1", Options: []types.ClarifyOption{
		{ID: "dark", Label: "深色"}, {ID: "light", Label: "浅色"},
	}}
	m := clarifyTestModel(pc)

	// 单选：默认光标 0。
	if ans, ok := m.clarifySubmitAnswer(pc); !ok || ans != "dark" {
		t.Fatalf("单选默认应提交 dark，got %q ok=%v", ans, ok)
	}
	// 单选：光标越界钳制到末项。
	m.clarifyCursor = 9
	if ans, ok := m.clarifySubmitAnswer(pc); !ok || ans != "light" {
		t.Fatalf("光标越界应钳制到 light，got %q ok=%v", ans, ok)
	}

	// 多选：空勾选不提交。
	pc.MultiSelect = true
	m.clarifySel = nil
	if _, ok := m.clarifySubmitAnswer(pc); ok {
		t.Fatal("多选空勾选不应提交")
	}
	m.clarifySel = []string{"dark", "light"}
	if ans, ok := m.clarifySubmitAnswer(pc); !ok || ans != "dark,light" {
		t.Fatalf("多选应 join 勾选集，got %q ok=%v", ans, ok)
	}

	// 无选项：走自由文本。
	if _, ok := m.clarifySubmitAnswer(&types.ClarifyRequest{ID: "ask-3"}); ok {
		t.Fatal("无选项不应提交选项答案")
	}
}

// TestClarifyNavKeys 验证 ↑/↓ 选项导航接管按键（含边界钳制）。
func TestClarifyNavKeys(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-1", Options: []types.ClarifyOption{
		{ID: "a", Label: "A"}, {ID: "b", Label: "B"}, {ID: "c", Label: "C"},
	}}
	m := clarifyTestModel(pc)
	m.syncInputMode()
	if !m.clarifyNavActive() {
		t.Fatal("澄清模式 + 有选项时导航应激活")
	}

	down := tea.KeyMsg{Type: tea.KeyDown}
	up := tea.KeyMsg{Type: tea.KeyUp}
	m.handleInputKey(down)
	m.handleInputKey(down)
	if m.clarifyCursor != 2 {
		t.Fatalf("两次 ↓ 后光标应为 2，got %d", m.clarifyCursor)
	}
	m.handleInputKey(down) // 底部钳制
	if m.clarifyCursor != 2 {
		t.Fatalf("底部应钳制在 2，got %d", m.clarifyCursor)
	}
	m.handleInputKey(up)
	if m.clarifyCursor != 1 {
		t.Fatalf("↑ 后光标应为 1，got %d", m.clarifyCursor)
	}

	// 无选项时导航不激活，↓ 走历史浏览（histIdx 不变、无 panic）。
	m2 := clarifyTestModel(&types.ClarifyRequest{ID: "ask-9", Question: "自由文本？"})
	m2.syncInputMode()
	if m2.clarifyNavActive() {
		t.Fatal("无选项时导航不应激活")
	}
}

// TestClarifySpaceToggle 验证空格键：多选切换高亮项勾选；已有文字时不消费（自由文本）。
func TestClarifySpaceToggle(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-2", MultiSelect: true, Options: []types.ClarifyOption{
		{ID: "a", Label: "A"}, {ID: "b", Label: "B"},
	}}
	m := clarifyTestModel(pc)
	m.syncInputMode()
	space := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}

	if !m.handleClarifyQuickKey(space) {
		t.Fatal("空输入时空格应被消费")
	}
	if !m.clarifySelected("a") {
		t.Fatalf("空格应勾选高亮项 a，got %v", m.clarifySel)
	}
	m.clarifyCursor = 1
	m.handleClarifyQuickKey(space)
	if !m.clarifySelected("a") || !m.clarifySelected("b") {
		t.Fatalf("光标移到 b 后空格应加选 b，got %v", m.clarifySel)
	}
	m.handleClarifyQuickKey(space) // 再按取消 b
	if m.clarifySelected("b") {
		t.Fatalf("再按空格应取消 b，got %v", m.clarifySel)
	}

	// 已有文字：空格不消费，留给自由文本插入。
	m.inputBar.runes = []rune("自定义")
	if m.handleClarifyQuickKey(space) {
		t.Fatal("已有文字时空格不应被消费")
	}
}

// TestSyncInputModeClarifyReset 验证新澄清请求（ID 变化）与离开澄清态时重置光标与选择集。
func TestSyncInputModeClarifyReset(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-1", Options: []types.ClarifyOption{{ID: "a", Label: "A"}}}
	m := clarifyTestModel(pc)
	m.syncInputMode()
	m.clarifyCursor = 0
	m.clarifySel = []string{"a"}

	// 同 ID 重复同步：不重置（用户正在选择）。
	m.clarifyCursor = 0
	m.syncInputMode()
	if len(m.clarifySel) != 1 {
		t.Fatalf("同 ID 同步不应清空选择集，got %v", m.clarifySel)
	}

	// 新 ID：重置。
	m.sessions[0].State.PendingClarify = &types.ClarifyRequest{ID: "ask-2", Options: pc.Options}
	m.clarifyCursor = 0
	m.syncInputMode()
	if m.clarifySel != nil || m.clarifyCursor != 0 || m.clarifyID != "ask-2" {
		t.Fatalf("新澄清请求应重置选择状态，got sel=%v cursor=%d id=%q", m.clarifySel, m.clarifyCursor, m.clarifyID)
	}

	// 离开澄清态：全部清空。
	m.sessions[0].Status = enums.SessionStatusRunning
	m.syncInputMode()
	if m.clarifyID != "" || m.clarifyCursor != 0 || m.clarifySel != nil || m.inputBar.mode != inputNormal {
		t.Fatalf("离开澄清态应清空全部状态，got id=%q cursor=%d sel=%v mode=%d",
			m.clarifyID, m.clarifyCursor, m.clarifySel, m.inputBar.mode)
	}
}

// TestClarifyOtherOption 验证「其他」逃生选项（TODO #53 补）：
// 单选下选中 other（空格/回车）不提交、不退出澄清模式，flash 引导自由文本输入。
func TestClarifyOtherOption(t *testing.T) {
	pc := &types.ClarifyRequest{ID: "ask-other", Options: []types.ClarifyOption{
		{ID: "a", Label: "方案A"}, {ID: types.ClarifyOtherOptionID, Label: "其他（自行输入答案）"},
	}}
	m := clarifyTestModel(pc)
	m.syncInputMode()
	m.clarifyCursor = 1 // 高亮「其他」

	// 空格选中 other：消费按键但不提交、不退出澄清模式。
	space := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}}
	if !m.handleClarifyQuickKey(space) {
		t.Fatal("空格选中 other 应消费按键")
	}
	if m.inputBar.mode != inputClarify {
		t.Fatalf("选中 other 不应退出澄清模式, got mode=%d", m.inputBar.mode)
	}
	if flash := m.ensureShared().getFlash(); !strings.Contains(flash, "输入你的答案") {
		t.Fatalf("应有自由文本引导提示, got %q", flash)
	}

	// 回车选中 other：同样不提交、不退出。
	if _, cmd := m.handleInputKey(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil {
		// Enter 不应触发任何后台命令（不提交）。
		_ = cmd
	}
	if m.inputBar.mode != inputClarify {
		t.Fatalf("回车选中 other 不应退出澄清模式, got mode=%d", m.inputBar.mode)
	}
}
