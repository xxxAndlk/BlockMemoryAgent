package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/go-kratos/blades"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// mockProviderForTUI 是一个 blades 模型提供者，始终返回固定纯文本响应，
// 让 ReAct 会话无需真实 API 调用即可结束。
type mockProviderForTUI struct {
	response string
}

// Generate 返回固定的助手回复。
func (p *mockProviderForTUI) Generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	return &blades.ModelResponse{Message: blades.AssistantMessage(p.response)}, nil
}

// testAgent 构造一个基于 ReAct 的 agent.Agent facade 用于测试。
func testAgent(t *testing.T, summary string) agent.Agent {
	t.Helper()
	cfg := minimalRoleConfigForRender()
	roleRegistry := role.NewRegistry(cfg)
	toolRegistry := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	s := agent.NewReactService(roleRegistry, nil, toolRegistry, nil, agent.NopMemoryPipeline{}, nil)
	s.SetModelProvider(&mockProviderForTUI{response: summary})
	return s
}

// mockAgentForPlan 是一个最小化的 agent.Agent 实现，用于 TestPlanPanelReflectsAgentStatuses，
// 避免测试依赖 *runtime.Runtime。
type mockAgentForPlan struct {
	sessionID  string
	goal       string
	boardSnap  board.Snapshot
	agentInsts []agent.AgentInstance
}

// CreateSession 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) {
	return nil, nil
}

// ResumeSession 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) ResumeSession(ctx context.Context, sessionID string, req agent.ResumeRequest) (*agent.Session, error) {
	return nil, nil
}

// Send 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) Send(ctx context.Context, sessionID string, msg agent.Message) error {
	return nil
}

// Stream 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) Stream(ctx context.Context, sessionID string) (<-chan agent.Event, error) {
	return nil, nil
}

// Query 返回预设的看板快照（当查询类型为 Board 时）。
func (m *mockAgentForPlan) Query(ctx context.Context, sessionID string, q agent.Query) (agent.Result, error) {
	if sessionID == m.sessionID && q.Kind == agent.QueryKindBoard {
		return agent.Result{Data: m.boardSnap}, nil
	}
	return agent.Result{}, nil
}

// Control 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error {
	return nil
}

// List 返回一个仅包含当前会话的列表。
func (m *mockAgentForPlan) List(ctx context.Context, filter agent.Filter) ([]*agent.Session, error) {
	return []*agent.Session{{ID: m.sessionID, Goal: m.goal}}, nil
}

// Get 返回当前会话。
func (m *mockAgentForPlan) Get(ctx context.Context, sessionID string) (*agent.Session, error) {
	if sessionID == m.sessionID {
		return &agent.Session{ID: m.sessionID, Goal: m.goal}, nil
	}
	return nil, fmt.Errorf("not found")
}

// ListAgents 返回预设的 Agent 实例列表。
func (m *mockAgentForPlan) ListAgents(ctx context.Context, sessionID string) ([]agent.AgentInstance, error) {
	return m.agentInsts, nil
}

// Shutdown 是 mockAgentForPlan 的空实现。
func (m *mockAgentForPlan) Shutdown(ctx context.Context) error { return nil }

// SummarizeTaskTitle 是 mockAgentForPlan 的标题摘要实现，直接返回原标题。
func (m *mockAgentForPlan) SummarizeTaskTitle(ctx context.Context, title string) string { return title }

// minimalRoleConfigForRender 返回一个用于测试的最小角色配置。
func minimalRoleConfigForRender() *pkgconfig.RoleConfigFile {
	return &pkgconfig.RoleConfigFile{
		MetaAgent: pkgconfig.MetaAgentConfig{
			MaxBlocks:       4,
			SummaryInterval: 1,
			SystemPrompt:    "You are a helpful assistant.",
			ModelConfig:     types.AgentModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"},
		},
		DomainAgent: pkgconfig.DomainAgentConfig{
			ModelConfig: types.AgentModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"},
		},
	}
}

// TestFirstMessagePendingToRealSession 验证：无会话时发送首条消息后，
// 本地预展示的消息可见；待后端会话创建完成并选中后，真实会话中的同一条
// 用户消息仍然以 "You" 高亮展示。
func TestFirstMessagePendingToRealSession(t *testing.T) {
	soulPath := filepath.Join(t.TempDir(), "soul.md")
	if err := os.WriteFile(soulPath, []byte("test persona"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	rt, err := runtime.New(soulPath, skill.BuiltinPool())
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rt.SetAgentConfig(&config.AgentConfig{})

	agentSvc := testAgent(t, "收到，开始处理。")

	m := &Model{
		styles:       NewStyles(),
		chatPanel:    ChatPanel{vp: viewport.New(80, 20)},
		width:        80,
		height:       24,
		agent:        agentSvc,
		streamEvents: make(chan agent.Event, 16),
		httpAddr:     "http://127.0.0.1:1",
		flashMu:      &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")

	// 无会话时发送首条消息
	m.submitInput("hello")
	if m.chatPanel.pendingFirstMessage != "hello" {
		t.Fatalf("pendingFirstMessage 应被设置，got %q", m.chatPanel.pendingFirstMessage)
	}
	view := m.View()
	if !strings.Contains(view, "You hello") {
		t.Fatalf("本地预展示消息应可见，got:\n%s", view)
	}

	// 模拟后端会话创建完成（直接创建，跳过异步 HTTP）
	session, _ := agentSvc.CreateSession(context.Background(), agent.CreateRequest{Goal: "hello"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := agentSvc.Get(context.Background(), session.ID)
		if snap != nil && enums.SessionStatus(snap.Status) == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.pendingSelectID = session.ID

	// 触发 tick，让 selectSession 消费 pendingSelectID
	nm, _ := m.Update(tickMsg{})
	if mv, ok := nm.(Model); ok {
		m = &mv
	}

	if m.chatPanel.pendingFirstMessage != "" {
		t.Fatalf("selectSession 后 pendingFirstMessage 应被清空，got %q", m.chatPanel.pendingFirstMessage)
	}

	view = m.View()
	t.Logf("after session selected view:\n%s", view)
	if !strings.Contains(view, "You hello") {
		t.Fatalf("真实会话中的首条用户消息仍应以 'You' 高亮展示，got:\n%s", view)
	}
}

// TestFirstMessageRenderedInExistingSession 验证：已有会话中发送的首条用户消息
// 必须出现在对话区并以 "You" 高亮展示，助手回复紧随其后可见。
func TestFirstMessageRenderedInExistingSession(t *testing.T) {
	soulPath := filepath.Join(t.TempDir(), "soul.md")
	if err := os.WriteFile(soulPath, []byte("test persona"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	rt, err := runtime.New(soulPath, skill.BuiltinPool())
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rt.SetAgentConfig(&config.AgentConfig{})

	agentSvc := testAgent(t, "你好！我是你的多 Agent 编排助手 BlockMemoryAgent。")

	// 创建会话；mock provider 立即结束，不会并发修改 Messages
	session, _ := agentSvc.CreateSession(context.Background(), agent.CreateRequest{Goal: "a"})

	// 等待会话完成，确保 Messages 已追加助手总结
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := agentSvc.Get(context.Background(), session.ID)
		if snap != nil && enums.SessionStatus(snap.Status) == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:       NewStyles(),
		chatPanel:    ChatPanel{vp: viewport.New(80, 20)},
		width:        80,
		height:       24,
		agent:        agentSvc,
		streamEvents: make(chan agent.Event, 16),
		httpAddr:     "http://127.0.0.1:1",
		flashMu:      &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")
	m.refreshSessions()
	if len(m.sessions) == 0 {
		t.Fatal("expected at least one session after CreateSession")
	}
	m.selectSession(0)

	view := m.View()
	t.Logf("rendered view:\n%s", view)

	if !strings.Contains(view, "You") {
		t.Fatal("用户消息应以 'You' 高亮标签展示")
	}
	if !strings.Contains(view, "You a") {
		t.Fatalf("用户消息 'a' 应以 'You' 标签在对话区可见，got:\n%s", view)
	}
	if !strings.Contains(view, "BlockMemoryAgent") {
		t.Fatalf("助手回复应在对话区可见，got:\n%s", view)
	}
}

// TestScrollbarDragScrollsChat 验证：鼠标拖动聊天区滚动条滑块时，viewport 会跟随滚动。
func TestScrollbarDragScrollsChat(t *testing.T) {
	m := &Model{
		styles:    NewStyles(),
		chatPanel: ChatPanel{vp: viewport.New(80, 5)},
		width:     80,
		height:    12,
		flashMu:   &sync.Mutex{},
	}
	// 顶栏 1 行 + Token 栏 1 行 + 主内容区 4 行 + 输入栏 5 行（含边框） + 快捷键栏 1 行 = 12 行
	// mainContentHeight = 12 - 1（顶栏） - 1（Token 栏） - 5（输入栏） - 1（快捷键栏） = 4
	if h := m.mainContentHeight(); h != 4 {
		t.Fatalf("mainContentHeight 应为 4，got %d", h)
	}

	// 构造 20 行内容，使 viewport 可滚动。
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = strings.Repeat("x", 70)
	}
	m.chatPanel.vp.SetContent(strings.Join(lines, "\n"))
	m.chatPanel.vp.YOffset = 0

	totalLines := m.chatPanel.vp.TotalLineCount()
	viewportH := m.chatPanel.vp.VisibleLineCount()
	if totalLines <= viewportH {
		t.Fatalf("内容应超出 viewport，totalLines=%d viewportH=%d", totalLines, viewportH)
	}

	sx, sy, sw, sh := m.chatPanel.scrollbarArea(m.chatAreaWidth(), m.mainContentHeight())
	if sx < 0 || sy != 1 || sw != 1 || sh != 4 {
		t.Fatalf("scrollbarArea 异常: x=%d y=%d w=%d h=%d", sx, sy, sw, sh)
	}

	thumbStart, thumbEnd := m.chatPanel.thumbBounds(m.mainContentHeight())
	if thumbStart < 0 || thumbEnd < thumbStart {
		t.Fatalf("滑块边界异常: start=%d end=%d", thumbStart, thumbEnd)
	}
	thumbCenter := sy + (thumbStart+thumbEnd)/2

	// 在滑块上按下左键开始拖动。
	press := tea.MouseMsg{X: sx, Y: thumbCenter, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	nm, _ := m.Update(press)
	m = modelPtr(nm)
	if !m.chatPanel.scrollbarDragging {
		t.Fatal("在滑块上按下左键后应开始拖动")
	}

	initialOffset := m.chatPanel.vp.YOffset

	// 向下拖动 3 行。
	drag := tea.MouseMsg{X: sx, Y: thumbCenter + 3, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion}
	nm, _ = m.Update(drag)
	m = modelPtr(nm)
	if m.chatPanel.vp.YOffset <= initialOffset {
		t.Fatalf("向下拖动后 YOffset 应增大，初始=%d 现在=%d", initialOffset, m.chatPanel.vp.YOffset)
	}

	// 释放左键。
	release := tea.MouseMsg{X: sx, Y: thumbCenter + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	nm, _ = m.Update(release)
	m = modelPtr(nm)
	if m.chatPanel.scrollbarDragging {
		t.Fatal("释放左键后应结束拖动")
	}
}

// modelPtr 将 tea.Model 转换为 *Model，支持指针与值两种形式。
func modelPtr(m tea.Model) *Model {
	if mv, ok := m.(*Model); ok {
		return mv
	}
	if mv, ok := m.(Model); ok {
		return &mv
	}
	panic("model is not tui.Model")
}

// TestRightPanelVisibleWithMetaAgent 验证：会话激活且存在 MetaAgent 时，
// 宽度≥80 应自动展示右侧 Agent 编排栏；宽度不足时隐藏。
func TestRightPanelVisibleWithMetaAgent(t *testing.T) {
	soulPath := filepath.Join(t.TempDir(), "soul.md")
	if err := os.WriteFile(soulPath, []byte("test persona"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	rt, err := runtime.New(soulPath, skill.BuiltinPool())
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rt.SetAgentConfig(&config.AgentConfig{})

	agentSvc := testAgent(t, "收到，开始处理。")
	session, _ := agentSvc.CreateSession(context.Background(), agent.CreateRequest{Goal: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := agentSvc.Get(context.Background(), session.ID)
		if snap != nil && enums.SessionStatus(snap.Status) == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:       NewStyles(),
		chatPanel:    ChatPanel{vp: viewport.New(80, 20)},
		width:        80,
		height:       24,
		agent:        agentSvc,
		streamEvents: make(chan agent.Event, 16),
		httpAddr:     "http://127.0.0.1:1",
		flashMu:      &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")
	m.refreshSessions()
	if len(m.sessions) == 0 {
		t.Fatal("expected at least one session after CreateSession")
	}
	m.selectSession(0)

	if len(m.agentTreePanel.nodes) == 0 {
		t.Fatal("selectSession 后应至少包含 MetaAgent")
	}
	if !m.rightPanelVisible() {
		t.Fatalf("宽度 80 且有 MetaAgent，右侧面板应显示")
	}
	view := m.View()
	if !strings.Contains(view, "Agent 编排") {
		t.Fatalf("视图中应出现 Agent 编排面板，got:\n%s", view)
	}

	// 宽度不足时应隐藏。
	m.width = 70
	if m.rightPanelVisible() {
		t.Fatalf("宽度 70 时不应显示右侧面板")
	}
}

// TestRightPanelLayoutDoesNotOverflow 验证：右侧面板同时展示计划与 Agent 编排，
// 且整体视图宽度不超过终端宽度，避免 box-drawing 字符宽度计算错误导致溢位。
func TestRightPanelLayoutDoesNotOverflow(t *testing.T) {
	soulPath := filepath.Join(t.TempDir(), "soul.md")
	if err := os.WriteFile(soulPath, []byte("test persona"), 0644); err != nil {
		t.Fatalf("write soul: %v", err)
	}

	rt, err := runtime.New(soulPath, skill.BuiltinPool())
	if err != nil {
		t.Fatalf("init runtime: %v", err)
	}
	rt.SetAgentConfig(&config.AgentConfig{})

	agentSvc := testAgent(t, "收到，开始处理。")
	session, _ := agentSvc.CreateSession(context.Background(), agent.CreateRequest{Goal: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := agentSvc.Get(context.Background(), session.ID)
		if snap != nil && enums.SessionStatus(snap.Status) == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:       NewStyles(),
		chatPanel:    ChatPanel{vp: viewport.New(80, 20)},
		width:        120,
		height:       40,
		agent:        agentSvc,
		streamEvents: make(chan agent.Event, 16),
		httpAddr:     "http://127.0.0.1:1",
		flashMu:      &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")
	m.refreshSessions()
	if len(m.sessions) == 0 {
		t.Fatal("expected at least one session after CreateSession")
	}
	m.selectSession(0)

	view := m.View()
	if !strings.Contains(view, "Agent 编排") {
		t.Fatalf("视图中应出现 Agent 编排面板，got:\n%s", view)
	}
	if !strings.Contains(view, "执行计划") {
		t.Fatalf("视图中应出现执行计划面板，got:\n%s", view)
	}
	if got := lipgloss.Width(view); got > m.width {
		t.Fatalf("视图宽度 %d 超过终端宽度 %d，右侧栏可能溢出", got, m.width)
	}
}

// TestRightPanelShowsBothPanelsEvenWhenShort 验证：即使终端高度较紧张，
// 右侧计划栏与 Agent 编排栏也应同时出现，而不是计划栏被完全丢弃。
func TestRightPanelShowsBothPanelsEvenWhenShort(t *testing.T) {
	m := &Model{
		styles:           NewStyles(),
		chatPanel:        ChatPanel{vp: viewport.New(80, 20)},
		width:            80,
		height:           12,
		rightPanelForced: 1,
		flashMu:          &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")

	view := m.View()
	if !strings.Contains(view, "Agent 编排") {
		t.Fatalf("高度 12 时仍应显示 Agent 编排面板，got:\n%s", view)
	}
	if !strings.Contains(view, "执行计划") {
		t.Fatalf("高度 12 时仍应显示执行计划面板，got:\n%s", view)
	}
}

// TestLongUserMessageWrapsAtRightPanelBoundary 验证：当右侧栏显示时，
// 超长用户消息在对话区可用宽度内自动换行，而不是被截断为 "…"。
func TestLongUserMessageWrapsAtRightPanelBoundary(t *testing.T) {
	m := &Model{
		styles:           NewStyles(),
		chatPanel:        ChatPanel{vp: viewport.New(80, 20)},
		width:            80,
		height:           24,
		rightPanelForced: 1, // 强制显示右侧栏，模拟右侧栏出现后的窄对话区
		flashMu:          &sync.Mutex{},
	}
	m.chatPanel.vp.SetContent("")

	cw := m.chatContentWidth()
	if cw >= 80-2 {
		t.Fatalf("chatContentWidth 应因右侧栏而变窄，got %d", cw)
	}

	m.chatPanel.pendingFirstMessage = strings.Repeat("a", 200)
	content := m.chatPanel.buildContent(m.collectChatItems(), m.styles, cw)
	if strings.Contains(content, "…") {
		t.Fatalf("长用户消息不应被截断为省略号，got:\n%s", content)
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 2 {
		t.Fatalf("长用户消息应换行为多行，got %d line(s):\n%s", len(lines), content)
	}
	if !strings.Contains(lines[0], "You") {
		t.Fatalf("首行应保留 You 标签，got:\n%s", lines[0])
	}
}

// TestPlanPanelReflectsAgentStatuses 验证：当后端 TaskBoard 未及时更新时，
// 右侧面板的计划进度仍会根据 Agent 实例的真实状态显示完成率与 Done 标记。
func TestPlanPanelReflectsAgentStatuses(t *testing.T) {
	mock := &mockAgentForPlan{
		sessionID: "session-1",
		goal:      "塔防游戏 demo",
		boardSnap: board.Snapshot{
			Goal: "塔防游戏 demo",
			Tasks: []board.SubTask{
				{ID: "t1", Title: "战斗领域 - 实现怪物路径"},
				{ID: "t2", Title: "UI领域 - Canvas 渲染"},
				{ID: "t3", Title: "经济领域 - 金币系统"},
			},
		},
	}

	m := &Model{
		styles:           NewStyles(),
		chatPanel:        ChatPanel{vp: viewport.New(80, 20)},
		width:            120,
		height:           40,
		agent:            mock,
		rightPanelForced: 1,
		flashMu:          &sync.Mutex{},
		sessions: []*server.Session{
			{ID: "session-1", Goal: "塔防游戏 demo"},
		},
		sessionsCursor: 0,
		agentTreePanel: AgentTreePanel{nodes: []agentTreeNode{
			{depth: 0, instID: "MetaAgent", name: "MetaAgent", roleType: enums.RoleTypeMeta, status: enums.RoleStatusDone},
			{depth: 1, instID: "d1", name: "战斗领域", domain: "战斗领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusDone},
			{depth: 1, instID: "d2", name: "UI领域", domain: "UI领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusActive},
			{depth: 1, instID: "d3", name: "经济领域", domain: "经济领域", roleType: enums.RoleTypeDomain, status: enums.RoleStatusWaiting},
		}},
	}
	m.chatPanel.vp.SetContent("")

	view := m.View()
	// 进度应大于 0（3 个里 1 个完成）
	if !strings.Contains(view, "33%") && !strings.Contains(view, "34%") {
		t.Fatalf("计划面板应显示约 33%% 进度，got:\n%s", view)
	}
	// 战斗领域应显示完成
	if !strings.Contains(view, "战斗领域") {
		t.Fatalf("计划面板应包含战斗领域任务，got:\n%s", view)
	}
	// 检查完成标记出现（✓ Done 或中文完成徽章）
	if !strings.Contains(view, "✓") {
		t.Fatalf("计划面板应显示完成标记，got:\n%s", view)
	}
}
