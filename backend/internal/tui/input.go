package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/blockmemory/agent/backend/internal/dag"
	"github.com/blockmemory/agent/backend/internal/server"
)

func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.focus = panelChat
		m.inputRunes = nil
		m.inputCursor = 0
		m.inputMode = inputNormal
		m.inputHistIdx = -1
		return m, nil

	case tea.KeyEnter:
		cmd := string(m.inputRunes)
		if strings.TrimSpace(cmd) != "" {
			m.inputHistory = append(m.inputHistory, cmd)
		}
		m.inputHistIdx = -1
		m.submitInput(cmd)
		m.inputRunes = nil
		m.inputCursor = 0
		m.inputMode = inputNormal
		// Keep focus in input so the user can immediately type the next message.
		m.focus = panelInput
		return m, nil

	case tea.KeyUp:
		if len(m.inputHistory) == 0 {
			return m, nil
		}
		if m.inputHistIdx == -1 {
			m.inputHistIdx = len(m.inputHistory)
		}
		if m.inputHistIdx > 0 {
			m.inputHistIdx--
			m.inputRunes = []rune(m.inputHistory[m.inputHistIdx])
			m.inputCursor = len(m.inputRunes)
		}
		return m, nil

	case tea.KeyDown:
		if m.inputHistIdx == -1 {
			return m, nil
		}
		if m.inputHistIdx < len(m.inputHistory)-1 {
			m.inputHistIdx++
			m.inputRunes = []rune(m.inputHistory[m.inputHistIdx])
			m.inputCursor = len(m.inputRunes)
		} else {
			m.inputHistIdx = -1
			m.inputRunes = nil
			m.inputCursor = 0
		}
		return m, nil

	case tea.KeyBackspace:
		if m.inputCursor > 0 {
			m.inputRunes = append(m.inputRunes[:m.inputCursor-1], m.inputRunes[m.inputCursor:]...)
			m.inputCursor--
		}
		return m, nil

	case tea.KeyDelete:
		if m.inputCursor < len(m.inputRunes) {
			m.inputRunes = append(m.inputRunes[:m.inputCursor], m.inputRunes[m.inputCursor+1:]...)
		}
		return m, nil

	case tea.KeyLeft:
		if m.inputCursor > 0 {
			m.inputCursor--
		}
		return m, nil

	case tea.KeyRight:
		if m.inputCursor < len(m.inputRunes) {
			m.inputCursor++
		}
		return m, nil

	case tea.KeyHome:
		m.inputCursor = 0
		return m, nil

	case tea.KeyEnd:
		m.inputCursor = len(m.inputRunes)
		return m, nil

	case tea.KeyCtrlC:
		// ctrl+c quits the TUI from anywhere, including the input bar.
		return m, tea.Quit

	case tea.KeyRunes:
		m.inputRunes = append(m.inputRunes[:m.inputCursor], append(msg.Runes, m.inputRunes[m.inputCursor:]...)...)
		m.inputCursor += len(msg.Runes)
		return m, nil
	}

	return m, nil
}

func (m *Model) submitInput(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}

	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return
	}

	// /new <goal...> works without a selected session.
	if parts[0] == "/new" && len(parts) > 1 {
		goal := strings.TrimSpace(strings.TrimPrefix(cmd, "/new "))
		m.createSession(goal)
		return
	}

	s := m.selectedSession()
	if s == nil {
		// No session yet: treat plain input as a new conversation goal.
		m.createSession(cmd)
		return
	}

	// /clarify <id> <answer...>
	if parts[0] == "/clarify" && len(parts) >= 3 {
		id := parts[1]
		answer := strings.TrimSpace(strings.TrimPrefix(cmd, "/clarify "+id))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", s.ID), map[string]string{"question_id": id, "answer": answer})
		return
	}

	// /interrupt <goal...>
	if parts[0] == "/interrupt" && len(parts) > 1 {
		content := strings.TrimSpace(strings.TrimPrefix(cmd, "/interrupt "))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/interrupt", s.ID), map[string]string{"content": content})
		m.flashMsg("interrupted")
		return
	}

	// /enqueue <text...>
	if parts[0] == "/enqueue" && len(parts) > 1 {
		content := strings.TrimSpace(strings.TrimPrefix(cmd, "/enqueue "))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/enqueue", s.ID), map[string]string{"content": content})
		return
	}

	// /dag trigger <id>
	if len(parts) >= 3 && parts[0] == "/dag" && parts[1] == "trigger" {
		m.postJSON(fmt.Sprintf("/api/dag/%s/trigger", parts[2]), map[string]any{})
		return
	}

	// /dag new <json...>
	if len(parts) >= 3 && parts[0] == "/dag" && parts[1] == "new" {
		var d dag.DAG
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(cmd, "/dag new "))), &d); err != nil {
			m.flashMsg("invalid dag json: " + err.Error())
			return
		}
		m.postJSON("/api/dag", d)
		return
	}

	// default: message
	m.postJSON(fmt.Sprintf("/api/sessions/%s/message", s.ID), map[string]string{"content": cmd})
}

func (m *Model) postJSON(path string, body any) {
	addr := m.httpAddr
	if addr == "" {
		addr = "http://localhost:10010"
	}
	data, err := json.Marshal(body)
	if err != nil {
		m.flashMsg("marshal error: " + err.Error())
		return
	}
	req, err := http.NewRequest(http.MethodPost, addr+path, bytes.NewReader(data))
	if err != nil {
		m.flashMsg("request error: " + err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Use a short timeout; failures are non-fatal in TUI.
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		m.flashMsg("post error: " + err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		m.flashMsg(fmt.Sprintf("%s returned %d", path, resp.StatusCode))
	}
}

// createSession POSTs /api/sessions with the goal, then selects the new session.
func (m *Model) createSession(goal string) {
	addr := m.httpAddr
	if addr == "" {
		addr = "http://localhost:10010"
	}
	body, _ := json.Marshal(map[string]string{"goal": goal})
	req, err := http.NewRequest(http.MethodPost, addr+"/api/sessions", bytes.NewReader(body))
	if err != nil {
		m.flashMsg("request error: " + err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		m.flashMsg("create session: " + err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		m.flashMsg(fmt.Sprintf("create session returned %d", resp.StatusCode))
		return
	}
	var created server.Session
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		m.flashMsg("decode session: " + err.Error())
		return
	}
	m.refreshSessions()
	// Select the freshly created session by ID.
	for i, s := range m.sessions {
		if s.ID == created.ID {
			m.selectSession(i)
			break
		}
	}
	m.flashMsg("session started: " + created.ID)
}

const requestTimeout = 3 * time.Second
