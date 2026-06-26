package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/blockmemory/agent/backend/internal/board"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func (m *Model) flashMsg(msg string) {
	m.flash = msg
	m.flashUntil = time.Now().Add(2 * time.Second)
}

func (m *Model) hasPlan() bool {
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		return false
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		return false
	}
	return len(b.Snapshot().Tasks) > 0
}

func (m *Model) planStats() (done, total int) {
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		return 0, 0
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		return 0, 0
	}
	snap := b.Snapshot()
	total = len(snap.Tasks)
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
	}
	return done, total
}

// showChatDetail opens a popup with the full content of the selected chat item.
func (m *Model) showChatDetail() {
	s := m.selectedSession()
	if s == nil {
		return
	}
	items := chatItems(s)
	if m.chatCursor < 0 || m.chatCursor >= len(items) {
		return
	}
	item := items[m.chatCursor]
	m.openOverlay(item.title, strings.Split(item.detail, "\n"))
}

func (m *Model) showPlanDetailByIndex(idx int) {
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		return
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		return
	}
	snap := b.Snapshot()
	if idx < 0 || idx >= len(snap.Tasks) {
		return
	}
	t := snap.Tasks[idx]
	lines := []string{
		fmt.Sprintf("ID: %s", t.ID),
		fmt.Sprintf("Title: %s", t.Title),
		fmt.Sprintf("Status: %s", t.Status),
		fmt.Sprintf("Assignee: %s", t.Assignee),
	}
	if t.Result != "" {
		lines = append(lines, fmt.Sprintf("Result: %s", t.Result))
	}
	m.openOverlay("Plan Task", lines)
}

func (m *Model) showAgentDetailByIndex(idx int) {
	if idx < 0 || idx >= len(m.agentsNodes) {
		return
	}
	node := m.agentsNodes[idx]
	lines := []string{
		fmt.Sprintf("Instance: %s", node.instID),
		fmt.Sprintf("Name: %s", node.name),
		fmt.Sprintf("Type: %s", node.roleType),
		fmt.Sprintf("Status: %s", node.status),
	}
	if node.domain != "" {
		lines = append(lines, fmt.Sprintf("Domain: %s", node.domain))
	}
	if node.goal != "" {
		lines = append(lines, fmt.Sprintf("Goal: %s", node.goal))
	}
	m.openOverlay("Agent", lines)
}

type chatItem struct {
	title  string
	detail string
}

// buildPlanLines renders the current session's TaskBoard as flat lines for the popup.
func (m *Model) buildPlanLines() []string {
	s := m.selectedSession()
	if s == nil || m.rt == nil || m.rt.Boards == nil {
		return []string{"(no plan)"}
	}
	b := m.rt.Boards.Get(s.ID)
	if b == nil {
		return []string{"(no plan)"}
	}
	snap := b.Snapshot()
	done, total := 0, len(snap.Tasks)
	for _, t := range snap.Tasks {
		if t.Status == board.TaskDone {
			done++
		}
	}
	lines := []string{
		fmt.Sprintf("Goal: %s", snap.Goal),
		fmt.Sprintf("Progress: %d/%d", done, total),
		"",
	}
	for i, t := range snap.Tasks {
		marker := " "
		if i == m.overlayCursor {
			marker = "▸"
		}
		lines = append(lines, fmt.Sprintf("%s %s  %s", marker, statusIcon(string(t.Status)), t.Title))
	}
	if total == 0 {
		lines = append(lines, m.styles.Dim.Render("(empty)"))
	}
	return lines
}

// buildAgentsLines renders the agent topology as flat lines for the popup.
func (m *Model) buildAgentsLines() []string {
	if len(m.agentsNodes) == 0 {
		return []string{"(no agents)"}
	}
	var lines []string
	for i, node := range m.agentsNodes {
		prefix := strings.Repeat("  ", node.depth)
		var icon string
		switch node.roleType {
		case types.RoleTypeMeta:
			icon = "◆"
		case types.RoleTypeDomain:
			icon = "◆"
		case types.RoleTypeSubDomain:
			icon = "◇"
		default:
			icon = "▸"
		}
		marker := " "
		if i == m.overlayCursor {
			marker = "▸"
		}
		name := node.name
		if node.goal != "" {
			name += " — " + truncate(node.goal, 40)
		}
		if node.isClarify {
			name = "Clarify pending"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s  %s", marker, prefix, icon, name, statusIcon(string(node.status))))
	}
	return lines
}

func chatItems(s *server.Session) []chatItem {
	var items []chatItem
	for _, msg := range s.Messages {
		items = append(items, chatItem{
			title:  fmt.Sprintf("[%s] %s", msg.Role, msg.Timestamp.Format("15:04:05")),
			detail: msg.Content,
		})
	}
	for _, ev := range s.Events {
		items = append(items, chatItem{
			title:  fmt.Sprintf("[%s] %s %s", ev.Type, ev.Agent, ev.Timestamp.Format("15:04:05")),
			detail: ev.Message,
		})
	}
	return items
}

func statusIcon(status string) string {
	switch status {
	case "running", string(types.RoleStatusActive), string(board.TaskInProgress):
		return "●"
	case "awaiting_clarify", string(types.RoleStatusWaiting), string(board.TaskBlocked):
		return "◐"
	case "completed", string(board.TaskDone):
		return "✓"
	case "error", string(board.TaskFailed):
		return "✗"
	case string(board.TaskPending), string(types.RoleStatusIdle):
		return "◦"
	default:
		return "◦"
	}
}

func statusColor(styles *Styles, status string) lipgloss.Style {
	switch status {
	case "running", string(types.RoleStatusActive), string(board.TaskInProgress):
		return styles.TreeActive
	case "awaiting_clarify", string(types.RoleStatusWaiting), string(board.TaskBlocked):
		return styles.LogWarn
	case "completed", string(board.TaskDone):
		return styles.TreeDone
	case "error", string(board.TaskFailed):
		return styles.LogError
	default:
		return styles.Dim
	}
}
