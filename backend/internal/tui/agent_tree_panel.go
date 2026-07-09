package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// agentTreeNode is one flattened row in the agent topology.
type agentTreeNode struct {
	depth     int
	instID    string
	name      string
	domain    string
	roleType  enums.RoleType
	status    enums.RoleStatus
	goal      string
	isClarify bool
	createdAt time.Time
}

// AgentTreePanel owns the flattened agent tree rendering state built from
// agent.Agent data.
type AgentTreePanel struct {
	nodes []agentTreeNode
}

// NewAgentTreePanel constructs an empty AgentTreePanel.
func NewAgentTreePanel() AgentTreePanel {
	return AgentTreePanel{}
}

func (at *AgentTreePanel) rebuild(agentFacade agent.Agent, s *server.Session) {
	at.nodes = nil
	if s == nil || agentFacade == nil {
		return
	}

	metaStatus := enums.RoleStatusIdle
	switch s.Status {
	case "running":
		metaStatus = enums.RoleStatusActive
	case "completed":
		metaStatus = enums.RoleStatusDone
	case "error":
		metaStatus = enums.RoleStatusError
	case "awaiting_clarify":
		metaStatus = enums.RoleStatusWaiting
	}
	metaGoal := ""
	if s.Goal != "" {
		metaGoal = s.Goal
	} else if len(s.Messages) > 0 {
		for _, msg := range s.Messages {
			if msg.Role == enums.ChatRoleUser {
				metaGoal = strings.TrimSpace(msg.Content)
				break
			}
		}
	}
	at.nodes = append(at.nodes, agentTreeNode{
		depth:     0,
		instID:    "MetaAgent",
		name:      "MetaAgent",
		roleType:  enums.RoleTypeMeta,
		status:    metaStatus,
		goal:      metaGoal,
		createdAt: s.StartedAt,
	})

	insts, err := agentFacade.ListAgents(context.Background(), s.ID)
	if err != nil {
		return
	}
	byID := make(map[string]agent.AgentInstance)
	for _, inst := range insts {
		byID[inst.ModuleID] = inst
	}
	for _, inst := range insts {
		if inst.RoleType != enums.RoleTypeDomain {
			continue
		}
		goal := ""
		if s.State != nil {
			for _, b := range s.State.ActiveBlocks {
				if b.Domain == inst.Domain {
					goal = b.Goal
					break
				}
			}
		}
		at.nodes = append(at.nodes, agentTreeNode{
			depth:     1,
			instID:    inst.ModuleID,
			name:      inst.Name,
			domain:    inst.Domain,
			roleType:  inst.RoleType,
			status:    enums.RoleStatus(inst.Status),
			goal:      goal,
			createdAt: inst.CreatedAt,
		})
		for _, childID := range inst.Children {
			child := byID[childID]
			if child.ModuleID == "" {
				continue
			}
			depth := 2
			if child.RoleType == enums.RoleTypeSubDomain {
				at.nodes = append(at.nodes, agentTreeNode{
					depth:     depth,
					instID:    child.ModuleID,
					name:      child.Name,
					domain:    child.Domain,
					roleType:  child.RoleType,
					status:    enums.RoleStatus(child.Status),
					createdAt: child.CreatedAt,
				})
				for _, subID := range child.Children {
					sub := byID[subID]
					if sub.ModuleID == "" {
						continue
					}
					at.nodes = append(at.nodes, agentTreeNode{
						depth:     3,
						instID:    sub.ModuleID,
						name:      sub.Name,
						domain:    sub.Domain,
						roleType:  sub.RoleType,
						status:    enums.RoleStatus(sub.Status),
						createdAt: sub.CreatedAt,
					})
				}
			} else if child.RoleType == enums.RoleTypeFixed || child.RoleType == enums.RoleTypeDynamic {
				at.nodes = append(at.nodes, agentTreeNode{
					depth:     depth,
					instID:    child.ModuleID,
					name:      child.Name,
					domain:    child.Domain,
					roleType:  child.RoleType,
					status:    enums.RoleStatus(child.Status),
					createdAt: child.CreatedAt,
				})
			}
		}
	}

	if s.State != nil && s.State.PendingClarify != nil {
		at.nodes = append(at.nodes, agentTreeNode{
			depth:     1,
			instID:    "clarify",
			name:      "Clarify pending",
			roleType:  enums.RoleTypeMeta,
			status:    enums.RoleStatusWaiting,
			isClarify: true,
		})
	}
}

func (at *AgentTreePanel) deriveDomainTaskStatuses() map[string]board.TaskStatus {
	status := make(map[string]board.TaskStatus)
	for _, node := range at.nodes {
		if node.domain == "" {
			continue
		}
		var st board.TaskStatus
		switch node.status {
		case enums.RoleStatusError:
			st = board.TaskFailed
		case enums.RoleStatusActive:
			st = board.TaskInProgress
		case enums.RoleStatusDone:
			st = board.TaskDone
		default:
			st = board.TaskPending
		}
		cur := status[node.domain]
		status[node.domain] = strongerTaskStatus(cur, st)
	}
	return status
}

func (at *AgentTreePanel) buildLines() []string {
	if len(at.nodes) == 0 {
		return []string{"(no agents)"}
	}
	var lines []string
	for _, node := range at.nodes {
		prefix := strings.Repeat("  ", node.depth)
		var icon string
		switch node.roleType {
		case enums.RoleTypeMeta:
			icon = "◆"
		case enums.RoleTypeDomain:
			icon = "◆"
		case enums.RoleTypeSubDomain:
			icon = "◇"
		default:
			icon = "▸"
		}
		name := node.name
		if node.goal != "" {
			name += " — " + truncate(node.goal, 40)
		}
		if node.isClarify {
			name = "Clarify pending"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", " ", prefix, icon, name, statusIcon(string(node.status))))
	}
	return lines
}

func (m Model) renderAgentsPanel(w, h int) string {
	if w < 20 {
		w = 20
	}

	titleLeft := "Agent 编排"
	titleRight := "[A] 关闭"
	titlePadding := w - lipgloss.Width(titleLeft) - lipgloss.Width(titleRight) - 2
	if titlePadding < 1 {
		titlePadding = 1
	}
	headerText := titleLeft + strings.Repeat(" ", titlePadding) + titleRight
	header := m.styles.PanelHeader.Width(w).Render(headerText)
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}

	var lines []string
	if len(m.agentTreePanel.nodes) == 0 {
		lines = append(lines, "(no agents)")
	} else {
		for i, node := range m.agentTreePanel.nodes {
			if node.depth >= 2 && node.goal == "" &&
				(node.status == enums.RoleStatusDone || node.status == enums.RoleStatusIdle) {
				continue
			}
			card := m.agentCardLine(node, i, innerW)
			lines = append(lines, card...)
		}
	}

	body := strings.Join(lines, "\n")
	return lipgloss.JoinVertical(lipgloss.Top, header, m.styles.PanelBox.Width(w-2).Height(h-3).Render(body))
}

// agentCardLine renders a single Agent card row, returning one or two lines.
func (m Model) agentCardLine(node agentTreeNode, idx, innerW int) []string {
	prefix := agentTreePrefix(m.agentTreePanel.nodes, idx)
	prefixW := runewidth.StringWidth(prefix)
	avail := innerW - prefixW
	if avail < 10 {
		avail = 10
	}

	icon := statusIcon(string(node.status))
	iconStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor(string(node.status)))).Render(icon)
	nameColor := agentRoleColor(node.roleType)
	nameStyled := lipgloss.NewStyle().Foreground(lipgloss.Color(nameColor)).Bold(true).Render(node.name)
	badge := agentStatusBadge(m.styles, node.status)

	ts := ""
	if !node.createdAt.IsZero() {
		ts = node.createdAt.Format("15:04:05")
	}
	tsStyled := m.styles.Dim.Render(ts)

	line := fmt.Sprintf("%s%s %s %s", prefix, iconStyled, nameStyled, badge)
	if ts != "" {
		gap := avail - lipgloss.Width(line) + prefixW - lipgloss.Width(tsStyled)
		if gap < 1 {
			gap = 1
		}
		line += strings.Repeat(" ", gap) + tsStyled
	}
	line = truncate(line, innerW)

	lines := []string{line}

	if node.goal != "" && !node.isClarify {
		goal := strings.ReplaceAll(node.goal, "\n", " ")
		goalPrefix := strings.Repeat(" ", prefixW) + "  "
		goalAvail := innerW - runewidth.StringWidth(goalPrefix)
		if goalAvail < 10 {
			goalAvail = 10
		}
		goalLine := goalPrefix + m.styles.Dim.Render(truncate(goal, goalAvail))
		lines = append(lines, goalLine)
	}

	return lines
}

// agentTreePrefix generates tree-drawing prefix characters based on node depth
// and sibling relationships.
func agentTreePrefix(nodes []agentTreeNode, idx int) string {
	if idx < 0 || idx >= len(nodes) {
		return ""
	}
	depth := nodes[idx].depth
	if depth == 0 {
		return ""
	}

	var parts []string
	for d := 1; d <= depth; d++ {
		ancestorIdx := -1
		for j := idx; j >= 0; j-- {
			if nodes[j].depth == d {
				ancestorIdx = j
				break
			}
		}
		if ancestorIdx == -1 {
			parts = append(parts, "   ")
			continue
		}
		isLast := true
		for j := ancestorIdx + 1; j < len(nodes); j++ {
			if nodes[j].depth < d {
				break
			}
			if nodes[j].depth == d {
				isLast = false
				break
			}
		}
		if d == depth {
			if isLast {
				parts = append(parts, "└─ ")
			} else {
				parts = append(parts, "├─ ")
			}
		} else {
			if isLast {
				parts = append(parts, "   ")
			} else {
				parts = append(parts, "│  ")
			}
		}
	}
	return strings.Join(parts, "")
}

func agentRoleColor(roleType enums.RoleType) string {
	switch roleType {
	case enums.RoleTypeMeta:
		return cMeta
	case enums.RoleTypeDomain:
		return cDomain
	case enums.RoleTypeSubDomain:
		return cSub
	case enums.RoleTypeFixed, enums.RoleTypeDynamic:
		return cAssist
	default:
		return cInfo
	}
}

func agentStatusBadge(styles *Styles, status enums.RoleStatus) string {
	switch status {
	case enums.RoleStatusActive:
		return styles.BadgeWarn.Render(" 运行 ")
	case enums.RoleStatusDone:
		return styles.BadgeOk.Render(" 完成 ")
	case enums.RoleStatusError:
		return styles.BadgeWarn.Render(" 错误 ")
	case enums.RoleStatusWaiting:
		return styles.Badge.Render(" 等待 ")
	default:
		return styles.Badge.Render(" 空闲 ")
	}
}

// renderAgentPanel is the legacy right-side Agent panel renderer kept for
// backwards compatibility; the main layout now uses renderAgentsPanel.
func renderAgentPanel(m *Model, w int) string {
	s := m.selectedSession()
	if s == nil {
		return m.styles.BlurBorder.Width(w).Render(m.styles.Dim.Render("No active session"))
	}

	var lines []string
	lines = append(lines, m.styles.Header.Render("Agents"))

	if len(m.agentTreePanel.nodes) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (no agents)"))
	} else {
		for _, node := range m.agentTreePanel.nodes {
			prefix := strings.Repeat("  ", node.depth)
			icon := statusIcon(string(node.status))
			name := node.name
			if node.goal != "" {
				name += " " + m.styles.Dim.Render(truncate(node.goal, w-12))
			}
			lines = append(lines, fmt.Sprintf("%s%s %s %s", prefix, icon, name, m.styles.Dim.Render("")))
		}
	}

	lines = append(lines, "", m.styles.Header.Render("Memory"))
	recalls := recentMemoryRecalls(s, 3)
	if len(recalls) == 0 {
		lines = append(lines, m.styles.Dim.Render("  (none)"))
	} else {
		for _, r := range recalls {
			lines = append(lines, m.styles.Dim.Render("  • "+truncate(r, w-4)))
		}
	}

	lines = append(lines, "", m.styles.Header.Render("Tokens"))
	lines = append(lines, fmt.Sprintf("  in:%d out:%d", m.totalInputTokens, m.totalOutputTokens))

	content := strings.Join(lines, "\n")
	return m.styles.BlurBorder.Width(w).Height(m.height - 2).Render(content)
}
