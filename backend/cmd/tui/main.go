package main

// cmd/tui 是 bubbletea 终端 UI 入口: 本地构建三层图并实时渲染角色树/会话状态/事件日志/统计。
// 与 HTTP 服务解耦，直接在进程内驱动图执行。

import (
	"context" // 上下文
	"fmt"     // 格式化输出
	"log"     // 警告日志
	"strings" // 字符串拼接
	"time"    // 定时器与时间格式化

	tea "github.com/charmbracelet/bubbletea"       // bubbletea 框架
	"github.com/charmbracelet/lipgloss"            // 终端样式
	"github.com/blockmemory/agent/backend/internal/graph" // 三层图
	"github.com/blockmemory/agent/backend/internal/model" // 模型工厂
	"github.com/blockmemory/agent/backend/pkg/config"     // 角色配置
	"github.com/blockmemory/agent/backend/pkg/types"      // 公共类型
)

// Styles 集中管理 TUI 各元素的 lipgloss 样式（Tokyo Night 配色）。
type Styles struct {
	Title       lipgloss.Style // 标题栏
	Header      lipgloss.Style // 面板小标题
	FocusBorder lipgloss.Style // 聚焦面板边框
	BlurBorder  lipgloss.Style // 非聚焦面板边框
	TreeMeta    lipgloss.Style // MetaAgent 行
	TreeDomain  lipgloss.Style // DomainAgent 行
	TreeSub     lipgloss.Style // SubDomainAgent 行
	TreeAssist  lipgloss.Style // Assistant 行
	TreeDone    lipgloss.Style // 完成状态
	TreeActive  lipgloss.Style // 活跃状态
	LogInfo     lipgloss.Style // 普通日志
	LogSuccess  lipgloss.Style // 成功日志
	LogWarn     lipgloss.Style // 警告日志
	StatLabel   lipgloss.Style // 统计标签
	StatValue   lipgloss.Style // 统计数值
	HelpBar     lipgloss.Style // 底部帮助栏
	CallStack   lipgloss.Style // 调用栈
	SessionSum  lipgloss.Style // 会话总结
	StepNode    lipgloss.Style // 步骤节点徽标
}

// NewStyles 构造并返回默认样式集合。
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

// stepMsg 每一步执行完成的消息（当前未直接使用，预留扩展）。
type stepMsg struct {
	state       *types.ThreeLayerState // 当前状态
	currentNode string                 // 当前节点名
	stepCount   int                    // 步数
}

// doneMsg 会话完成消息，携带最终状态、步数与错误。
type doneMsg struct {
	state     *types.ThreeLayerState
	stepCount int
	err       error
}

// LogEntry 日志条目，用于事件日志面板显示。
type LogEntry struct {
	Time    time.Time // 时间戳
	Level   string    // 级别: info/success/warn/error
	Message string    // 文本
}

// Model bubbletea模型，持有运行时图、状态与 TUI 交互状态。
type Model struct {
	width  int // 终端宽
	height int // 终端高

	// 三层架构运行时
	registry    *graph.RoleRegistry       // 角色注册表
	graph       *graph.ThreeLayerGraph    // 三层图
	state       *types.ThreeLayerState    // 当前状态
	completed   bool                       // 是否已完成
	stepCount   int                        // 已执行步数
	currentNode string                     // 当前节点名
	err         error                      // 执行错误

	// TUI状态
	focusPanel  int         // 0: roleTree, 1: sessionState, 2: eventLog, 3: stats
	roleCursor  int         // 角色树光标
	logCursor   int         // 日志光标
	showDetail  bool        // 是否显示详情
	logs        []LogEntry  // 日志条目缓冲

	styles *Styles // 样式集合
}

// initialModel 构造初始 Model，注入图与状态。
func initialModel(registry *graph.RoleRegistry, g *graph.ThreeLayerGraph, state *types.ThreeLayerState) Model {
	return Model{
		registry: registry,
		graph:    g,
		state:    state,
		logs:     make([]LogEntry, 0),
		styles:   NewStyles(),
	}
}

// Init 启动时同时发起会话执行与定时刷新。
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.runSession(), // 后台驱动会话
		tickCmd(),      // 定时刷新
	)
}

// tickCmd 返回 200ms 后触发的 tick 消息，用于周期性重绘。
func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

type tickMsg struct{}

// runSession 在后台逐步运行三层架构会话，循环执行节点直到 Finish 或达到步数上限。
// 返回 doneMsg 携带最终状态与错误。
func (m Model) runSession() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		current := m.state             // 当前状态指针
		currentStr := "MetaAgent"      // 起始节点
		stepCount := 0
		maxSteps := 200                // 步数上限，防止死循环

		for {
			// 超出步数上限则终止
			if stepCount >= maxSteps {
				return doneMsg{state: current, stepCount: stepCount, err: fmt.Errorf("max steps exceeded")}
			}
			stepCount++

			// 查找节点: 优先按名查找，再按动态实例解析
			node, ok := m.graph.GetNode(currentStr)
			if !ok {
				node = m.graph.ResolveInstanceNode(currentStr)
				if node == nil {
					return doneMsg{state: current, stepCount: stepCount, err: fmt.Errorf("node %s not found", currentStr)}
				}
			}

			// 执行节点，失败则终止
			newState, err := node.Invoke(ctx, current)
			if err != nil {
				return doneMsg{state: current, stepCount: stepCount, err: err}
			}
			current = newState

			// 发送步骤消息（异步，不阻塞）
			// 注意：在 bubbletea 的 Cmd 中不能直接发送多个消息
			// 这里返回 doneMsg 时包含最终状态

			// 收到 Finish 动作则结束
			if current.NextAction == types.ActionFinish {
				return doneMsg{state: current, stepCount: stepCount, err: nil}
			}

			// 计算下一节点；无下一节点则结束
			next := m.graph.DetermineNext(currentStr, current)
			if next == "" {
				return doneMsg{state: current, stepCount: stepCount, err: nil}
			}
			currentStr = next
		}
	}
}

// Update 处理键盘、窗口、tick、完成等消息，更新模型状态。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c": // 退出
			return m, tea.Quit
		case "tab": // 切换聚焦面板（正向）
			m.focusPanel = (m.focusPanel + 1) % 4
		case "shift+tab": // 切换聚焦面板（反向）
			m.focusPanel = (m.focusPanel - 1 + 4) % 4
		case "up": // 光标上移
			if m.focusPanel == 0 && m.roleCursor > 0 {
				m.roleCursor--
			} else if m.focusPanel == 2 && m.logCursor > 0 {
				m.logCursor--
			}
		case "down": // 光标下移
			if m.focusPanel == 0 {
				m.roleCursor++
			} else if m.focusPanel == 2 {
				m.logCursor++
			}
		case "d": // 切换详情显示
			m.showDetail = !m.showDetail
		}

	case tea.WindowSizeMsg: // 窗口尺寸变化
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg: // 周期刷新: 仅在未完成时继续 tick
		if !m.completed {
			return m, tickCmd()
		}

	case doneMsg: // 会话完成: 更新状态并追加日志
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

// View 渲染整个界面: 标题 + 上行（角色树|会话状态）+ 下行（事件日志|统计）+ 帮助栏。
func (m Model) View() string {
	// 初始化阶段尚未拿到尺寸，显示占位
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	// 计算各区域高度
	titleH := 1
	helpH := 1
	contentH := m.height - titleH - helpH

	topH := contentH * 3 / 5   // 上行占 3/5
	bottomH := contentH - topH // 下行剩余
	leftW := m.width / 3       // 左列占 1/3
	rightW := m.width - leftW  // 右列剩余

	// 根据状态选择状态指示符: 运行/完成/错误
	status := m.styles.TreeActive.Render("●")
	if m.completed {
		status = m.styles.LogSuccess.Render("✓")
	}
	if m.err != nil {
		status = m.styles.TreeMeta.Render("✗")
	}

	title := m.styles.Title.Render(fmt.Sprintf(" BlockMemory Agent Console %s │ Session: %s ", status, m.state.SessionID))

	// 渲染上行两面板
	roleTree := m.renderRoleTree(leftW, topH)
	sessionState := m.renderSessionState(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, roleTree, sessionState)

	// 渲染下行两面板
	eventLog := m.renderEventLog(leftW, bottomH)
	stats := m.renderStats(rightW, bottomH)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventLog, stats)

	content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)

	help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [D]etail ")

	return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

// renderRoleTree 渲染角色树面板: MetaAgent → DomainAgent → SubDomainAgent → Assistant 层级。
func (m Model) renderRoleTree(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Role Hierarchy"))

	// MetaAgent 顶层节点
	metaStatus := m.styles.TreeActive.Render("active")
	if m.completed {
		metaStatus = m.styles.TreeDone.Render("done")
	}
	lines = append(lines, m.styles.TreeMeta.Render("◆ MetaAgent")+"  "+metaStatus)

	// 按层级收集角色
	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	// DomainAgent 第二层
	for _, inst := range instances {
		if inst.Type != types.RoleTypeDomain {
			continue
		}
		status := m.statusStyle(inst.Status).Render(string(inst.Status))
		lines = append(lines, fmt.Sprintf("  └── %s %s [%s]", m.styles.TreeDomain.Render("◆"), inst.Domain, status))

		// SubDomainAgent 第三层
		for _, childID := range inst.Children {
			child := m.registry.GetInstance(childID)
			if child == nil || child.Type != types.RoleTypeSubDomain {
				continue
			}
			cstatus := m.statusStyle(child.Status).Render(string(child.Status))
			lines = append(lines, fmt.Sprintf("      └── %s %s [%s]", m.styles.TreeSub.Render("◇"), child.Domain, cstatus))

			// SubDomain 下的 Assistant 第四层
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

		// 直挂 Domain 的 Assistant（无 SubDomain 中间层）
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

// statusStyle 根据角色状态返回对应样式。
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

// renderSessionState 渲染会话状态面板: 总结、活跃块、当前域、调用栈、下一动作。
func (m Model) renderSessionState(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Session State"))

	// Summary 会话总结
	sum := m.state.SessionSummary
	if sum == "" {
		sum = "(empty)"
	}
	lines = append(lines, "Summary: "+m.styles.SessionSum.Render(sum))

	// Blocks 活跃与已完成块
	lines = append(lines, fmt.Sprintf("Active Blocks: %d", len(m.state.ActiveBlocks)))
	lines = append(lines, fmt.Sprintf("Completed: %v", m.state.CompletedBlocks))

	// Current 当前域
	if m.state.CurrentDomain != "" {
		lines = append(lines, fmt.Sprintf("Current Domain: %s", m.styles.TreeDomain.Render(m.state.CurrentDomain)))
	}

	// Call Stack 调用栈（自顶向下展示）
	lines = append(lines, "")
	lines = append(lines, m.styles.Header.Render("Call Stack"))
	if len(m.state.CallStack) == 0 {
		lines = append(lines, m.styles.LogInfo.Render("  (empty)"))
	} else {
		// 从栈顶向栈底打印
		for i := len(m.state.CallStack) - 1; i >= 0; i-- {
			req := m.state.CallStack[i]
			caller := m.registry.GetInstance(req.CallerID)
			callee := m.registry.GetInstance(req.CalleeID)
			callerName := req.CallerID
			calleeName := req.CalleeID
			// 解析调用方显示名
			if caller != nil {
				callerName = caller.Domain
				if callerName == "" {
					callerName = string(caller.Type)
				}
			}
			// 解析被调用方显示名
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

	// Next Action 下一动作，Finish 用成功色高亮
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

// renderEventLog 渲染事件日志面板: 从已完成角色实例生成条目，并限制显示数量。
func (m Model) renderEventLog(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Event Log"))

	// 从角色实例状态生成日志
	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	var entries []LogEntry
	for _, inst := range instances {
		// 已完成角色记为成功事件
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

	// 限制显示数量: 只保留尾部可显示的条目
	start := 0
	if len(entries) > h-3 {
		start = len(entries) - (h - 3)
	}

	for i := start; i < len(entries); i++ {
		entry := entries[i]
		// 按级别选择样式
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

// renderStats 渲染统计面板: 按类型统计角色数、完成数、步数、调用栈深度、状态。
func (m Model) renderStats(w, h int) string {
	var lines []string
	lines = append(lines, m.styles.Header.Render("Statistics"))

	sessionID := m.state.SessionID
	instances := m.registry.GetInstancesBySession(sessionID)

	// 分类计数
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

	// 输出各统计项
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

// statusText 返回带样式的状态文本: Error/Finish/Running。
func (m Model) statusText() string {
	if m.err != nil {
		return m.styles.TreeMeta.Render("Error")
	}
	if m.completed {
		return m.styles.LogSuccess.Render("Finish")
	}
	return m.styles.TreeActive.Render("Running")
}

// panelBorder 根据当前聚焦面板返回对应边框样式，并设置宽高。
func (m Model) panelBorder(panelIndex, w, h int) lipgloss.Style {
	border := m.styles.BlurBorder
	if m.focusPanel == panelIndex {
		border = m.styles.FocusBorder
	}
	return border.Width(w - 2).Height(h - 2).Padding(0, 1)
}

// main 是 TUI 入口: 装配依赖 → 构建图 → 启动 bubbletea 程序。
func main() {
	ctx := context.Background()

	// 1. 加载角色配置: 失败则用默认配置
	roleCfg, err := config.LoadRoleConfig("config/roles.yaml")
	if err != nil {
		log.Printf("Warning: load role config failed: %v", err)
		roleCfg = defaultConfig()
	}

	// 2. 初始化模型工厂并预热（忽略错误，回退 Mock）
	modelFactory := model.NewModelFactory(roleCfg)
	_ = modelFactory.WarmUp(ctx)

	// 3. 初始化注册表和角色工厂
	registry := graph.NewRoleRegistry(roleCfg)
	factory := graph.NewRoleFactory(registry, modelFactory, roleCfg)

	// 4. 构建三层图: mock 节点充当升级处理器与终止节点
	metaAgent := graph.NewMetaAgentNode(registry, factory, roleCfg.MetaAgent.MaxBlocks, roleCfg.MetaAgent.SummaryInterval)
	escalation := &mockThreeLayerNode{name: "EscalationHandler"}
	sinker := &mockThreeLayerNode{name: "Sinker"}

	threeLayerGraph := graph.BuildThreeLayerGraph(metaAgent, escalation, sinker, registry, factory)

	// 5. 启动会话: 构造初始状态与目标
	sessionID := "tui-session-001"
	state := types.NewThreeLayerState(sessionID)
	state.DomainGoal = "修复商城主页穿模问题"

	// 6. 启动TUI: 使用 alt screen 全屏模式
	p := tea.NewProgram(
		initialModel(registry, threeLayerGraph, state),
		tea.WithAltScreen(),
	)

	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}

// mockThreeLayerNode 模拟三层节点，用于 TUI 演示中替代真实升级处理器与终止节点。
type mockThreeLayerNode struct {
	name string // 节点名称
}

// Name 返回节点名称。
func (m *mockThreeLayerNode) Name() string {
	return m.name
}

// Invoke 根据 name 决定 NextAction: Sinker 强制 Finish，其他继续。
func (m *mockThreeLayerNode) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	if m.name == "Sinker" {
		state.NextAction = types.ActionFinish
	} else {
		state.NextAction = types.ActionContinue
	}
	return state, nil
}

// defaultConfig 返回内置默认角色配置，供配置加载失败时回退使用。
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
