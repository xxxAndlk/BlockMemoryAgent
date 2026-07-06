package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/skill"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeMetaAgentForRender 是一个立即结束的 MetaAgent 节点，用于在测试中
// 快速得到一个包含用户消息和助手回复的已完成会话。
type fakeMetaAgentForRender struct {
	summary string
}

func (n *fakeMetaAgentForRender) Name() string { return "MetaAgent" }
func (n *fakeMetaAgentForRender) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.SessionSummary = n.summary
	state.NextAction = enums.ActionFinish
	return state, nil
}

type fakeSinkerForRender struct{}

func (n *fakeSinkerForRender) Name() string { return "Sinker" }
func (n *fakeSinkerForRender) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}

func minimalRoleConfigForRender() *pkgconfig.RoleConfigFile {
	return &pkgconfig.RoleConfigFile{
		MetaAgent: pkgconfig.MetaAgentConfig{
			MaxBlocks:       4,
			SummaryInterval: 1,
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

	cfg := minimalRoleConfigForRender()
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)

	rt := runtime.New(soulPath, skill.BuiltinPool())
	rt.SetAgentConfig(&config.AgentConfig{
		StallSteps:           30,
		MaxRepeatFingerprint: 3,
		SessionTimeoutMin:    60,
	})

	meta := &fakeMetaAgentForRender{summary: "收到，开始处理。"}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &fakeSinkerForRender{}

	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetRuntime(rt)
	builder.AddNode(meta)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	g := builder.Build()

	sessionMgr := server.NewSessionManager(g, registry)

	m := &Model{
		styles:     NewStyles(),
		chatVP:     viewport.New(80, 20),
		width:      80,
		height:     24,
		sessionMgr: sessionMgr,
		registry:   registry,
		httpAddr:   "http://127.0.0.1:1",
		flashMu:    &sync.Mutex{},
	}
	m.chatVP.SetContent("")

	// 无会话时发送首条消息
	m.submitInput("hello")
	if m.pendingFirstMessage != "hello" {
		t.Fatalf("pendingFirstMessage 应被设置，got %q", m.pendingFirstMessage)
	}
	view := m.View()
	if !strings.Contains(view, "You hello") {
		t.Fatalf("本地预展示消息应可见，got:\n%s", view)
	}

	// 模拟后端会话创建完成（直接创建，跳过异步 HTTP）
	session := sessionMgr.CreateSession(context.Background(), "hello")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := sessionMgr.SnapshotSession(session.ID)
		if snap != nil && snap.Status == enums.SessionStatusCompleted {
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

	if m.pendingFirstMessage != "" {
		t.Fatalf("selectSession 后 pendingFirstMessage 应被清空，got %q", m.pendingFirstMessage)
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

	cfg := minimalRoleConfigForRender()
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)

	rt := runtime.New(soulPath, skill.BuiltinPool())
	rt.SetAgentConfig(&config.AgentConfig{
		StallSteps:           30,
		MaxRepeatFingerprint: 3,
		SessionTimeoutMin:    60,
	})

	meta := &fakeMetaAgentForRender{summary: "你好！我是你的多 Agent 编排助手 BlockMemoryAgent。"}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &fakeSinkerForRender{}

	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetRuntime(rt)
	builder.AddNode(meta)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	g := builder.Build()

	sessionMgr := server.NewSessionManager(g, registry)

	// 创建会话；fake MetaAgent 立即结束，不会并发修改 Messages
	session := sessionMgr.CreateSession(context.Background(), "a")

	// 等待会话完成，确保 Messages 已追加助手总结
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := sessionMgr.SnapshotSession(session.ID)
		if snap != nil && snap.Status == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:     NewStyles(),
		chatVP:     viewport.New(80, 20),
		width:      80,
		height:     24,
		sessionMgr: sessionMgr,
		registry:   registry,
		httpAddr:   "http://127.0.0.1:1",
		flashMu:    &sync.Mutex{},
	}
	m.chatVP.SetContent("")
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
		styles:  NewStyles(),
		chatVP:  viewport.New(80, 5),
		width:   80,
		height:  12,
		flashMu: &sync.Mutex{},
	}
	// 顶栏 1 行 + 主内容区 8 行 + 输入栏 3 行 = 12 行
	// mainContentHeight = 12 - 1 - 3 - 1 = 7
	if h := m.mainContentHeight(); h != 7 {
		t.Fatalf("mainContentHeight 应为 7，got %d", h)
	}

	// 构造 20 行内容，使 viewport 可滚动。
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = strings.Repeat("x", 70)
	}
	m.chatVP.SetContent(strings.Join(lines, "\n"))
	m.chatVP.YOffset = 0

	totalLines := m.chatVP.TotalLineCount()
	viewportH := m.chatVP.VisibleLineCount()
	if totalLines <= viewportH {
		t.Fatalf("内容应超出 viewport，totalLines=%d viewportH=%d", totalLines, viewportH)
	}

	sx, sy, sw, sh := m.scrollbarArea()
	if sx < 0 || sy != 1 || sw != 1 || sh != 7 {
		t.Fatalf("scrollbarArea 异常: x=%d y=%d w=%d h=%d", sx, sy, sw, sh)
	}

	thumbStart, thumbEnd := m.scrollbarThumbBounds()
	if thumbStart < 0 || thumbEnd < thumbStart {
		t.Fatalf("滑块边界异常: start=%d end=%d", thumbStart, thumbEnd)
	}
	thumbCenter := sy + (thumbStart+thumbEnd)/2

	// 在滑块上按下左键开始拖动。
	press := tea.MouseMsg{X: sx, Y: thumbCenter, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	nm, _ := m.Update(press)
	m = modelPtr(nm)
	if !m.scrollbarDragging {
		t.Fatal("在滑块上按下左键后应开始拖动")
	}

	initialOffset := m.chatVP.YOffset

	// 向下拖动 3 行。
	drag := tea.MouseMsg{X: sx, Y: thumbCenter + 3, Button: tea.MouseButtonNone, Action: tea.MouseActionMotion}
	nm, _ = m.Update(drag)
	m = modelPtr(nm)
	if m.chatVP.YOffset <= initialOffset {
		t.Fatalf("向下拖动后 YOffset 应增大，初始=%d 现在=%d", initialOffset, m.chatVP.YOffset)
	}

	// 释放左键。
	release := tea.MouseMsg{X: sx, Y: thumbCenter + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}
	nm, _ = m.Update(release)
	m = modelPtr(nm)
	if m.scrollbarDragging {
		t.Fatal("释放左键后应结束拖动")
	}
}

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

	cfg := minimalRoleConfigForRender()
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)

	rt := runtime.New(soulPath, skill.BuiltinPool())
	rt.SetAgentConfig(&config.AgentConfig{
		StallSteps:           30,
		MaxRepeatFingerprint: 3,
		SessionTimeoutMin:    60,
	})

	meta := &fakeMetaAgentForRender{summary: "收到，开始处理。"}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &fakeSinkerForRender{}

	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetRuntime(rt)
	builder.AddNode(meta)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	g := builder.Build()

	sessionMgr := server.NewSessionManager(g, registry)
	session := sessionMgr.CreateSession(context.Background(), "x")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := sessionMgr.SnapshotSession(session.ID)
		if snap != nil && snap.Status == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:     NewStyles(),
		chatVP:     viewport.New(80, 20),
		width:      80,
		height:     24,
		sessionMgr: sessionMgr,
		registry:   registry,
		httpAddr:   "http://127.0.0.1:1",
		flashMu:    &sync.Mutex{},
	}
	m.chatVP.SetContent("")
	m.refreshSessions()
	if len(m.sessions) == 0 {
		t.Fatal("expected at least one session after CreateSession")
	}
	m.selectSession(0)

	if len(m.agentsNodes) == 0 {
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

	cfg := minimalRoleConfigForRender()
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)

	rt := runtime.New(soulPath, skill.BuiltinPool())
	rt.SetAgentConfig(&config.AgentConfig{
		StallSteps:           30,
		MaxRepeatFingerprint: 3,
		SessionTimeoutMin:    60,
	})

	meta := &fakeMetaAgentForRender{summary: "收到，开始处理。"}
	escalation := graph.NewEscalationHandlerNode()
	sinker := &fakeSinkerForRender{}

	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.SetRuntime(rt)
	builder.AddNode(meta)
	builder.AddNode(escalation)
	builder.AddNode(sinker)
	g := builder.Build()

	sessionMgr := server.NewSessionManager(g, registry)
	session := sessionMgr.CreateSession(context.Background(), "x")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap := sessionMgr.SnapshotSession(session.ID)
		if snap != nil && snap.Status == enums.SessionStatusCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	m := &Model{
		styles:     NewStyles(),
		chatVP:     viewport.New(80, 20),
		width:      120,
		height:     40,
		sessionMgr: sessionMgr,
		registry:   registry,
		httpAddr:   "http://127.0.0.1:1",
		flashMu:    &sync.Mutex{},
	}
	m.chatVP.SetContent("")
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
		chatVP:           viewport.New(80, 20),
		width:            80,
		height:           12,
		rightPanelForced: 1,
		flashMu:          &sync.Mutex{},
	}
	m.chatVP.SetContent("")

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
		chatVP:           viewport.New(80, 20),
		width:            80,
		height:           24,
		rightPanelForced: 1, // 强制显示右侧栏，模拟右侧栏出现后的窄对话区
		flashMu:          &sync.Mutex{},
	}
	m.chatVP.SetContent("")

	cw := m.chatContentWidth()
	if cw >= 80-2 {
		t.Fatalf("chatContentWidth 应因右侧栏而变窄，got %d", cw)
	}

	m.pendingFirstMessage = strings.Repeat("a", 200)
	content := m.buildChatContent(cw)
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
