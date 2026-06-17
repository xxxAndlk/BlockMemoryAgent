package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/blockmemory/agent/backend/pkg/types"
)

var (
	endpoint = flag.String("endpoint", "http://localhost:8080", "Backend HTTP/SSE endpoint")
	topicID  = flag.String("topic", "", "Topic ID to subscribe (required)")
	retry    = flag.String("retry", "5s", "SSE reconnection interval")
)

func main() {
	flag.Parse()
	if *topicID == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s --topic TOPIC_ID [--endpoint URL]\n", os.Args[0])
		os.Exit(1)
	}

	p := tea.NewProgram(
		initialModel(),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// Model bubbletea 模型
type Model struct {
	width  int
	height int

	agents     []types.AgentStatusPayload
	graphSteps []types.GraphStepPayload
	events     []types.EventPayload
	episodes   map[string]*types.EpisodePayload
	stats      types.StatsView

	focusPanel  int // 0: agents, 1: graph, 2: events, 3: stats
	agentCursor int
	eventCursor int
	showDetail  bool
	paused      bool
	connected   bool

	styles *Styles
}

type Styles struct {
	Title        lipgloss.Style
	ActiveAgent  lipgloss.Style
	WaitingAgent lipgloss.Style
	IdleAgent    lipgloss.Style
	ErrorAgent   lipgloss.Style
	FocusBorder  lipgloss.Style
	BlurBorder   lipgloss.Style
	EventHigh    lipgloss.Style
	EventNormal  lipgloss.Style
	EventDone    lipgloss.Style
	StepNode     lipgloss.Style
	StepArrow    lipgloss.Style
	HelpBar      lipgloss.Style
}

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

func initialModel() Model {
	return Model{
		episodes: make(map[string]*types.EpisodePayload),
		styles:   NewStyles(),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		connectSSE(*endpoint, *topicID),
	)
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
			if m.focusPanel == 0 && m.agentCursor > 0 {
				m.agentCursor--
			} else if m.focusPanel == 2 && m.eventCursor > 0 {
				m.eventCursor--
			}
		case "down":
			if m.focusPanel == 0 && m.agentCursor < len(m.agents)-1 {
				m.agentCursor++
			} else if m.focusPanel == 2 && m.eventCursor < len(m.events)-1 {
				m.eventCursor++
			}
		case "f3":
			m.showDetail = !m.showDetail
		case "p":
			m.paused = !m.paused
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case SSEMsg:
		m.applyEvent(msg.Event)
		return m, connectSSE(*endpoint, *topicID)

	case ConnStatusMsg:
		m.connected = msg.Connected
		if !msg.Connected {
			return m, reconnectCmd()
		}
	}

	return m, nil
}

func (m *Model) applyEvent(ev types.UIEvent) {
	switch ev.Type {
	case "agent.status":
		p, _ := ev.Payload.(types.AgentStatusPayload)
		found := false
		for i := range m.agents {
			if m.agents[i].AgentID == p.AgentID {
				m.agents[i] = p
				found = true
				break
			}
		}
		if !found {
			m.agents = append(m.agents, p)
		}
	case "graph.step":
		p, _ := ev.Payload.(types.GraphStepPayload)
		m.graphSteps = append(m.graphSteps, p)
		if len(m.graphSteps) > 50 {
			m.graphSteps = m.graphSteps[len(m.graphSteps)-50:]
		}
	case "workspace.event":
		p, _ := ev.Payload.(types.EventPayload)
		m.events = append([]types.EventPayload{p}, m.events...)
	case "episode.new":
		p, _ := ev.Payload.(types.EpisodePayload)
		key := fmt.Sprintf("%s#%s", p.AgentID, p.StepID)
		m.episodes[key] = &p
	case "stats.tick":
		p, _ := ev.Payload.(types.StatsView)
		m.stats = p
	}
}

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing..."
	}

	topH := m.height * 3 / 5
	bottomH := m.height - topH - 1
	leftW := m.width / 3
	rightW := m.width - leftW

	agentsPanel := m.renderAgents(leftW, topH)
	graphPanel := m.renderGraphFlow(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, agentsPanel, graphPanel)

	eventsPanel := m.renderEvents(leftW, bottomH)
	statsPanel := m.renderStats(rightW, bottomH)
	bottomRow := lipgloss.JoinHorizontal(lipgloss.Top, eventsPanel, statsPanel)

	content := lipgloss.JoinVertical(lipgloss.Left, topRow, bottomRow)
	if m.showDetail {
		detail := m.renderEpisodeDetail(m.width, m.height/3)
		content = lipgloss.JoinVertical(lipgloss.Left, topRow, detail, bottomRow)
	}

	status := "●"
	if !m.connected {
		status = "○"
	}
	title := m.styles.Title.Render(fmt.Sprintf(" Memory Console %s │ Topic: %s ", status, *topicID))
	help := m.styles.HelpBar.Render(" [Q]uit [Tab]Focus [↑↓]Navigate [F3]Detail [P]ause ")

	return lipgloss.JoinVertical(lipgloss.Left, title, content, help)
}

func (m Model) renderAgents(w, h int) string {
	var content string
	for i, ag := range m.agents {
		cursor := "  "
		if m.focusPanel == 0 && i == m.agentCursor {
			cursor = "▸ "
		}
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

	border := m.styles.BlurBorder
	if m.focusPanel == 0 {
		border = m.styles.FocusBorder
	}
	return border.Width(w).Height(h).Render(content)
}

func (m Model) renderGraphFlow(w, h int) string {
	var content string
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

func (m Model) renderEvents(w, h int) string {
	var content string
	for i, ev := range m.events {
		if i >= h-2 {
			break
		}
		cursor := "  "
		if m.focusPanel == 2 && i == m.eventCursor {
			cursor = "▸ "
		}
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

func (m Model) renderEpisodeDetail(w, h int) string {
	key := ""
	if m.focusPanel == 0 && m.agentCursor < len(m.agents) {
		ag := m.agents[m.agentCursor]
		key = fmt.Sprintf("%s#%d", ag.AgentID, ag.CurrentStep)
	}

	ep, ok := m.episodes[key]
	if !ok {
		return m.styles.BlurBorder.Width(w).Height(h).Render(" No episode selected ")
	}

	lines := fmt.Sprintf(
		"Step: %s  |  Importance: %.2f  |  %s\nSummary: %s\nFacts: %v",
		key, ep.Importance, ep.Timestamp.Format("15:04:05"),
		ep.Summary, ep.Facts,
	)
	return m.styles.FocusBorder.Width(w).Height(h).Render(lines)
}

// SSE 消息类型
type SSEMsg struct {
	Event types.UIEvent
}

type ConnStatusMsg struct {
	Connected bool
	Err       error
}

func connectSSE(endpoint, topicID string) tea.Cmd {
	return func() tea.Msg {
		url := fmt.Sprintf("%s/api/tui/stream?topic_id=%s", endpoint, topicID)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return ConnStatusMsg{Connected: false, Err: err}
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")

		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			return ConnStatusMsg{Connected: false, Err: err}
		}
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		var buf bytes.Buffer

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				data := bytes.TrimPrefix(buf.Bytes(), []byte("data: "))
				var ev types.UIEvent
				if err := json.Unmarshal(data, &ev); err == nil {
					return SSEMsg{Event: ev}
				}
				buf.Reset()
				continue
			}
			if bytes.HasPrefix([]byte(line), []byte("data: ")) {
				buf.WriteString(line)
				buf.WriteByte('\n')
			}
		}

		return ConnStatusMsg{Connected: false, Err: scanner.Err()}
	}
}

func reconnectCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return connectSSE(*endpoint, *topicID)()
	})
}
