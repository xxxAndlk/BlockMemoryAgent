package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
			m.pushHistory(cmd)
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
		h := m.sessionHistory()
		if len(h) == 0 {
			return m, nil
		}
		if m.inputHistIdx == -1 {
			m.inputHistIdx = len(h)
		}
		if m.inputHistIdx > 0 {
			m.inputHistIdx--
			m.inputRunes = []rune(h[m.inputHistIdx])
			m.inputCursor = len(m.inputRunes)
		}
		return m, nil

	case tea.KeyDown:
		h := m.sessionHistory()
		if m.inputHistIdx == -1 {
			return m, nil
		}
		if m.inputHistIdx < len(h)-1 {
			m.inputHistIdx++
			m.inputRunes = []rune(h[m.inputHistIdx])
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

// submitInput 解析用户输入命令并路由到对应 HTTP 端点。
// 命令语义：
//   - "/new <goal>"         无需选中会话，直接创建新会话
//   - "/cancel"            终止当前运行中会话
//   - "/clarify <id> <ans>" 回复特性5 的人机澄清请求
//   - "/interrupt <text>"   特性6 抢占中断
//   - "/enqueue <text>"     特性6 队列注入
//   - "/dag trigger <id>"   立即触发 DAG
//   - "/dag new <json>"     创建 DAG（JSON 内联）
//   - 其他                  作为普通消息追加到当前会话
// 未选中会话时，纯文本输入自动作为新会话的 goal。
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
		// 用 TrimPrefix 而非 Fields 拼接 answer，保留 answer 内的空格
		answer := strings.TrimSpace(strings.TrimPrefix(cmd, "/clarify "+id))
		m.postJSON(fmt.Sprintf("/api/sessions/%s/clarify", s.ID), map[string]string{"question_id": id, "answer": answer})
		return
	}

	// /cancel — 终止当前运行中会话
	if parts[0] == "/cancel" {
		m.postJSON(fmt.Sprintf("/api/sessions/%s/cancel", s.ID), map[string]any{})
		m.flashMsg("session cancelled")
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

// postJSON 向本地 TUI 后端发 POST 请求。
// 失败时自动重试 2 次（间隔 500ms），仅对网络/连接错误重试，4xx/5xx 不重试。
// 失败仅写 flashMsg 提示，不阻塞 TUI 主循环。
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
	client := &http.Client{Timeout: requestTimeout}

	const maxRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		req, err := http.NewRequest(http.MethodPost, addr+path, bytes.NewReader(data))
		if err != nil {
			m.flashMsg("request error: " + err.Error())
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		// Drain body for connection reuse (Step 8)
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			m.flashMsg(fmt.Sprintf("%s returned %d", path, resp.StatusCode))
		}
		return
	}
	m.flashMsg("post error (retried): " + lastErr.Error())
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
		_, _ = io.ReadAll(resp.Body)
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

// sessionHistory returns the input history slice for the currently selected session.
func (m *Model) sessionHistory() []string {
	s := m.selectedSession()
	if s == nil {
		return nil
	}
	return m.inputHistory[s.ID]
}

// pushHistory appends a command to the current session's input history.
func (m *Model) pushHistory(cmd string) {
	s := m.selectedSession()
	if s == nil {
		return
	}
	m.inputHistory[s.ID] = append(m.inputHistory[s.ID], cmd)
}

// getJSON 向本地 TUI 后端发 GET 请求并 JSON 解码到 dst。
// 失败返回 error，调用方自行处理（如显示 flash 或退回空结果）。
func (m *Model) getJSON(path string, dst any) error {
	addr := m.httpAddr
	if addr == "" {
		addr = "http://localhost:10010"
	}
	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Get(addr + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		_, _ = io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

const requestTimeout = 3 * time.Second
