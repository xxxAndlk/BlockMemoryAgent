package tui

// model_switch_test.go 覆盖模型切换弹窗（overlayModel 两段式）：
// s 键 / /model 命令打开 → 选角色 → 选预设 → 异步切换结果回填（顶栏模型名/flash），
// 以及 Esc 逐级退、切换失败 fail-closed 提示。

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
	presets   []types.ModelPreset
	roles     []agent.RoleModelStatus
	switched  []string
	switchErr error
}

func (s *modelManagerStubTUI) ListModels(ctx context.Context) (*agent.ModelCatalog, error) {
	return &agent.ModelCatalog{Presets: s.presets, Roles: s.roles}, nil
}

func (s *modelManagerStubTUI) SwitchModel(ctx context.Context, roleID, presetID string) (types.AgentModelConfig, error) {
	if s.switchErr != nil {
		return types.AgentModelConfig{}, s.switchErr
	}
	s.switched = append(s.switched, roleID+"→"+presetID)
	return types.AgentModelConfig{Provider: "openai", Model: "glm-5.3-flash"}, nil
}

func newModelSwitchTestModel(t *testing.T, stub *modelManagerStubTUI) *Model {
	t.Helper()
	if stub.mockAgentForPlan == nil {
		stub.mockAgentForPlan = &mockAgentForPlan{}
	}
	if stub.presets == nil {
		stub.presets = []types.ModelPreset{{ID: "glm-flash", Name: "GLM Flash", Provider: "openai", Model: "glm-5.3-flash"}}
	}
	if stub.roles == nil {
		stub.roles = []agent.RoleModelStatus{
			{RoleID: "meta", Provider: "openai", Model: "meta-old"},
			{RoleID: "code_assistant", Provider: "openai", Model: "code-old", Overridden: true, PresetID: "glm-flash"},
		}
	}
	return NewModel(stub, nil, "http://127.0.0.1:1", "meta-old")
}

func keyMsg(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func enterKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

func escKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEscape} }

// TestModelPopupOpenSelectSwitch 全流程：打开 → 选角色 → 选预设 → 切换成功。
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
	if !strings.Contains(m.overlayPanel.lines[1], "[override: glm-flash]") {
		t.Fatalf("override 标记缺失: %v", m.overlayPanel.lines)
	}

	// Enter 选 meta → 进 preset 列表
	next, cmd := m.handleKey(enterKey())
	m = next.(*Model)
	if m.modelStage != 1 || m.modelSelRole != "meta" {
		t.Fatalf("stage=%d selRole=%q", m.modelStage, m.modelSelRole)
	}
	if cmd != nil {
		t.Fatal("stage0 Enter 不应发起切换")
	}
	if len(m.overlayPanel.lines) != 2 || !strings.Contains(m.overlayPanel.lines[1], "glm-5.3-flash") {
		t.Fatalf("preset lines = %v", m.overlayPanel.lines)
	}

	// 光标移到预设行（index1）后 Enter → 异步切换 cmd
	m.overlayPanel.cursor = 1
	next, cmd = m.handleKey(enterKey())
	m = next.(*Model)
	if cmd == nil {
		t.Fatal("stage1 Enter 应返回切换 cmd")
	}
	if !m.modelSwitching {
		t.Fatal("switching 标记未置位")
	}

	// 执行 cmd 拿到结果消息，回灌 Update（Update 值接收者，取回值副本）
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
	if len(stub.switched) != 1 || stub.switched[0] != "meta→glm-flash" {
		t.Fatalf("switched = %v", stub.switched)
	}
	if !strings.Contains(m.shared.getFlash(), "switched meta") {
		t.Fatalf("flash = %q", m.shared.getFlash())
	}
}

// TestModelPopupEscBackAndClose stage1 Esc 退回角色列表，再 Esc 关闭。
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

	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.modelStage != 0 || m.overlayPanel.mode != overlayModel {
		t.Fatalf("esc 应退回 stage0: stage=%d mode=%d", m.modelStage, m.overlayPanel.mode)
	}
	if !strings.Contains(strings.Join(m.overlayPanel.lines, "\n"), "code_assistant") {
		t.Fatalf("退回应恢复角色列表: %v", m.overlayPanel.lines)
	}

	next, _ = m.handleKey(escKey())
	m = next.(*Model)
	if m.overlayPanel.mode != overlayNone {
		t.Fatalf("二次 esc 应关闭，mode=%d", m.overlayPanel.mode)
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
	next, cmd := m.handleKey(enterKey())
	_ = next
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

	// 无 ModelManager 能力：flash 提示
	bare := NewModel(&mockAgentForPlan{}, nil, "http://127.0.0.1:1", "test")
	bare.submitInput("/model")
	if bare.overlayPanel.mode == overlayModel {
		t.Fatal("无 ModelManager 不应打开弹窗")
	}
	if !strings.Contains(bare.shared.getFlash(), "not available") {
		t.Fatalf("flash = %q", bare.shared.getFlash())
	}
}
