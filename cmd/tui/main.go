package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/internal/model"
	"github.com/blockmemory/agent/pkg/config"
	"github.com/blockmemory/agent/pkg/types"
)

// Styles TUI样式
type Styles struct {
	Title       lipgloss.Style
	Header      lipgloss.Style
	FocusBorder lipgloss.Style
	BlurBorder  lipgloss.Style
	TreeMeta    lipgloss.Style
	TreeDomain  lipgloss.Style
	TreeSub     lipgloss.Style
	TreeAssist  lipgloss.Style
	TreeDone    lipgloss.Style
	TreeActive  lipgloss.Style
	LogInfo     lipgloss.Style
	LogSuccess  lipgloss.Style
	LogWarn     lipgloss.Style
	StatLabel   lipgloss.Style
	StatValue   lipgloss.Style
	HelpBar     lipgloss.Style
	CallStack   lipgloss.Style
	SessionSum  lipgloss.Style
	StepNode    lipgloss.Style
}

func NewStyles() *Styles {
	return &Styles{
		Title:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7")).Padding(0, 1),
		Header:      lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#BB9AF7")),
		FocusBorder: lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#7AA2F7")),
		BlurBorder:  lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#414868")),
		TreeMeta:    lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")).Bold(true),
		TreeDomain:  lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")).Bold(true),
		TreeSub:     lipgloss.NewStyle().Foreground(lipgloss.Color("#7AA2F7")),
		TreeAssist:  lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
		TreeDone:    lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
		TreeActive:  lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")).Bold(true),
		LogInfo:     lipgloss.NewStyle().Foreground(lipgloss.Color("#A9B1D6")),
		LogSuccess:  lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
		LogWarn:     lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")),
		StatLabel:   lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
		StatValue:   lipgloss.NewStyle().Foreground(lipgloss.Color("#C0CAF5")).Bold(true),
		HelpBar:     lipgloss.NewStyle().Background(lipgloss.Color("#1F2335")).Foreground(lipgloss.Color("#A9B1D6")).Padding(0, 1),
		CallStack:   lipgloss.NewStyle().Foreground(lipgloss.Color("#7AA2F7")),
		SessionSum:  lipgloss.NewStyle().Foreground(lipgloss.Color("#C0CAF5")).Italic(true),
		StepNode:    lipgloss.NewStyle().Background(lipgloss.Color("#414868")).Foreground(lipgloss.Color("#C0CAF5")).Padding(0, 1),
	}
}

// stepMsg 每一步执行完成的消息
type stepMsg struct {
	state       *types.ThreeLayerState
	currentNode string
	stepCount   int
}

// doneMsg 会话完成消息
type doneMsg struct {
	state     *types.ThreeLayerState
	stepCount int
	err       error
}

// LogEntry 日志条目
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
}

// Model bubbletea模型
type Model struct {
	width  int
	height int

	// 三层架构运行时
	registry    *graph.RoleRegistry
	graph       *graph.ThreeLayerGraph
	state       *types.ThreeLayerState
	completed   bool
	stepCount   int
	currentNode string
	err         error

	// TUI状态
	focusPanel  int // 0: roleTree, 1: sessionState, 2: eventLog, 3: stats
	roleCursor  int
	logCursor   int
	showDetail  bool
	logs        []LogEntry

	styles *Styles
}

func initialModel(registry *graph.RoleRegistry, g *graph.ThreeLayerGraph, state *types.ThreeLayerState) Model {
	return Model{
		registry: registry,
		graph:    g,
		state:    state,
		logs:     make([]LogEntry, 0),
		styles:   NewStyles(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.runSession(),
		tickCmd(),
	)
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

type tickMsg struct{}

// runSession 在后台逐步运行三层架构会话
func (m Model) runSession() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		current := m.state
		currentStr := "MetaAgent"
		stepCount := 0
		maxSteps := 200

		for {
			if stepCount >= maxSteps {
				return doneMsg{state: current, stepCount: stepCount, err: fmt.Errorf("max steps exceeded")}
			}
			stepCount++

			node, ok := m.graph.GetNode(currentStr)
			if !ok {
				node = m.graph.ResolveInstanceNode(currentStr)
				if node == nil {
					return doneMsg{state: current, stepCount: stepCount, err: fmt.Errorf("node %s not found", currentStr)}
				}
			}

			newState, err := node.Invoke(ctx, current)
			if err != nil {
				return doneMsg{state: current, stepCount: stepCount, err: err}
			}
			current = newState

			// 发送步骤消息（异步，不阻塞）
			// 注意：在 bubbletea 的 Cmd 中不能直接发送多个消息
			// 这里返回 doneMsg 时包含最终状态

			if current.NextAction == types.ActionFinish {
				return doneMsg{state: current, stepCount: stepCount, err: nil}
			}

			next := m.graph.DetermineNext(currentStr, current)
			if next == "" {
				return doneMsg{state: current, stepCount: stepCount, err: nil}
			}
			currentStr = next
		}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.focusPanel = (m.focusPanel + 1) % 4
		case "shift+tab":
			m.focusPanel = (m.focusPanel - 1 + 4) % 4
		case "up":
			if m.focusPanel == 0 && m.roleCursor > 0 {
				m.roleCursor--
			} else if m.focusPanel == 2 && m.logCursor > 0 {
				m.logCursor--
			}
		case "down":
			if m.focusPanel == 0 {
				m.roleCursor++
			} else if m.focusPanel == 2 {
				m.logCursor++
			}
		case "d":
			m.showDetail = !m.showDetail
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		if !m.completed {
			return m, tickCmd()
		}

	case doneMsg:
		m.state = msg.state
		m.stepCount = msg.stepCount
		m.completed = true
		m.err = msg.err
		if msg.err != nil {
			m.logs = append(m.logs, LogEntry{
				Time:    time.Now(),
				Level:   "error",
				Message: msg.err.Error(),
			})
		} else {
			m.logs = append(m.logs, LogEntry{
				Time:    time.Now(),
				Level:   "success",
				Message: fmt.Sprintf("Session completed in %d steps", msg.stepCount),
			})
		}
	}

	return m, nil
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	titleH := 1
	helpH := 1
	contentH := m.height - titleH - helpH

	topH := contentH * 3 / 5
	bottomH := contentH - topH
	leftW := m.width / 3
	rightW := m.width - leftW

	status := m.styles.TreeActive.Render("●")
	if m.completed {
		status = m.styles.LogSuccess.Render("✓")
	}
	if m.err != nil {
		status = m.styles.TreeMeta.Render("✗")
	}

	title := m.styles.Title.Render(fmt.Sprintf(" BlockMemory Agent Console %s │ Session: %s ", status, m.state.SessionID))

	roleTree := m.renderRoleTree(leftW, topH)
	sessionState := m.renderSessionState(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, roleTree, sessionState)

	eventLog := m.renderEventLog(leftW, bottomH)
	stats := m.renderStats(rightW, bottomH)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventLog, stats)

	content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)

	help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [D]etail ")

	return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

// renderRoleTree 渲染角色树面板
func (m Model) renderRoleTree(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Role Hierarchy"))

	// MetaAgent
	metaStatus := m.styles.TreeActive.Render("active")
	if m.completed {
		metaStatus = m.styles.TreeDone.Render("done")
	}
	lines = append(lines, m.styles.TreeMeta.Render("◆ MetaAgent")+"  "+metaStatus)

	// 按层级收集角色
	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	// DomainAgent
	for _, inst := range instances {
		if inst.Type != types.RoleTypeDomain {
			continue
		}
		status := m.statusStyle(inst.Status).Render(string(inst.Status))
		lines = append(lines, fmt.Sprintf("  └── %s %s [%s]", m.styles.TreeDomain.Render("◆"), inst.Domain, status))

		// SubDomainAgent
		for _, childID := range inst.Children {
			child := m.registry.GetInstance(childID)
			if child == nil || child.Type != types.RoleTypeSubDomain {
				continue
			}
			cstatus := m.statusStyle(child.Status).Render(string(child.Status))
			lines = append(lines, fmt.Sprintf("      └── %s %s [%s]", m.styles.TreeSub.Render("◇"), child.Domain, cstatus))

			// Assistant under SubDomain
			for _, subChildID := range child.Children {
				subChild := m.registry.GetInstance(subChildID)
				if subChild == nil {
					continue
				}
				scstatus := m.statusStyle(subChild.Status).Render(string(subChild.Status))
				roleDef := m.registry.GetRoleDef(subChild.RoleDefID)
				name := "临时助手"
				if roleDef != nil {
					name = roleDef.Name
				}
				lines = append(lines, fmt.Sprintf("          └── %s %s [%s]", m.styles.TreeAssist.Render("▸"), name, scstatus))
			}
		}

		// Assistant directly under Domain
		for _, childID := range inst.Children {
			child := m.registry.GetInstance(childID)
			if child == nil || (child.Type != types.RoleTypeFixed && child.Type != types.RoleTypeDynamic) {
				continue
			}
			cstatus := m.statusStyle(child.Status).Render(string(child.Status))
			roleDef := m.registry.GetRoleDef(child.RoleDefID)
			name := "临时助手"
			if roleDef != nil {
				name = roleDef.Name
			}
			lines = append(lines, fmt.Sprintf("      └── %s %s [%s]", m.styles.TreeAssist.Render("▸"), name, cstatus))
		}
	}

	content := strings.Join(lines, "\n")
	border := m.panelBorder(0, w, h)
	return border.Render(content)
}

func (m Model) statusStyle(status types.RoleStatus) lipgloss.Style {
	switch status {
	case types.RoleStatusActive:
		return m.styles.TreeActive
	case types.RoleStatusDone:
		return m.styles.TreeDone
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89"))
	}
}

// renderSessionState 渲染会话状态面板
func (m Model) renderSessionState(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Session State"))

	// Summary
	sum := m.state.SessionSummary
	if sum == "" {
		sum = "(empty)"
	}
	lines = append(lines, "Summary: "+m.styles.SessionSum.Render(sum))

	// Blocks
	lines = append(lines, fmt.Sprintf("Active Blocks: %d", len(m.state.ActiveBlocks)))
	lines = append(lines, fmt.Sprintf("Completed: %v", m.state.CompletedBlocks))

	// Current
	if m.state.CurrentDomain != "" {
		lines = append(lines, fmt.Sprintf("Current Domain: %s", m.styles.TreeDomain.Render(m.state.CurrentDomain)))
	}

	// Call Stack
	lines = append(lines, "")
	lines = append(lines, m.styles.Header.Render("Call Stack"))
	if len(m.state.CallStack) == 0 {
		lines = append(lines, m.styles.LogInfo.Render("  (empty)"))
	} else {
		for i := len(m.state.CallStack) - 1; i >= 0; i-- {
			req := m.state.CallStack[i]
			caller := m.registry.GetInstance(req.CallerID)
			callee := m.registry.GetInstance(req.CalleeID)
			callerName := req.CallerID
			calleeName := req.CalleeID
			if caller != nil {
				callerName = caller.Domain
				if callerName == "" {
					callerName = string(caller.Type)
				}
			}
			if callee != nil {
				calleeName = callee.Domain
				if calleeName == "" {
					roleDef := m.registry.GetRoleDef(callee.RoleDefID)
					if roleDef != nil {
						calleeName = roleDef.Name
					} else {
						calleeName = string(callee.Type)
					}
				}
			}
			lines = append(lines, fmt.Sprintf("  [%d] %s → %s: %s", i,
				m.styles.CallStack.Render(callerName),
				m.styles.TreeAssist.Render(calleeName),
				req.Task))
		}
	}

	// Next Action
	lines = append(lines, "")
	actionStyle := m.styles.LogInfo
	if m.state.NextAction == types.ActionFinish {
		actionStyle = m.styles.LogSuccess
	}
	lines = append(lines, fmt.Sprintf("Next Action: %s", actionStyle.Render(string(m.state.NextAction))))

	content := strings.Join(lines, "\n")
	border := m.panelBorder(1, w, h)
	return border.Render(content)
}

// renderEventLog 渲染事件日志面板
func (m Model) renderEventLog(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Event Log"))

	// 从角色实例状态生成日志
	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	var entries []LogEntry
	for _, inst := range instances {
		if inst.Status == types.RoleStatusDone {
			roleDef := m.registry.GetRoleDef(inst.RoleDefID)
			name := inst.ID
			if roleDef != nil {
				name = roleDef.Name
			}
			entries = append(entries, LogEntry{
				Time:    inst.CreatedAt,
				Level:   "success",
				Message: fmt.Sprintf("%s [%s] completed", name, inst.Domain),
			})
		}
	}

	// 添加步骤日志
	if m.completed {
		entries = append(entries, LogEntry{
			Time:    time.Now(),
			Level:   "success",
			Message: fmt.Sprintf("Session finished in %d steps", m.stepCount),
		})
	}

	// 限制显示数量
	start := 0
	if len(entries) > h-3 {
		start = len(entries) - (h - 3)
	}

	for i := start; i < len(entries); i++ {
		entry := entries[i]
		var style lipgloss.Style
		switch entry.Level {
		case "success":
			style = m.styles.LogSuccess
		case "warn":
			style = m.styles.LogWarn
		default:
			style = m.styles.LogInfo
		}
		lines = append(lines, style.Render(fmt.Sprintf("  %s %s", entry.Time.Format("15:04:05"), entry.Message)))
	}

	if len(entries) == 0 {
		lines = append(lines, m.styles.LogInfo.Render("  (no events)"))
	}

	content := strings.Join(lines, "\n")
	border := m.panelBorder(2, w, h)
	return border.Render(content)
}

// renderStats 渲染统计面板
func (m Model) renderStats(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Statistics"))

	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	var metaCount, domainCount, subCount, assistCount int
	var doneCount int
	for _, inst := range instances {
		switch inst.Type {
		case types.RoleTypeMeta:
			metaCount++
		case types.RoleTypeDomain:
			domainCount++
		case types.RoleTypeSubDomain:
			subCount++
		case types.RoleTypeFixed, types.RoleTypeDynamic:
			assistCount++
		}
		if inst.Status == types.RoleStatusDone {
			doneCount++
		}
	}

	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("Total Roles:"), m.styles.StatValue.Render(fmt.Sprintf("%d", len(instances)))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("  MetaAgent:"), m.styles.StatValue.Render(fmt.Sprintf("%d", metaCount))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("  DomainAgent:"), m.styles.StatValue.Render(fmt.Sprintf("%d", domainCount))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("  SubDomainAgent:"), m.styles.StatValue.Render(fmt.Sprintf("%d", subCount))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("  Assistants:"), m.styles.StatValue.Render(fmt.Sprintf("%d", assistCount))))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("Completed:"), m.styles.LogSuccess.Render(fmt.Sprintf("%d/%d", doneCount, len(instances)))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("Steps:"), m.styles.StatValue.Render(fmt.Sprintf("%d", m.stepCount))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("Call Stack:"), m.styles.StatValue.Render(fmt.Sprintf("%d", len(m.state.CallStack)))))
	lines = append(lines, fmt.Sprintf("%s %s", m.styles.StatLabel.Render("Status:"), m.statusText()))

	content := strings.Join(lines, "\n")
	border := m.panelBorder(3, w, h)
	return border.Render(content)
}

func (m Model) statusText() string {
	if m.err != nil {
		return m.styles.TreeMeta.Render("Error")
	}
	if m.completed {
		return m.styles.LogSuccess.Render("Finish")
	}
	return m.styles.TreeActive.Render("Running")
}

func (m Model) panelBorder(panelIndex, w, h int) lipgloss.Style {
	border := m.styles.BlurBorder
	if m.focusPanel == panelIndex {
		border = m.styles.FocusBorder
	}
	return border.Width(w - 2).Height(h - 2).Padding(0, 1)
}

func main() {
	ctx := context.Background()

	// 1. 加载角色配置
	roleCfg, err := config.LoadRoleConfig("config/roles.yaml")
	if err != nil {
		log.Printf("Warning: load role config failed: %v", err)
		roleCfg = defaultConfig()
	}

	// 2. 初始化模型工厂
	modelFactory := model.NewModelFactory(roleCfg)
	_ = modelFactory.WarmUp(ctx)

	// 3. 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 4. 构建三层图
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	escalation := &mockThreeLayerNode{name: "EscalationHandler"}
	sinker := &mockThreeLayerNode{name: "Sinker"}

	threeLayerGraph := graph.BuildThreeLayerGraph(metaAgent, escalation, sinker, registry, factory)

	// 5. 启动会话
	sessionID := "tui-session-001"
	state := types.NewThreeLayerState(sessionID)
	state.DomainGoal = "修复商城主页穿模问题"

	// 6. 启动TUI
	p := tea.NewProgram(
		initialModel(registry, threeLayerGraph, state),
		tea.WithAltScreen(),
	)

	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}

// mockThreeLayerNode 模拟三层节点
type mockThreeLayerNode struct {
	name string
}

func (m *mockThreeLayerNode) Name() string {
	return m.name
}

func (m *mockThreeLayerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	if m.name == "Sinker" {
		state.NextAction = types.ActionFinish
	} else {
		state.NextAction = types.ActionContinue
	}
	return state, nil
}

func defaultConfig() *config.RoleConfigFile {
	return &config.RoleConfigFile{
		MetaAgent:   config.MetaAgentConfig{MaxBlocks: 5, SummaryInterval: 3},
		DomainAgent: config.DomainAgentConfig{},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "代码助手", Type: types.RoleTypeFixed, Lifecycle: types.RoleLifecyclePermanent, Description: "代码编写与审查", Skills: []string{"代码编写", "代码审查"}, Keywords: []string{"代码", "bug"}, CanBeCalled: true},
			{ID: "ui_assistant", Name: "UI助手", Type: types.RoleTypeFixed, Lifecycle: types.RoleLifecyclePermanent, Description: "前端UI实现", Skills: []string{"UI修复", "组件开发"}, Keywords: []string{"UI", "样式"}, CanBeCalled: true},
		},
	}
}
