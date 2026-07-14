package main

// cmd/memory-console 是记忆检查控制台的可执行文件入口。
// 它通过 Server-Sent Events (SSE) 订阅后端事件流，在终端实时展示：
//   - Agent 状态列表
//   - 图步骤（graph step）执行流
//   - 工作区事件
//   - 全局统计指标
//   - 当前选中 Agent/Step 对应的 Episode 详情
// 控制台基于 bubbletea 框架实现，使用 lipgloss 进行终端样式渲染。

import (
	"bufio"         // bufio.Scanner 用于按行扫描 SSE 响应体
	"bytes"         // bytes.Buffer 与 HasPrefix/TrimPrefix 用于处理 data: 行
	"encoding/json" // 解析 SSE 事件 JSON 负载
	"flag"          // 命令行参数解析
	"fmt"           // 格式化输出
	"net/http"      // SSE 客户端：构造请求、发送请求、读取响应
	"os"            // 标准错误输出与进程退出
	"time"          // 时长格式化与重连间隔

	"github.com/blockmemory/agent/backend/pkg/types" // 公共事件类型定义（UIEvent 等）
	tea "github.com/charmbracelet/bubbletea"         // TUI 框架，提供 Model-Update-View 循环
	"github.com/charmbracelet/lipgloss"              // 终端样式渲染库
)

// ---- 命令行参数 ----
// endpoint 指定后端 HTTP/SSE 端点地址，默认本地 8080 端口。
// topicID 指定要订阅的 Topic ID，程序运行必需，缺失会打印用法并退出。
// retry 指定 SSE 断开后重连的等待间隔，默认 5 秒。
var (
	endpoint = flag.String("endpoint", "http://localhost:8080", "Backend HTTP/SSE endpoint")
	topicID  = flag.String("topic", "", "Topic ID to subscribe (required)")
	retry    = flag.String("retry", "5s", "SSE reconnection interval")
)

// main 是控制台入口函数：
//  1. 解析命令行参数；
//  2. 校验 topicID 必填；
//  3. 创建 bubbletea 程序并进入全屏 alt-screen 模式；
//  4. 运行事件循环，出错时打印到 stderr 并退出。
func main() {
	flag.Parse() // 解析 os.Args 中的命令行参数

	// topicID 为必填项，缺失则无法建立 SSE 订阅，直接打印用法并退出
	if *topicID == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s --topic TOPIC_ID [--endpoint URL]\n", os.Args[0])
		os.Exit(1)
	}

	// 创建 bubbletea 程序，使用 alt screen 全屏模式，
	// 避免日志或滚动条污染终端主屏幕。
	p := tea.NewProgram(
		initialModel(),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		// 运行期错误（如终端不支持、SSE 连接失败等）输出到 stderr
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// ---- bubbletea 模型分割线 ----

// Model 是 bubbletea 框架的核心模型，持有 SSE 推送来的各类数据与 TUI 交互状态。
// 它实现 tea.Model 接口（Init / Update / View）。
type Model struct {
	width  int // 终端当前宽度（字符列数），用于布局
	height int // 终端当前高度（字符行数），用于布局

	agents     []types.AgentStatusPayload       // Agent 状态列表，按 AgentID 去重/更新
	graphSteps []types.GraphStepPayload         // 图步骤历史，最多保留 50 条
	events     []types.EventPayload             // 工作区事件列表，最新事件位于头部
	episodes   map[string]*types.EpisodePayload // Episode 详情映射，键为 agent#step
	stats      types.StatsView                  // 全局统计视图

	focusPanel  int  // 当前聚焦面板：0 agents, 1 graph, 2 events, 3 stats
	agentCursor int  // Agent 列表中的光标下标
	eventCursor int  // 事件列表中的光标下标
	showDetail  bool // 是否显示 Episode 详情浮层
	paused      bool // 是否暂停自动滚动/更新（当前为预留开关）
	connected   bool // SSE 连接是否处于连通状态

	styles *Styles // 样式集合指针，避免每次 View 重复构造样式
}

// Styles 集中管理控制台各 UI 元素的 lipgloss 样式。
// 使用结构体缓存样式对象，避免每次渲染重新计算样式，提升 TUI 性能。
type Styles struct {
	Title        lipgloss.Style // 顶部标题栏样式
	ActiveAgent  lipgloss.Style // 活跃 Agent 文本颜色
	WaitingAgent lipgloss.Style // 等待中 Agent 文本颜色
	IdleAgent    lipgloss.Style // 空闲 Agent 文本颜色
	ErrorAgent   lipgloss.Style // 错误 Agent 文本颜色
	FocusBorder  lipgloss.Style // 聚焦面板边框样式
	BlurBorder   lipgloss.Style // 非聚焦面板边框样式
	EventHigh    lipgloss.Style // 高优先级事件文本颜色
	EventNormal  lipgloss.Style // 普通优先级事件文本颜色
	EventDone    lipgloss.Style // 已完成事件文本颜色
	StepNode     lipgloss.Style // 步骤节点徽标样式
	StepArrow    lipgloss.Style // 步骤箭头样式
	HelpBar      lipgloss.Style // 底部帮助栏样式
}

// NewStyles 构造并返回默认样式集合。
// 颜色方案基于 Tokyo Night 调色板，在深色终端下对比度较好。
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

// initialModel 构造初始 Model：
//   - 初始化 episodes map，避免后续写入时 nil map  panic；
//   - 初始化 styles，保证 View 渲染时不访问空指针。
func initialModel() Model {
	return Model{
		episodes: make(map[string]*types.EpisodePayload),
		styles:   NewStyles(),
	}
}

// Init 是 bubbletea 启动时调用的初始化函数，负责发起首次 SSE 连接。
// 返回 tea.Cmd，由框架在第一次渲染前执行。
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		connectSSE(*endpoint, *topicID), // 发起 SSE 订阅请求
	)
}

// Update 是 bubbletea 每帧调用的更新函数，处理：
//   - 键盘输入（退出、切换焦点、移动光标、切换详情、暂停）；
//   - 窗口尺寸变化；
//   - SSE 收到新事件；
//   - SSE 连接状态变化（断线重连）。
//
// 参数 msg 是框架或自定义消息；返回更新后的模型与下一个要执行的命令。
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// 根据消息类型分发处理
	switch msg := msg.(type) {
	case tea.KeyMsg: // 键盘输入消息
		// 根据按键字符串进一步分发
		switch msg.String() {
		case "q", "ctrl+c": // 用户请求退出
			return m, tea.Quit
		case "tab": // 正向切换聚焦面板（0→1→2→3→0）
			m.focusPanel = (m.focusPanel + 1) % 4
		case "shift+tab": // 反向切换聚焦面板，+4 避免负数取模
			m.focusPanel = (m.focusPanel - 1 + 4) % 4
		case "up": // 光标上移，仅在 Agent 面板或事件面板生效
			if m.focusPanel == 0 && m.agentCursor > 0 {
				// Agent 面板：光标上边界保护
				m.agentCursor--
			} else if m.focusPanel == 2 && m.eventCursor > 0 {
				// 事件面板：光标上边界保护
				m.eventCursor--
			}
		case "down": // 光标下移，带下边界保护，避免越界
			if m.focusPanel == 0 && m.agentCursor < len(m.agents)-1 {
				m.agentCursor++
			} else if m.focusPanel == 2 && m.eventCursor < len(m.events)-1 {
				m.eventCursor++
			}
		case "f3": // F3 切换详情显示/隐藏
			m.showDetail = !m.showDetail
		case "p": // P 切换暂停状态（预留，当前不影响渲染）
			m.paused = !m.paused
		}

	case tea.WindowSizeMsg: // 终端窗口尺寸变化
		// 更新模型中的宽高，View 在下一帧使用新尺寸重新布局
		m.width = msg.Width
		m.height = msg.Height

	case SSEMsg: // 自定义消息：收到一条 SSE 事件
		// 将事件应用到模型，更新对应字段
		m.applyEvent(msg.Event)
		// 当前实现采用"单条返回"模型：每收到一条事件就重新发起 SSE 连接读取下一条
		return m, connectSSE(*endpoint, *topicID)

	case ConnStatusMsg: // 自定义消息：连接状态变更
		// 更新连接指示符状态
		m.connected = msg.Connected
		// 如果连接断开，触发重连命令，避免事件流永久中断
		if !msg.Connected {
			return m, reconnectCmd()
		}
	}

	// 默认返回模型与 nil 命令，等待下一个消息
	return m, nil
}

// applyEvent 根据 UIEvent 的类型更新模型对应字段。
// 参数 ev 是后端推送的 UI 事件；方法会修改 m 的 agents/graphSteps/events/episodes/stats。
func (m *Model) applyEvent(ev types.UIEvent) {
	// 按事件类型分发处理
	switch ev.Type {
	case "agent.status":
		// 更新或追加 Agent 状态：先查找是否已存在同 AgentID，存在则覆盖，否则追加
		p, _ := ev.Payload.(types.AgentStatusPayload)
		found := false
		for i := range m.agents {
			if m.agents[i].AgentID == p.AgentID {
				// 命中：用最新状态覆盖旧状态
				m.agents[i] = p
				found = true
				break // 找到即结束循环
			}
		}
		if !found {
			// 未命中：作为新 Agent 追加到列表尾部
			m.agents = append(m.agents, p)
		}
	case "graph.step":
		// 追加图步骤；为控制内存与渲染高度，最多保留最近 50 条
		p, _ := ev.Payload.(types.GraphStepPayload)
		m.graphSteps = append(m.graphSteps, p)
		if len(m.graphSteps) > 50 {
			// 超出上限时只保留尾部 50 条
			m.graphSteps = m.graphSteps[len(m.graphSteps)-50:]
		}
	case "workspace.event":
		// 事件插入到列表头部，保证最新事件显示在最上方
		p, _ := ev.Payload.(types.EventPayload)
		m.events = append([]types.EventPayload{p}, m.events...)
	case "episode.new":
		// 按 agent#step 索引存储 Episode 详情，便于详情面板快速查找
		p, _ := ev.Payload.(types.EpisodePayload)
		key := fmt.Sprintf("%s#%s", p.AgentID, p.StepID)
		m.episodes[key] = &p
	case "stats.tick":
		// 更新全局统计视图
		p, _ := ev.Payload.(types.StatsView)
		m.stats = p
	}
}

// View 渲染整个控制台界面：
//   - 顶部标题栏（含连接状态与 Topic）；
//   - 上行两面板：Agent 列表 | Graph 步骤流；
//   - 可选 Episode 详情浮层；
//   - 下行两面板：事件列表 | 统计；
//   - 底部帮助栏。
//
// 返回最终要输出到终端的字符串。
func (m Model) View() string {
	// 初始化阶段尚未收到 WindowSizeMsg，宽高为 0，显示占位文本避免 panic
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	// 按 3:2 比例分配上下区域高度，底部留 1 行给帮助栏
	topH := m.height * 3 / 5
	bottomH := m.height - topH - 1
	// 按 1:2 比例分配左右区域宽度
	leftW := m.width / 3
	rightW := m.width - leftW

	// 渲染上行两个面板
	agentsPanel := m.renderAgents(leftW, topH)
	graphPanel := m.renderGraphFlow(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, agentsPanel, graphPanel)

	// 渲染下行两个面板
	eventsPanel := m.renderEvents(leftW, bottomH)
	statsPanel := m.renderStats(rightW, bottomH)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventsPanel, statsPanel)

	// 组合主要内容区域
	content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)
	// 如果开启详情浮层，在上下行之间插入 Episode 详情面板
	if m.showDetail {
		detail := m.renderEpisodeDetail(m.width, m.height/3)
		content = lipgloss.JoinVertical(lipgloss.Left, topRow, detail, bottomRow)
	}

	// 连接状态指示符：已连接显示实心圆，未连接显示空心圆
	status := "●"
	if !m.connected {
		status = "○"
	}
	// 渲染标题栏：包含连接状态与当前订阅 Topic
	title := m.styles.Title.Render(fmt.Sprintf(" Memory Console %s │ Topic: %s ", status, *topicID))
	// 渲染底部帮助栏，提示可用按键
	help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [F3]Detail [P]ause ")

	// 垂直组合标题、内容、帮助栏
	return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

// renderAgents 渲染 Agent 列表面板。
// 参数 w、h 分别指定面板宽度和高度；返回带边框的渲染字符串。
func (m Model) renderAgents(w, h int) string {
	var content string
	// 遍历所有 Agent，按状态着色并标记光标
	for i, ag := range m.agents {
		// 默认无光标标记
		cursor := "  "
		if m.focusPanel == 0 && i == m.agentCursor {
			// 当前聚焦面板且光标命中此 Agent，显示箭头标记
			cursor = "▸ "
		}
		// 根据 State 选择对应样式
		var style lipgloss.Style
		switch ag.State {
		case "ACTIVE":
			style = m.styles.ActiveAgent
		case "WAITING":
			style = m.styles.WaitingAgent
		case "ERROR":
			style = m.styles.ErrorAgent
		default:
			// 未匹配状态统一视为空闲
			style = m.styles.IdleAgent
		}
		// 行格式：光标 + 状态首字母 + AgentID
		line := fmt.Sprintf("%s%s %s", cursor, ag.State[:1], ag.AgentID)
		content += style.Render(line) + "\n"
	}

	// 边框：聚焦时高亮，否则使用暗淡边框
	border := m.styles.BlurBorder
	if m.focusPanel == 0 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderGraphFlow 渲染图步骤流面板，显示最近可容纳的步骤。
// 参数 w、h 分别指定面板宽度和高度。
func (m Model) renderGraphFlow(w, h int) string {
	var content string
	// 计算起始下标，只显示尾部能容纳 h-2 行的步骤（预留边框占行）
	start := len(m.graphSteps) - h + 2
	if start < 0 {
		// 步骤数不足一屏时从 0 开始
		start = 0
	}
	// 遍历可见范围的步骤
	for i := start; i < len(m.graphSteps); i++ {
		step := m.graphSteps[i]
		// 渲染步骤节点徽标
		node := m.styles.StepNode.Render(step.NodeName)
		// 渲染连接箭头
		arrow := m.styles.StepArrow.Render("→")
		// 构造耗时与动作信息
		info := fmt.Sprintf(" %s %s", arrow, step.Duration.Round(time.Millisecond))
		if step.Action != "" {
			// 若存在动作名，追加显示
			info += fmt.Sprintf(" [%s]", step.Action)
		}
		content += node + info + "\n"
	}

	// 边框：聚焦时高亮
	border := m.styles.BlurBorder
	if m.focusPanel == 1 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderEvents 渲染事件列表面板，按优先级与状态着色。
// 参数 w、h 分别指定面板宽度和高度。
func (m Model) renderEvents(w, h int) string {
	var content string
	// 遍历事件列表，最多显示 h-2 行
	for i, ev := range m.events {
		// 控制显示数量，避免超出面板高度
		if i >= h-2 {
			break
		}
		// 默认无光标标记
		cursor := "  "
		if m.focusPanel == 2 && i == m.eventCursor {
			// 当前聚焦面板且光标命中此事件
			cursor = "▸ "
		}
		// Pending 状态按优先级着色，其他状态视为已完成
		var style lipgloss.Style
		switch ev.Status {
		case "Pending":
			if ev.Priority >= 8 {
				// 高优先级事件使用醒目颜色
				style = m.styles.EventHigh
			} else {
				// 普通优先级事件使用中等颜色
				style = m.styles.EventNormal
			}
		default:
			// 非 Pending 状态（完成/取消等）使用完成色
			style = m.styles.EventDone
		}
		// 行格式：光标 + 事件类型首字母 + 摘要
		line := fmt.Sprintf("%s%s %s", cursor, ev.Type[:1], ev.Summary)
		content += style.Render(line) + "\n"
	}

	// 边框：聚焦时高亮
	border := m.styles.BlurBorder
	if m.focusPanel == 2 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

// renderStats 渲染统计面板，显示：
//   - 私有 Episode 数量
//   - 压缩层级（Raw / Standard）
//   - 全局 KB 命中次数
//   - Token 预算使用百分比
//   - 活跃/总 Agent 数
//
// 参数 w、h 分别指定面板宽度和高度。
func (m Model) renderStats(w, h int) string {
	lines := fmt.Sprintf(
		"Private Episodes: %d\nCompressed Raw:   %d\nCompressed Std:   %d\nGlobal KB Hits:   %d\nToken Budget:     %d%%\nActive Agents:    %d/%d",
		m.stats.PrivateEpisodes, m.stats.CompressedRaw, m.stats.CompressedStandard,
		m.stats.GlobalKBHits, m.stats.TokenBudgetUsed,
		m.stats.ActiveAgents, m.stats.TotalAgents,
	)

	// 边框：聚焦时高亮
	border := m.styles.BlurBorder
	if m.focusPanel == 3 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(lines)
}

// renderEpisodeDetail 渲染当前选中 Agent 的 Episode 详情面板。
// 参数 w、h 分别指定面板宽度和高度。
func (m Model) renderEpisodeDetail(w, h int) string {
	// 根据当前聚焦面板与光标计算 Episode 查找键
	key := ""
	if m.focusPanel == 0 && m.agentCursor < len(m.agents) {
		// Agent 面板被选中时，使用当前 AgentID + CurrentStep 组合键
		ag := m.agents[m.agentCursor]
		key = fmt.Sprintf("%s#%d", ag.AgentID, ag.CurrentStep)
	}

	// 在 episodes 映射中查找对应 Episode
	ep, ok := m.episodes[key]
	if !ok {
		// 未命中则显示提示信息，保持面板高度稳定
		return m.styles.BlurBorder.Width(w).Height(h).Render(" No episode selected ")
	}

	// 渲染详情：步骤键、重要性、时间戳、摘要、事实列表
	lines := fmt.Sprintf(
		"Step: %s  |  Importance: %.2f  |  %s\nSummary: %s\nFacts: %v",
		key, ep.Importance, ep.Timestamp.Format("15:04:05"),
		ep.Summary, ep.Facts,
	)
	return m.styles.FocusBorder.Width(w).Height(h).Render(lines)
}

// ---- SSE 消息类型 ----

// SSEMsg 是自定义 bubbletea 消息，携带一条解析成功的 UIEvent。
type SSEMsg struct {
	Event types.UIEvent // 后端推送的 UI 事件
}

// ConnStatusMsg 是自定义 bubbletea 消息，表示 SSE 连接状态变更。
type ConnStatusMsg struct {
	Connected bool  // true 表示已连接，false 表示断开
	Err       error // 断开时的错误，可能为 nil
}

// connectSSE 返回一个 bubbletea Cmd：
// 它会发起 SSE GET 请求并阻塞读取响应体，直到拿到一条完整事件帧或连接断开。
// 拿到事件返回 SSEMsg；连接断开返回 ConnStatusMsg，由 Update 触发重连。
// 参数 endpoint 是后端地址；topicID 是订阅主题。
func connectSSE(endpoint, topicID string) tea.Cmd {
	return func() tea.Msg {
		// 构造 SSE 订阅 URL，topic_id 作为查询参数
		url := fmt.Sprintf("%s/api/tui/stream?topic_id=%s", endpoint, topicID)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			// 请求构造失败（如非法 URL）直接返回断连状态
			return ConnStatusMsg{Connected: false, Err: err}
		}
		// SSE 标准请求头：声明接受事件流并禁用缓存
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		// 无超时客户端：SSE 是长连接，不能设置整体超时
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			// 网络层连接失败
			return ConnStatusMsg{Connected: false, Err: err}
		}
		defer resp.Body.Close() // 函数退出时关闭响应体

		// 使用 bufio.Scanner 按行扫描 SSE 响应体
		scanner := bufio.NewScanner(resp.Body)
		var buf bytes.Buffer // 累积 data: 行内容

		// 循环读取每一行，直到连接断开或解析出事件
		for scanner.Scan() {
			line := scanner.Text()
			// SSE 协议中，空行表示一帧结束
			if line == "" {
				// 去掉 "data: " 前缀，解析 JSON 事件
				data := bytes.TrimPrefix(buf.Bytes(), []byte("data: "))
				var ev types.UIEvent
				if err := json.Unmarshal(data, &ev); err == nil {
					// 解析成功：返回事件消息，由 Update 更新模型
					return SSEMsg{Event: ev}
				}
				// 解析失败：清空缓冲区，继续读取下一帧
				buf.Reset()
				continue
			}
			// 累积以 "data: " 开头的行
			if bytes.HasPrefix([]byte(line), []byte("data: ")) {
				buf.WriteString(line)
				buf.WriteByte('\n')
			}
		}

		// 扫描结束，说明连接已断开；返回断连状态，触发重连
		return ConnStatusMsg{Connected: false, Err: scanner.Err()}
	}
}

// reconnectCmd 返回一个在 5 秒后重新发起 SSE 连接的 Cmd。
// 使用 tea.Tick 实现延迟，避免断线后无限快速重试导致 CPU/日志风暴。
func reconnectCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		// 时间到达后执行 connectSSE 并立即返回其 tea.Msg
		return connectSSE(*endpoint, *topicID)()
	})
}
