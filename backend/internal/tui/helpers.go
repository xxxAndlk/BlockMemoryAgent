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
	// TUI chat shows only the conversation: user / assistant / system messages.
	// All Agent-internal events (think/intend/prompt/token_usage/tool_exec/...)
	// are logged server-side, not surfaced here.
	var items []chatItem
	for _, msg := range s.Messages {
		items = append(items, chatItem{
			title:  fmt.Sprintf("[%s] %s", msg.Role, msg.Timestamp.Format("15:04:05")),
			detail: formatMarkdown(msg.Content),
		})
	}
	return items
}

// formatMarkdown applies light Markdown formatting for the chat view.
// Supports: headers, bold, italic, inline code, code blocks, lists, blockquotes.
func formatMarkdown(text string) string {
	var out []string
	var inCodeBlock bool
	var codeBlock []string

	mdHeader := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cHeader))
	mdBold := lipgloss.NewStyle().Bold(true)
	mdItalic := lipgloss.NewStyle().Italic(true)
	mdCode := lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue))
	mdCodeBlock := lipgloss.NewStyle().Background(lipgloss.Color(cBlur)).Foreground(lipgloss.Color(cValue)).Padding(0, 1)
	mdDim := lipgloss.NewStyle().Foreground(lipgloss.Color(cDone))

	flushCode := func() {
		if len(codeBlock) == 0 {
			return
		}
		out = append(out, mdCodeBlock.Render(strings.Join(codeBlock, "\n")))
		codeBlock = nil
	}

	for _, raw := range strings.Split(text, "\n") {
		line := raw
		trimmed := strings.TrimSpace(line)

		// Code fence
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				flushCode()
				inCodeBlock = false
			} else {
				inCodeBlock = true
			}
			continue
		}
		if inCodeBlock {
			codeBlock = append(codeBlock, line)
			continue
		}

		// Header
		if strings.HasPrefix(trimmed, "#") {
			level := 0
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 0 && (level == len(trimmed) || trimmed[level] == ' ') {
				content := strings.TrimSpace(trimmed[level:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdHeader.Render(content))
				continue
			}
		}

		// Blockquote
		if strings.HasPrefix(trimmed, ">") {
			content := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
			content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
			out = append(out, mdDim.Render("┃ "+content))
			continue
		}

		// List item
		if strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "+") {
			if len(trimmed) > 1 && trimmed[1] == ' ' {
				content := strings.TrimSpace(trimmed[1:])
				content = applyInlineMarkdown(content, mdBold, mdItalic, mdCode)
				out = append(out, mdDim.Render("• ")+content)
				continue
			}
		}

		// Plain line
		line = applyInlineMarkdown(line, mdBold, mdItalic, mdCode)
		out = append(out, line)
	}
	flushCode()

	return strings.Join(out, "\n")
}

// applyInlineMarkdown handles **bold**, *italic*, and `inline code`.
func applyInlineMarkdown(text string, bold, italic, code lipgloss.Style) string {
	// Inline code
	text = replacePairs(text, "`", func(s string) string { return code.Render(s) })
	// Bold
	text = replacePairs(text, "**", func(s string) string { return bold.Render(s) })
	// Italic (single asterisks not already consumed by bold)
	text = replacePairs(text, "*", func(s string) string { return italic.Render(s) })
	return text
}

// replacePairs replaces matching pairs of markers with the result of f(content).
func replacePairs(text, marker string, f func(string) string) string {
	for {
		start := strings.Index(text, marker)
		if start == -1 {
			break
		}
		end := strings.Index(text[start+len(marker):], marker)
		if end == -1 {
			break
		}
		end += start + len(marker)
		content := text[start+len(marker) : end]
		text = text[:start] + f(content) + text[end+len(marker):]
	}
	return text
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
