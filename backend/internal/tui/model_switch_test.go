package tui

// model_switch_test.go 覆盖模型切换弹窗（overlayModel 四段式）：
// s 键 / /model 命令打开 → stage0 选角色（←/→ 循环）→ stage1 选模型（末项=新增表单）
// → stage2 选思考档 → 异步切换结果回填（顶栏模型名/flash）；
// Esc 逐级回退、切换失败 fail-closed 提示、新增模型落盘回填。

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// modelManagerStubTUI 复用 mockAgentForPlan 提供 agent.Agent 表面，追加 ModelManager 能力。
type modelManagerStubTUI struct {
	*mockAgentForPlan
	models    []agent.ModelEntryView
	roles     []agent.RoleModelStatus
	switched  []string
	switchErr error
	added     []types.ModelEntry
	addErr    error
}

func (s *modelManagerStubTUI) ListModels(ctx context.Context) (*agent.ModelCatalog, error) {
	return &agent.ModelCatalog{Models: s.models, Roles: s.roles}, nil
}

func (s *modelManagerStubTUI) SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	if s.switchErr != nil {
		return types.AgentModelConfig{}, s.switchErr
	}
	s.switched = append(s.switched, roleID+"→"+modelID+"→"+thinking)
	return types.AgentModelConfig{Provider: "openai", Model: "glm-5.3-flash", Thinking: thinking}, nil
}

func (s *modelManagerStubTUI) AddModelEntry(entry types.ModelEntry) error {
	if s.addErr != nil {
		return s.addErr
	}
	if entry.ID == "" {
		entry.ID = entry.Model // 生产端由 factory slug 生成；桩同样补齐
	}
	s.added = append(s.added, entry)
	s.models = append(s.models, agent.ModelEntryView{ID: entry.ID, Provider: entry.Provider, Model: entry.Model})
	return nil
}

func newModelSwitchTestModel(t *testing.T, stub *modelManagerStubTUI) *Model {
	t.Helper()
	if stub.mockAgentForPlan == nil {
		stub.mockAgentForPlan = &mockAgentForPlan{}
	}
	if stub.models == nil {
		stub.models = []agent.ModelEntryView{{ID: "glm-flash", Provider: "openai", Model: "glm-5.3-flash"}}
	}
	if stub.roles == nil {
		stub.roles = []agent.RoleModelStatus{
			{RoleID: "meta", Provider: "openai", Model: "meta-old"},
			{RoleID: "code_assistant", Provider: "openai", Model: "glm-5.3-flash", ModelID: "glm-flash", Bound: true},
		}
	}
	return NewModel(stub, nil, "http://127.0.0.1:1", "meta-old")
}

func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func enterKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

func escKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEscape} }

func leftKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyLeft} }

func rightKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRight} }

// TestModelPopupOpenSelectSwitch 全流程：打开 → 选角色 → 选模型 → 选思考档 → 切换成功。
func TestModelPopupOpenSelectSwitch(t *testing.T) {
	stub := &modelManagerStubTUI{}
	m := newModelSwitchTestModel(t, stub)

	// s 键打开（NewModel 默认焦点在输入栏，先切回对话区让全局键生效）
	m.focus = panelChat
	next, _ := m.handleKey(keyMsg("s"))
	m = next.(*Model)
	if m.overlayPanel.mode != overlayModel || m.modelStage != 0 {
		t.Fatalf("popup not open: mode=%d stage=%d", m.overlayPanel.mode, m.modelStage)
	}
	if len(m.overlayPanel.lines) != 2 || !strings.Contains(m.overlayPanel.lines[0], "meta") {
		t.Fatalf("role lines = %v", m.overlayPanel.lines)
	}
	if !strings.Contains(m.overlayPanel.lines[1], "[bound: glm-flash]") {
		t.Fatalf("bound 标记缺失: %v", m.overlayPanel.lines)
	}

	// 左右键循环移光标（stage0）。
	next, _ = m.handleKey(rightKey())
	m = next.(*Model)
	if m.overlayPanel.cursor != 1 {
		t.Fatalf("right 后 cursor = %d, want 1", m.overlayPanel.cursor)
	}
	next, _ = m.handleKey(leftKey())
	m = next.(*Model)
	if m.overlayPanel.cursor != 0 {
		t.Fatalf("left 后 cursor = %d, want 0", m.overlayPanel.cursor)
	}

	// Enter 选 meta → 进模型列表（提示行 + 模型条目 + 新增入口）。
	next, cmd := m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 1 || m.modelSelRole != "meta" {
		t.Fatalf("stage=%d selRole=%q", m.modelStage, m.modelSelRole)
	}
	if cmd != nil {
		t.Fatal("stage0 Enter 不应发起切换")
	}
	if len(m.overlayPanel.lines) != 3 || !strings.Contains(m.overlayPanel.lines[1], "glm-5.3-flash") ||
		!strings.Contains(m.overlayPanel.lines[2], "新增模型") {
		t.Fatalf("model lines = %v", m.overlayPanel.lines)
	}

	// 光标移到模型行（index1）后 Enter → 进思考档列表（提示行 + 5 档位）。
	m.overlayPanel.cursor = 1
	next, cmd = m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 2 || m.modelSelModel.ID != "glm-flash" {
		t.Fatalf("stage=%d selModel=%+v", m.modelStage, m.modelSelModel)
	}
	if cmd != nil {
		t.Fatal("stage1 Enter 不应发起切换")
	}
	if len(m.overlayPanel.lines) != 6 || !strings.Contains(m.overlayPanel.lines[0], "glm-flash") {
		t.Fatalf("thinking lines = %v", m.overlayPanel.lines)
	}

	// 光标在首档（跟随角色默认=空串）后 Enter → 异步切换 cmd。
	m.overlayPanel.cursor = 1 // 提示行后第 1 项
	next, cmd = m.handleKey(enterKey())
	m = next.(*Model)
	if cmd == nil {
		t.Fatal("stage2 Enter 应返回切换 cmd")
	}
	if !m.modelSwitching {
		t.Fatal("switching 标记未置位")
	}

	// 执行 cmd 拿到结果消息，回灌 Update（Update 值接收者，取回值副本）。
	msg := cmd()
	done, ok := msg.(modelSwitchDoneMsg)
	if !ok || done.err != nil {
		t.Fatalf("cmd msg = %+v", msg)
	}
	next, _ = m.Update(done)
	nm := next.(Model)
	m = &nm
	if m.modelSwitching {
		t.Fatal("switching 未复位")
	}
	if m.modelName != "glm-5.3-flash" {
		t.Fatalf("顶栏模型名 = %q, want glm-5.3-flash", m.modelName)
	}
	if len(stub.switched) != 1 || stub.switched[0] != "meta→glm-flash→" {
		t.Fatalf("switched = %v", stub.switched)
	}
	if !strings.Contains(m.shared.getFlash(), "switched meta") {
		t.Fatalf("flash = %q", m.shared.getFlash())
	}
	// 成功后回角色列表便于连续切换。
	if m.modelStage != 0 {
		t.Fatalf("成功后应回 stage0, got %d", m.modelStage)
	}
}

// TestModelPopupThinkingOverride 思考档覆盖：选 low 切换，stub 收到 thinking=low。
func TestModelPopupThinkingOverride(t *testing.T) {
	stub := &modelManagerStubTUI{}
	m := newModelSwitchTestModel(t, stub)
	m.focus = panelChat
	next, _ := m.handleKey(keyMsg("s"))
	m = next.(*Model)
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	m.overlayPanel.cursor = 1 // 模型行
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	m.overlayPanel.cursor = 3 // hint 后第 3 项 = low
	next, cmd := m.handleKey(enterKey())
	if cmd == nil {
		t.Fatal("应返回切换 cmd")
	}
	done := cmd().(modelSwitchDoneMsg)
	if done.thinking != "low" {
		t.Fatalf("thinking = %q, want low", done.thinking)
	}
	next, _ = m.Update(done)
	nm := next.(Model)
	m = &nm
	if !strings.Contains(m.shared.getFlash(), "thinking: low") {
		t.Fatalf("flash = %q", m.shared.getFlash())
	}
	if len(stub.switched) != 1 || stub.switched[0] != "meta→glm-flash→low" {
		t.Fatalf("switched = %v", stub.switched)
	}
}

// TestModelPopupAddModelForm stage1 末项进新增表单：填写 → 提交 → 回模型列表含新条目。
func TestModelPopupAddModelForm(t *testing.T) {
	stub := &modelManagerStubTUI{}
	m := newModelSwitchTestModel(t, stub)
	m.focus = panelChat
	next, _ := m.handleKey(keyMsg("s"))
	m = next.(*Model)
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)

	// 光标到末项（新增模型入口）后 Enter → stage3 表单。
	m.overlayPanel.cursor = len(m.overlayPanel.lines) - 1
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 3 {
		t.Fatalf("stage = %d, want 3", m.modelStage)
	}

	// 填 4 字段（provider/model/api_key/base_url）。
	m.modelForm[0].SetValue("openai-chat")
	m.modelForm[1].SetValue("kimi-k3")
	m.modelForm[2].SetValue("sk-1")
	m.modelForm[3].SetValue("http://b")

	// Enter 逐字段推进（3 次到末字段），第 4 次提交。
	for i := 0; i < 3; i++ {
		next, _ = m.handleKey(enterKey())
		m = next.(*Model)
	}
	if m.modelFormFocus != 3 {
		t.Fatalf("form focus = %d, want 3", m.modelFormFocus)
	}
	next, cmd := m.handleKey(enterKey())
	m = next.(*Model)
	if cmd == nil || !m.modelFormBusy {
		t.Fatal("末字段 Enter 应提交新增")
	}
	msg := cmd()
	added, ok := msg.(modelAddedMsg)
	if !ok || added.err != nil {
		t.Fatalf("cmd msg = %+v", msg)
	}
	if len(stub.added) != 1 || stub.added[0].Model != "kimi-k3" || stub.added[0].APIKey != "sk-1" {
		t.Fatalf("added = %+v", stub.added)
	}
	next, _ = m.Update(added)
	nm := next.(Model)
	m = &nm
	if m.modelFormBusy || m.modelStage != 1 {
		t.Fatalf("提交后应回 stage1: busy=%v stage=%d", m.modelFormBusy, m.modelStage)
	}
	if !strings.Contains(strings.Join(m.overlayPanel.lines, "\n"), "kimi-k3") {
		t.Fatalf("模型列表应含新条目: %v", m.overlayPanel.lines)
	}
	if !strings.Contains(m.shared.getFlash(), "models.json") {
		t.Fatalf("flash = %q", m.shared.getFlash())
	}

	// Esc 回角色列表，再 Esc 关闭。
	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.modelStage != 0 || m.overlayPanel.mode != overlayModel {
		t.Fatalf("esc 应退回 stage0: stage=%d mode=%d", m.modelStage, m.overlayPanel.mode)
	}
	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.overlayPanel.mode != overlayNone {
		t.Fatalf("二次 esc 应关闭，mode=%d", m.overlayPanel.mode)
	}
}

// TestModelPopupEscBackAndClose stage1 Esc 退回角色列表，stage2 Esc 退回模型列表。
func TestModelPopupEscBackAndClose(t *testing.T) {
	stub := &modelManagerStubTUI{}
	m := newModelSwitchTestModel(t, stub)
	m.focus = panelChat
	next, _ := m.handleKey(keyMsg("s"))
	m = next.(*Model)
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 1 {
		t.Fatalf("stage = %d", m.modelStage)
	}

	// stage1 → stage2 → Esc 回 stage1。
	m.overlayPanel.cursor = 1
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 2 {
		t.Fatalf("stage = %d, want 2", m.modelStage)
	}
	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.modelStage != 1 || m.overlayPanel.mode != overlayModel {
		t.Fatalf("esc 应退回 stage1: stage=%d mode=%d", m.modelStage, m.overlayPanel.mode)
	}
	if !strings.Contains(strings.Join(m.overlayPanel.lines, "\n"), "glm-5.3-flash") {
		t.Fatalf("退回应恢复模型列表: %v", m.overlayPanel.lines)
	}

	// stage1 → stage0。
	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.modelStage != 0 || m.overlayPanel.mode != overlayModel {
		t.Fatalf("esc 应退回 stage0: stage=%d mode=%d", m.modelStage, m.overlayPanel.mode)
	}
	if !strings.Contains(strings.Join(m.overlayPanel.lines, "\n"), "code_assistant") {
		t.Fatalf("退回应恢复角色列表: %v", m.overlayPanel.lines)
	}

	// stage0 Esc 关闭。
	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.overlayPanel.mode != overlayNone {
		t.Fatalf("三次 esc 应关闭，mode=%d", m.overlayPanel.mode)
	}
}

// TestModelPopupSwitchFail 切换失败：flash 错误、顶栏模型名不变、fail-closed。
func TestModelPopupSwitchFail(t *testing.T) {
	stub := &modelManagerStubTUI{switchErr: errors.New("probe failed")}
	m := newModelSwitchTestModel(t, stub)
	m.focus = panelChat
	next, _ := m.handleKey(keyMsg("s"))
	m = next.(*Model)
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	m.overlayPanel.cursor = 1
	next, _ = m.handleKey(enterKey())
	m = next.(*Model)
	m.overlayPanel.cursor = 1 // 提示行后第 1 项
	next, cmd := m.handleKey(enterKey())
	if cmd == nil {
		t.Fatal("应返回切换 cmd")
	}
	msg := cmd().(modelSwitchDoneMsg)
	next, _ = m.Update(msg)
	nm2 := next.(Model)
	m = &nm2
	if m.modelName != "meta-old" {
		t.Fatalf("失败后模型名应保持 meta-old，got %q", m.modelName)
	}
	if !strings.Contains(m.shared.getFlash(), "switch failed") {
		t.Fatalf("flash = %q", m.shared.getFlash())
	}
	if m.modelSwitching {
		t.Fatal("switching 未复位")
	}
}

// TestModelCommandOpensPopup 输入栏 /model 命令打开弹窗；非 ModelManager 提示不可用。
func TestModelCommandOpensPopup(t *testing.T) {
	stub := &modelManagerStubTUI{}
	m := newModelSwitchTestModel(t, stub)
	m.submitInput("/model")
	if m.overlayPanel.mode != overlayModel {
		t.Fatalf("/model 未打开弹窗，mode=%d", m.overlayPanel.mode)
	}

	// 无 ModelManager 能力：flash 提示。
	bare := NewModel(&mockAgentForPlan{}, nil, "http://127.0.0.1:1", "test")
	bare.submitInput("/model")
	if bare.overlayPanel.mode == overlayModel {
		t.Fatal("无 ModelManager 不应打开弹窗")
	}
	if !strings.Contains(bare.shared.getFlash(), "not available") {
		t.Fatalf("flash = %q", bare.shared.getFlash())
	}
}
