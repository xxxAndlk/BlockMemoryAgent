package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
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
	hint := styles.Dim.Render("  [Esc close · ↑/↓ select · enter detail]")
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

// modelSwitchDoneMsg 模型切换异步完成消息（SwitchModel 含 60s 级连通性探测，
// 必须 tea.Cmd 异步执行，禁止阻塞 Update）。
type modelSwitchDoneMsg struct {
	role     string
	modelID  string
	thinking string
	cfg      types.AgentModelConfig
	err      error
}

// modelAddedMsg 新增模型条目异步完成消息（含刷新后的目录，成功时非 nil）。
type modelAddedMsg struct {
	catalog *agent.ModelCatalog
	err     error
}

// modelThinkingOptions 思考档位选项：值 + 展示名（空值 = 跟随角色配置）。
var modelThinkingOptions = []struct {
	value string
	label string
}{
	{"", "跟随角色默认"},
	{"off", "off 关闭思考"},
	{"low", "low 轻量思考"},
	{"medium", "medium 中等思考"},
	{"high", "high 深度思考"},
}

// toggleModelPopup 切换模型弹窗（多段式）：stage 0 选角色（←/→ 循环）→
// stage 1 选模型（末项=新增）→ stage 2 选思考档 / stage 3 新增模型表单。
// 依赖后端 agent.ModelManager 能力（ReactService 实现）；不可用时提示。
func (m *Model) toggleModelPopup() {
	// 已打开则关闭。
	if m.overlayPanel.mode == overlayModel {
		m.overlayPanel.close()
		return
	}
	mgr, ok := m.agent.(agent.ModelManager)
	if !ok {
		m.flashMsg("model switching not available")
		return
	}
	catalog, err := mgr.ListModels(context.Background())
	if err != nil || catalog == nil || len(catalog.Roles) == 0 {
		m.flashMsg("model catalog unavailable")
		return
	}
	m.modelRoles = catalog.Roles
	m.modelModels = catalog.Models
	m.modelStage = 0
	m.overlayPanel.mode = overlayModel
	m.overlayPanel.kind = overlayModel
	m.overlayPanel.title = "Switch Model · Select Role (←/→ or ↑/↓)"
	m.overlayPanel.lines = m.buildModelRoleLines()
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(m.overlayPanel.lines)-1)
}

// cycleModelCursor 左右键循环移动光标（stage0 角色列表 / stage2 思考档列表）。
func (m *Model) cycleModelCursor(right bool) {
	n := len(m.overlayPanel.lines)
	if n <= 0 {
		return
	}
	if right {
		m.overlayPanel.cursor = (m.overlayPanel.cursor + 1) % n
	} else {
		m.overlayPanel.cursor = (m.overlayPanel.cursor - 1 + n) % n
	}
}

// modelModelBack 模型弹窗逐段回退：3/2→1（模型列表）、1→0（角色列表）。
func (m *Model) modelModelBack() {
	switch m.modelStage {
	case 3, 2:
		m.modelStage = 1
		m.overlayPanel.title = "Switch Model · Select Model"
		m.overlayPanel.lines = m.buildModelModelLines()
	case 1:
		m.modelStage = 0
		m.overlayPanel.title = "Switch Model · Select Role (←/→ or ↑/↓)"
		m.overlayPanel.lines = m.buildModelRoleLines()
	}
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(m.overlayPanel.lines)-1)
}

// buildModelRoleLines 弹窗 stage 0 内容：可切换角色 + 当前模型（含绑定标记）。
func (m *Model) buildModelRoleLines() []string {
	lines := make([]string, 0, len(m.modelRoles))
	for _, r := range m.modelRoles {
		line := fmt.Sprintf("%-16s %s/%s", r.RoleID, r.Provider, r.Model)
		if r.Bound {
			line += fmt.Sprintf("  [bound: %s]", r.ModelID)
		}
		lines = append(lines, line)
	}
	return lines
}

// buildModelModelLines 弹窗 stage 1 内容：可选模型清单（末项=新增模型表单入口）。
func (m *Model) buildModelModelLines() []string {
	lines := make([]string, 0, len(m.modelModels)+2)
	thinking := ""
	for _, r := range m.modelRoles {
		if r.RoleID == m.modelSelRole {
			thinking = r.Thinking
			break
		}
	}
	lines = append(lines, fmt.Sprintf("target role: %s  thinking=%s  (enter=pick, esc=back)",
		m.modelSelRole, thinkingDisplay(thinking)))
	for _, e := range m.modelModels {
		extra := ""
		if e.BaseURL != "" {
			extra = "  " + e.BaseURL
		}
		marker := ""
		if st, ok := m.modelRoleStatus(m.modelSelRole); ok && st.ModelID == e.ID {
			marker = "  ← 当前"
		}
		lines = append(lines, fmt.Sprintf("%-22s %-10s %s%s%s", e.ID, e.Provider, e.Model, extra, marker))
	}
	lines = append(lines, "＋ 新增模型…  (provider/model/api_key/base_url)")
	return lines
}

// modelRoleStatus 查角色状态副本。
func (m *Model) modelRoleStatus(roleID string) (agent.RoleModelStatus, bool) {
	for _, r := range m.modelRoles {
		if r.RoleID == roleID {
			return r, true
		}
	}
	return agent.RoleModelStatus{}, false
}

// thinkingDisplay 思考档位展示名（空值 → 端点默认）。
func thinkingDisplay(t string) string {
	if t == "" {
		return "(default)"
	}
	return t
}

// buildModelThinkingLines 弹窗 stage 2 内容：思考档位选项。
func (m *Model) buildModelThinkingLines() []string {
	lines := make([]string, 0, len(modelThinkingOptions)+1)
	current := ""
	if st, ok := m.modelRoleStatus(m.modelSelRole); ok {
		current = st.Thinking
	}
	lines = append(lines, fmt.Sprintf("target: %s → %s  (enter=switch, esc=back)",
		m.modelSelRole, m.modelSelModel.ID))
	for _, opt := range modelThinkingOptions {
		marker := ""
		if opt.value == current {
			marker = "  ← 当前"
		}
		lines = append(lines, opt.label+marker)
	}
	return lines
}

// initModelForm 初始化新增模型表单（stage3）：4 个输入框，焦点在首个。
func (m *Model) initModelForm() {
	placeholders := [4]string{"provider (openai-chat/anthropic/ollama…)", "model 名称", "api_key", "base_url（可空）"}
	for i := range m.modelForm {
		in := textinput.New()
		in.Placeholder = placeholders[i]
		in.CharLimit = 512
		in.Width = 56
		m.modelForm[i] = in
	}
	m.modelFormFocus = 0
	m.modelForm[0].Focus()
}

// modelFormValues 读取表单当前值。
func (m *Model) modelFormValues() [4]string {
	var out [4]string
	for i := range m.modelForm {
		out[i] = strings.TrimSpace(m.modelForm[i].Value())
	}
	return out
}

// handleModelFormKey stage3 表单按键：Tab/Shift+Tab 或 Enter 切换焦点，
// 末字段 Enter 提交，Esc 返回模型列表；其余按键交给焦点输入框。
func (m *Model) handleModelFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.modelModelBack()
		return m, nil
	case "tab", "shift+tab", "enter":
		if msg.String() == "enter" && m.modelFormFocus < len(m.modelForm)-1 {
			m.modelFormFocus++
		} else if msg.String() == "shift+tab" {
			if m.modelFormFocus > 0 {
				m.modelFormFocus--
			}
		} else if msg.String() == "tab" && m.modelFormFocus < len(m.modelForm)-1 {
			m.modelFormFocus++
		} else if msg.String() == "enter" {
			// 末字段 Enter：提交新增。
			return m, m.submitModelForm()
		}
		for i := range m.modelForm {
			m.modelForm[i].Blur()
		}
		m.modelForm[m.modelFormFocus].Focus()
		return m, textinput.Blink
	default:
		var cmd tea.Cmd
		m.modelForm[m.modelFormFocus], cmd = m.modelForm[m.modelFormFocus].Update(msg)
		return m, cmd
	}
}

// submitModelForm 校验并发起异步新增（AddModelEntry 落盘 models.json）。
func (m *Model) submitModelForm() tea.Cmd {
	if m.modelFormBusy {
		return nil
	}
	v := m.modelFormValues()
	if v[0] == "" || v[1] == "" {
		m.flashMsg("provider 与 model 必填")
		return nil
	}
	mgr, ok := m.agent.(agent.ModelManager)
	if !ok {
		m.flashMsg("model switching not available")
		return nil
	}
	m.modelFormBusy = true
	return func() tea.Msg {
		entry := types.ModelEntry{Provider: v[0], Model: v[1], APIKey: v[2], BaseURL: v[3]}
		err := mgr.AddModelEntry(entry)
		var catalog *agent.ModelCatalog
		if err == nil {
			catalog, err = mgr.ListModels(context.Background())
		}
		return modelAddedMsg{catalog: catalog, err: err}
	}
}

// applyModelAdded 处理新增结果：成功刷新模型列表并跳回 stage1；
// 失败 flash 错误并停留在表单（输入保留便于修正）。
func (m *Model) applyModelAdded(msg modelAddedMsg) {
	m.modelFormBusy = false
	if msg.err != nil {
		m.flashMsg("add model failed: " + msg.err.Error())
		return
	}
	if msg.catalog != nil {
		m.modelModels = msg.catalog.Models
		m.modelRoles = msg.catalog.Roles
	}
	m.flashMsg("模型已写入 models.json")
	m.modelStage = 1
	m.overlayPanel.title = "Switch Model · Select Model"
	m.overlayPanel.lines = m.buildModelModelLines()
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(m.overlayPanel.lines)-1)
}

// handleModelEnter 模型弹窗 Enter：stage0 记录角色进模型列表；stage1 选模型
// （末项=新增表单）；stage2 发起异步切换（探测最长 60s，期间 Enter 忽略）。
func (m *Model) handleModelEnter() tea.Cmd {
	switch m.modelStage {
	case 0:
		if m.overlayPanel.cursor < 0 || m.overlayPanel.cursor >= len(m.modelRoles) {
			return nil
		}
		m.modelSelRole = m.modelRoles[m.overlayPanel.cursor].RoleID
		m.modelStage = 1
		m.overlayPanel.title = "Switch Model · Select Model"
		m.overlayPanel.lines = m.buildModelModelLines()
		m.overlayPanel.cursor = 0
		return nil
	case 1:
		// 减去首行提示行得到模型下标；末项是"新增模型"入口。
		idx := m.overlayPanel.cursor - 1
		if idx == len(m.modelModels) {
			m.modelStage = 3
			m.overlayPanel.title = "Add Model (Tab 切换字段, Enter 下一项, Esc 返回)"
			m.initModelForm()
			m.overlayPanel.lines = []string{
				"新增模型将写入 config/models.json（免重启热更新）:",
				"",
			}
			for _, in := range m.modelForm {
				m.overlayPanel.lines = append(m.overlayPanel.lines, in.View())
			}
			return textinput.Blink
		}
		if idx < 0 || idx >= len(m.modelModels) {
			return nil
		}
		m.modelSelModel = m.modelModels[idx]
		m.modelStage = 2
		m.overlayPanel.title = "Switch Model · Thinking 强度"
		m.overlayPanel.lines = m.buildModelThinkingLines()
		m.overlayPanel.cursor = 0
		return nil
	case 2:
		if m.modelSwitching {
			m.flashMsg("switching in progress…")
			return nil
		}
		// 减去首行提示行得到思考档下标。
		idx := m.overlayPanel.cursor - 1
		if idx < 0 || idx >= len(modelThinkingOptions) {
			return nil
		}
		thinking := modelThinkingOptions[idx].value
		m.modelSwitching = true
		m.flashMsg(fmt.Sprintf("probing %s… (up to 60s)", m.modelSelModel.ID))
		mgr, ok := m.agent.(agent.ModelManager)
		if !ok {
			m.modelSwitching = false
			return nil
		}
		role, modelID := m.modelSelRole, m.modelSelModel.ID
		return func() tea.Msg {
			cfg, err := mgr.SwitchModel(context.Background(), role, modelID, thinking)
			return modelSwitchDoneMsg{role: role, modelID: modelID, thinking: thinking, cfg: cfg, err: err}
		}
	}
	return nil
}

// applyModelSwitchDone 处理切换结果：成功回角色列表便于连续切换并刷新；
// 失败 flash 错误（fail-closed，后端状态未变）。
func (m *Model) applyModelSwitchDone(msg modelSwitchDoneMsg) {
	m.modelSwitching = false
	if msg.err != nil {
		m.flashMsg("switch failed: " + msg.err.Error())
		return
	}
	if msg.role == "meta" {
		m.modelName = msg.cfg.Model // 顶栏展示的是 meta 模型
	}
	if mgr, ok := m.agent.(agent.ModelManager); ok {
		if catalog, err := mgr.ListModels(context.Background()); err == nil && catalog != nil {
			m.modelRoles = catalog.Roles
			m.modelModels = catalog.Models
		}
	}
	m.modelStage = 0
	m.overlayPanel.title = "Switch Model · Select Role (←/→ or ↑/↓)"
	m.overlayPanel.lines = m.buildModelRoleLines()
	m.overlayPanel.cursor = clamp(m.overlayPanel.cursor, 0, len(m.overlayPanel.lines)-1)
	thinking := msg.thinking
	if thinking == "" {
		thinking = "默认"
	}
	m.flashMsg(fmt.Sprintf("switched %s → %s (thinking: %s)", msg.role, msg.cfg.Model, thinking))
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
