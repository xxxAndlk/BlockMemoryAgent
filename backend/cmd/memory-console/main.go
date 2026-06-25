package main

// cmd/memory-console 是记忆检查控制台: 通过 SSE 订阅后端事件流，
// 在终端实时展示 Agent 状态、图步骤、事件、统计与 Episode 详情。

import (
	"bufio"         // SSE 行扫描
	"bytes"         // 缓冲区处理 data 行
	"encoding/json" // 解析 SSE 事件 JSON
	"flag"          // 命令行参数
	"fmt"           // 格式化输出
	"net/http"      // SSE 客户端
	"os"            // 标准错误与退出
	"time"          // 时长与时间格式化

	tea "github.com/charmbracelet/bubbletea"   // bubbletea 框架
	"github.com/charmbracelet/lipgloss"        // 终端样式
	"github.com/blockmemory/agent/backend/pkg/types" // 公共事件类型
)

// 命令行参数: 后端地址、订阅 Topic、重连间隔
var (
	endpoint = flag.String("endpoint", "http://localhost:8080", "Backend HTTP/SSE endpoint")
	topicID  = flag.String("topic", "", "Topic ID to subscribe (required)")
	retry    = flag.String("retry", "5s", "SSE reconnection interval")
)

// main 解析参数并启动 bubbletea 程序。
func main() {
	flag.Parse()
	// topic 为必填，缺失则打印用法并退出
	if *topicID == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s --topic TOPIC_ID [--endpoint URL]\n", os.Args[0])
		os.Exit(1)
	}

	// 启动 bubbletea，使用 alt screen 全屏模式
	p := tea.NewProgram(
		initialModel(),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// Model bubbletea 模型，持有 SSE 推送来的各类数据与 TUI 交互状态。
type Model struct {
	width  int // 终端宽
	height int // 终端高

	agents     []types.AgentStatusPayload          // Agent 状态列表
	graphSteps []types.GraphStepPayload            // 图步骤历史
	events     []types.EventPayload                // 事件列表（最新在前）
	episodes   map[string]*types.EpisodePayload    // Episode 详情，按 agent#step 索引
	stats      types.StatsView                     // 全局统计

	focusPanel  int  // 0: agents, 1: graph, 2: events, 3: stats
	agentCursor int  // Agent 列表光标
	eventCursor int  // 事件列表光标
	showDetail  bool // 是否显示 Episode 详情
	paused      bool // 是否暂停（预留）
	connected   bool // SSE 是否连接

	styles *Styles // 样式集合
}

// Styles 集中管理控制台各元素的 lipgloss 样式。
type Styles struct {
	Title        lipgloss.Style
	ActiveAgent  lipgloss.Style // 活跃 Agent
	WaitingAgent lipgloss.Style // 等待中 Agent
	IdleAgent    lipgloss.Style // 空闲 Agent
	ErrorAgent   lipgloss.Style // 错误 Agent
	FocusBorder  lipgloss.Style // 聚焦边框
	BlurBorder   lipgloss.Style // 非聚焦边框
	EventHigh    lipgloss.Style // 高优事件
	EventNormal  lipgloss.Style // 普通事件
	EventDone    lipgloss.Style // 已完成事件
	StepNode     lipgloss.Style // 步骤节点徽标
	StepArrow    lipgloss.Style // 步骤箭头
	HelpBar      lipgloss.Style // 帮助栏
}

// NewStyles 构造并返回默认样式集合。
func NewStyles() *Styles {
	return &Styles{
		Title:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7AA2F7")),
		ActiveAgent:  lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
		WaitingAgent: lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")),
		IdleAgent:    lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
		ErrorAgent:   lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")),
		FocusBorder:  lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#7AA2F7")),
		BlurBorder:   lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#414868")),
		EventHigh:    lipgloss.NewStyle().Foreground(lipgloss.Color("#F7768E")),
		EventNormal:  lipgloss.NewStyle().Foreground(lipgloss.Color("#E0AF68")),
		EventDone:    lipgloss.NewStyle().Foreground(lipgloss.Color("#9ECE6A")),
		StepNode:     lipgloss.NewStyle().Background(lipgloss.Color("#414868")).Foreground(lipgloss.Color("#C0CAF5")).Padding(0, 1),
		StepArrow:    lipgloss.NewStyle().Foreground(lipgloss.Color("#565F89")),
		HelpBar:      lipgloss.NewStyle().Background(lipgloss.Color("#1F2335")).Foreground(lipgloss.Color("#A9B1D6")),
	}
}

// initialModel 构造初始 Model，初始化 episodes map 与样式。
func initialModel() Model {
	return Model{
		episodes: make(map[string]*types.EpisodePayload),
		styles:   NewStyles(),
	}
}

// Init 启动时发起首次 SSE 连接。
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		connectSSE(*endpoint, *topicID),
	)
}

// Update 处理键盘、窗口、SSE 事件、连接状态等消息。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c": // 退出
			return m, tea.Quit
		case "tab": // 切换聚焦（正向）
			m.focusPanel = (m.focusPanel + 1) % 4
		case "shift+tab": // 切换聚焦（反向）
			m.focusPanel = (m.focusPanel - 1 + 4) % 4
		case "up": // 光标上移
			if m.focusPanel == 0 && m.agentCursor > 0 {
				m.agentCursor--
			} else if m.focusPanel == 2 && m.eventCursor > 0 {
				m.eventCursor--
			}
		case "down": // 光标下移（带边界保护）
			if m.focusPanel == 0 && m.agentCursor < len(m.agents)-1 {
				m.agentCursor++
			} else if m.focusPanel == 2 && m.eventCursor < len(m.events)-1 {
				m.eventCursor++
			}
		case "f3": // 切换详情显示
			m.showDetail = !m.showDetail
		case "p": // 切换暂停（预留）
			m.paused = !m.paused
		}

	case tea.WindowSizeMsg: // 窗口尺寸变化
		m.width = msg.Width
		m.height = msg.Height

	case SSEMsg: // 收到一条 SSE 事件
		m.applyEvent(msg.Event)
		// 重新发起 SSE 连接以读取下一条事件（单条返回模型）
		return m, connectSSE(*endpoint, *topicID)

	case ConnStatusMsg: // 连接状态变更
		m.connected = msg.Connected
		// 断开则触发重连
		if !msg.Connected {
			return m, reconnectCmd()
		}
	}

	return m, nil
}

// applyEvent 根据 UIEvent 类型更新模型对应字段。
// 副作用: 修改 m 的 agents/graphSteps/events/episodes/stats。
func (m *Model) applyEvent(ev types.UIEvent) {
	switch ev.Type {
	case "agent.status":
		// 更新或追加 Agent 状态
		p, _ := ev.Payload.(types.AgentStatusPayload)
		found := false
		for i := range m.agents {
			if m.agents[i].AgentID == p.AgentID {
				m.agents[i] = p // 已存在则覆盖
				found = true
				break
			}
		}
		if !found {
			m.agents = append(m.agents, p) // 新 Agent 追加
		}
	case "graph.step":
		// 追加图步骤，最多保留 50 条
		p, _ := ev.Payload.(types.GraphStepPayload)
		m.graphSteps = append(m.graphSteps, p)
		if len(m.graphSteps) > 50 {
			m.graphSteps = m.graphSteps[len(m.graphSteps)-50:]
		}
	case "workspace.event":
		// 事件插入到列表头部（最新在前）
		p, _ := ev.Payload.(types.EventPayload)
		m.events = append([]types.EventPayload{p}, m.events...)
	case "episode.new":
		// 按 agent#step 索引存储 Episode 详情
		p, _ := ev.Payload.(types.EpisodePayload)
		key := fmt.Sprintf("%s#%s", p.AgentID, p.StepID)
		m.episodes[key] = &p
	case "stats.tick":
		// 更新全局统计
		p, _ := ev.Payload.(types.StatsView)
		m.stats = p
	}
}

// View 渲染整个控制台: 标题 + 上行（agents|graph）+ 下行（events|stats）+ 帮助栏。
func (m Model) View() string {
	// 初始化阶段尚未拿到尺寸，显示占位
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	// 计算各区域高度与宽度
	topH := m.height * 3 / 5
	bottomH := m.height - topH - 1
	leftW := m.width / 3
	rightW := m.width - leftW

	// 渲染上行两面板
	agentsPanel := m.renderAgents(leftW, topH)
	graphPanel := m.renderGraphFlow(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, agentsPanel, graphPanel)

	// 渲染下行两面板
	eventsPanel := m.renderEvents(leftW, bottomH)
	statsPanel := m.renderStats(rightW, bottomH)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventsPanel, statsPanel)

	content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)
	// 开启详情时在上下行之间插入详情面板
	if m.showDetail {
		detail := m.renderEpisodeDetail(m.width, m.height/3)
		content = lipgloss.JoinVertical(lipgloss.Left, topRow, detail, bottomRow)
	}

	// 连接状态指示符: 已连接实心，未连接空心
	status := "●"
	if !m.connected {
		status = "○"
	}
	title := m.styles.Title.Render(fmt.Sprintf(" Memory Console %s │ Topic: %s ", status, *topicID))
	help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [F3]Detail [P]ause ")

	return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

// renderAgents 渲染 Agent 列表面板，按状态着色并标记光标。
func (m Model) renderAgents(w, h int) string {
	var content string
	for i, ag := range m.agents {
		// 光标标记
		cursor := "  "
		if m.focusPanel == 0 && i == m.agentCursor {
			cursor = "▸ "
		}
		// 按 State 选择样式
		var style lipgloss.Style
		switch ag.State {
		case "ACTIVE":
			style = m.styles.ActiveAgent
		case "WAITING":
			style = m.styles.WaitingAgent
		case "ERROR":
			style = m.styles.ErrorAgent
		default:
			style = m.styles.IdleAgent
		}
		line := fmt.Sprintf("%s%s %s", cursor, ag.State[:1], ag.AgentID)
		content += style.Render(line) + "\n"
	}

	// 边框: 聚焦时高亮
	border := m.styles.BlurBorder
	if m.focusPanel == 0 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderGraphFlow 渲染图步骤流面板，显示最近可容纳的步骤。
func (m Model) renderGraphFlow(w, h int) string {
	var content string
	// 计算起始下标，只显示尾部可容纳的步骤
	start := len(m.graphSteps) - h + 2
	if start < 0 {
		start = 0
	}
	for i := start; i < len(m.graphSteps); i++ {
		step := m.graphSteps[i]
		node := m.styles.StepNode.Render(step.NodeName)
		arrow := m.styles.StepArrow.Render("→")
		info := fmt.Sprintf(" %s %s", arrow, step.Duration.Round(time.Millisecond))
		if step.Action != "" {
			info += fmt.Sprintf(" [%s]", step.Action)
		}
		content += node + info + "\n"
	}

	border := m.styles.BlurBorder
	if m.focusPanel == 1 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderEvents 渲染事件列表面板，按优先级与状态着色。
func (m Model) renderEvents(w, h int) string {
	var content string
	for i, ev := range m.events {
		// 控制显示数量
		if i >= h-2 {
			break
		}
		// 光标标记
		cursor := "  "
		if m.focusPanel == 2 && i == m.eventCursor {
			cursor = "▸ "
		}
		// Pending 且高优先级用高亮，其余按状态着色
		var style lipgloss.Style
		switch ev.Status {
		case "Pending":
			if ev.Priority >= 8 {
				style = m.styles.EventHigh
			} else {
				style = m.styles.EventNormal
			}
		default:
			style = m.styles.EventDone
		}
		line := fmt.Sprintf("%s%s %s", cursor, ev.Type[:1], ev.Summary)
		content += style.Render(line) + "\n"
	}

	border := m.styles.BlurBorder
	if m.focusPanel == 2 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderStats 渲染统计面板: 私有 Episode、压缩层级、KB 命中、Token 预算、活跃 Agent。
func (m Model) renderStats(w, h int) string {
	lines := fmt.Sprintf(
		"Private Episodes: %d\nCompressed L1:    %d\nCompressed L2:    %d\nGlobal KB Hits:   %d\nToken Budget:     %d%%\nActive Agents:    %d/%d",
		m.stats.PrivateEpisodes, m.stats.CompressedL1, m.stats.CompressedL2,
		m.stats.GlobalKBHits, m.stats.TokenBudgetUsed,
		m.stats.ActiveAgents, m.stats.TotalAgents,
	)

	border := m.styles.BlurBorder
	if m.focusPanel == 3 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(lines)
}

// renderEpisodeDetail 渲染当前选中 Agent 的 Episode 详情面板。
func (m Model) renderEpisodeDetail(w, h int) string {
	// 根据聚焦面板与光标计算 Episode 键
	key := ""
	if m.focusPanel == 0 && m.agentCursor < len(m.agents) {
		ag := m.agents[m.agentCursor]
		key = fmt.Sprintf("%s#%d", ag.AgentID, ag.CurrentStep)
	}

	// 未命中则提示
	ep, ok := m.episodes[key]
	if !ok {
		return m.styles.BlurBorder.Width(w).Height(h).Render(" No episode selected ")
	}

	// 渲染详情: 步骤、重要性、时间、摘要、事实
	lines := fmt.Sprintf(
		"Step: %s  |  Importance: %.2f  |  %s\nSummary: %s\nFacts: %v",
		key, ep.Importance, ep.Timestamp.Format("15:04:05"),
		ep.Summary, ep.Facts,
	)
	return m.styles.FocusBorder.Width(w).Height(h).Render(lines)
}

// SSE 消息类型

// SSEMsg 携带一条解析成功的 UIEvent。
type SSEMsg struct {
	Event types.UIEvent
}

// ConnStatusMsg 表示 SSE 连接状态变更。
type ConnStatusMsg struct {
	Connected bool  // 是否已连接
	Err       error // 断开时的错误
}

// connectSSE 返回一个 Cmd: 发起 SSE 请求并阻塞读取，直到拿到一条事件或断开。
// 拿到事件返回 SSEMsg，断开返回 ConnStatusMsg。
func connectSSE(endpoint, topicID string) tea.Cmd {
	return func() tea.Msg {
		url := fmt.Sprintf("%s/api/tui/stream?topic_id=%s", endpoint, topicID)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return ConnStatusMsg{Connected: false, Err: err}
		}
		// SSE 标准请求头
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		// 无超时，长连接读取
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			return ConnStatusMsg{Connected: false, Err: err}
		}
		defer resp.Body.Close()

		// 逐行扫描，按 SSE 帧解析
		scanner := bufio.NewScanner(resp.Body)
		var buf bytes.Buffer

		for scanner.Scan() {
			line := scanner.Text()
			// 空行表示一帧结束，尝试解析
			if line == "" {
				data := bytes.TrimPrefix(buf.Bytes(), []byte("data: "))
				var ev types.UIEvent
				if err := json.Unmarshal(data, &ev); err == nil {
					return SSEMsg{Event: ev}
				}
				buf.Reset()
				continue
			}
			// 累积 data: 行
			if bytes.HasPrefix([]byte(line), []byte("data: ")) {
				buf.WriteString(line)
				buf.WriteByte('\n')
			}
		}

		// 扫描结束（连接断开）
		return ConnStatusMsg{Connected: false, Err: scanner.Err()}
	}
}

// reconnectCmd 返回 5 秒后重连的 Cmd。
func reconnectCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return connectSSE(*endpoint, *topicID)()
	})
}
