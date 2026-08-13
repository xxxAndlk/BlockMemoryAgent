package tui

import (
	"encoding/json"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// flowEvent 是领域进度面板卡片上的一条事件行（图标 + 摘要）。
type flowEvent struct {
	icon string
	text string
}

// flowCard 是单个领域 Agent 的进度卡片数据。
type flowCard struct {
	node   agentTreeNode
	events []flowEvent
}

// maxFlowEvents 每张卡片最多展示的最近事件数。
const maxFlowEvents = 3

// flowCardW 卡片固定总宽（含边框），保证每行卡片列数计算确定。
const flowCardW = 34

// collectFlowCards 从 Agent 树与会话事件构建领域 Agent 进度卡片（纯函数，便于测试）。
// 仅领域 Agent（roleType==RoleTypeDomain）；每卡片事件取 DetailJSON 中 agent_id
// 匹配该实例的事件尾部 maxFlowEvents 条，无匹配时回退展示名 Agent==node.name。
func collectFlowCards(nodes []agentTreeNode, events []server.SessionEvent) []flowCard {
	var cards []flowCard
	for _, n := range nodes {
		if n.roleType != enums.RoleTypeDomain {
			continue
		}
		cards = append(cards, flowCard{node: n, events: flowEventsFor(n, events)})
	}
	return cards
}

// flowEventsFor 取单个领域 Agent 实例的最近事件。
func flowEventsFor(n agentTreeNode, events []server.SessionEvent) []flowEvent {
	var matched []server.SessionEvent
	for _, ev := range events {
		if ev.DetailJSON != "" && agentIDFromDetail(ev.DetailJSON) == n.instID {
			matched = append(matched, ev)
		}
	}
	if len(matched) == 0 {
		// 旧事件/无 DetailJSON 时回退展示名匹配（固定助手展示名与树节点名一致；
		// 领域 Agent 展示名 "领域Agent:xxx" 与节点名不同，此回退主要兜底历史数据）。
		for _, ev := range events {
			if ev.Agent != "" && ev.Agent == n.name {
				matched = append(matched, ev)
			}
		}
	}
	if len(matched) > maxFlowEvents {
		matched = matched[len(matched)-maxFlowEvents:]
	}
	out := make([]flowEvent, 0, len(matched))
	for _, ev := range matched {
		if fe, ok := formatFlowEvent(n.instID, ev); ok {
			out = append(out, fe)
		}
	}
	return out
}

// agentIDFromDetail 解析事件 DetailJSON（{"agent_id":"..."}）中的实例 ID；解析失败返回空串。
func agentIDFromDetail(detailJSON string) string {
	var d struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal([]byte(detailJSON), &d); err != nil {
		return ""
	}
	return d.AgentID
}

// formatFlowEvent 把一条会话事件映射为卡片事件行；不展示的事件类型返回 ok=false。
func formatFlowEvent(instID string, ev server.SessionEvent) (flowEvent, bool) {
	switch {
	case ev.Type == "tool_exec" || ev.Kind == "tool_call":
		icon := "✓"
		if ev.Kind == "tool_call" {
			icon = "●"
		} else if !ev.Success {
			icon = "✗"
		}
		tool := ev.Tool
		if tool == "" {
			tool = "tool"
		}
		text := tool
		if p := strings.TrimSpace(ev.ToolPath); p != "" {
			text += ": " + p
		}
		return flowEvent{icon: icon, text: text}, true
	case ev.Kind == "think":
		return flowEvent{icon: "💭", text: strings.TrimSpace(ev.Message)}, true
	case ev.Kind == "llm_result" && ev.Tool == instID:
		return flowEvent{icon: "◈", text: strings.TrimSpace(ev.Message)}, true
	case ev.Kind == "sub_agent_done" && strings.TrimSpace(ev.Message) == instID:
		return flowEvent{icon: "✓", text: "执行完成"}, true
	case ev.Kind == "error" || ev.Type == "error":
		return flowEvent{icon: "✗", text: strings.TrimSpace(ev.Message)}, true
	}
	return flowEvent{}, false
}

// flowPanelCards 返回当前会话的领域 Agent 进度卡片（空会话/无领域节点时为空切片）。
func (m *Model) flowPanelCards() []flowCard {
	s := m.selectedSession()
	if s == nil {
		return nil
	}
	return collectFlowCards(m.agentTreePanel.nodes, s.Events)
}

// subAgentFlowPanelHeight 返回领域进度面板占用的行数（含标题行）：
// 无领域 Agent 时为 0（面板不渲染）；有则为 1 + 6×卡片行数（卡片=名称 1 行+事件 3 行+边框 2 行）。
// 行数按 ceil(卡片数/每行列数) 确定计算，保证 mainContentHeight 的预算与渲染一致。
func (m *Model) subAgentFlowPanelHeight() int {
	cards := m.flowPanelCards()
	if len(cards) == 0 {
		return 0
	}
	cols := flowPanelCols(m.width)
	rows := (len(cards) + cols - 1) / cols
	return 1 + 6*rows
}

// flowPanelCols 返回面板每行可容纳的卡片列数（至少 1）。
func flowPanelCols(w int) int {
	cols := w / flowCardW
	if cols < 1 {
		cols = 1
	}
	return cols
}

// renderSubAgentFlowPanel 渲染对话栏下方的领域 Agent 进度面板：
// 标题行 + 横向卡片网格（放不下换行）。每卡片：状态图标+名称、最近 3 次事件行。
func (m *Model) renderSubAgentFlowPanel(w int) string {
	cards := m.flowPanelCards()
	if len(cards) == 0 {
		return ""
	}
	headerText := "🚀 领域 Agent 进度"
	header := m.styles.PanelHeader.Width(w).Render(headerText)

	cols := flowPanelCols(w)
	rows := (len(cards) + cols - 1) / cols
	cardLines := make([]string, 0, rows)
	for r := 0; r < rows; r++ {
		start := r * cols
		end := start + cols
		if end > len(cards) {
			end = len(cards)
		}
		var rowCards []string
		for _, c := range cards[start:end] {
			rowCards = append(rowCards, m.buildFlowCard(c))
		}
		cardLines = append(cardLines, joinHorizontalWithGap(rowCards, 2))
	}
	return lipgloss.JoinVertical(lipgloss.Top, header, strings.Join(cardLines, "\n"))
}

// buildFlowCard 渲染单张领域 Agent 卡片：状态色边框，行 1=状态图标+名称，行 2-4=最近事件。
func (m *Model) buildFlowCard(c flowCard) string {
	inner := flowCardW - 4
	if inner < 4 {
		inner = 4
	}
	n := c.node
	name := lipgloss.NewStyle().Foreground(lipgloss.Color(agentRoleColor(n.roleType))).Bold(true).
		Render(statusIcon(string(n.status)) + " " + truncate(n.name, inner-3))
	lines := []string{name}
	for i := 0; i < maxFlowEvents; i++ {
		if i < len(c.events) {
			ev := c.events[i]
			lines = append(lines, ev.icon+" "+truncate(ev.text, inner-3))
		} else {
			lines = append(lines, m.styles.Dim.Render("…"))
		}
	}
	borderColor := cBlur
	switch n.status {
	case enums.RoleStatusActive:
		borderColor = cStatusRun
	case enums.RoleStatusError:
		borderColor = cStatusErr
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1).
		Width(flowCardW - 2).
		Render(strings.Join(lines, "\n"))
}
