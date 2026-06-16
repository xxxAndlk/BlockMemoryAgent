package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/internal/graph"
	"github.com/blockmemory/agent/pkg/types"
)

// Session 会话信息
type Session struct {
	ID        string                `json:"id"`
	Goal      string                `json:"goal"`
	Status    string                `json:"status"` // running / completed / error
	Result    string                `json:"result,omitempty"`
	State     *types.ThreeLayerState `json:"state,omitempty"`
	StartedAt time.Time             `json:"started_at"`
	EndedAt   *time.Time            `json:"ended_at,omitempty"`
	Events    []SessionEvent        `json:"events"`
}

// SessionEvent 会话事件
type SessionEvent struct {
	Type       string    `json:"type"`
	Agent      string    `json:"agent"`
	Message    string    `json:"message"`
	Tool       string    `json:"tool,omitempty"`
	ToolPath   string    `json:"tool_path,omitempty"`
	ToolOutput string    `json:"tool_output,omitempty"`
	ToolError  string    `json:"tool_error,omitempty"`
	Success    bool      `json:"success,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
}

// SessionManager 会话管理器
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	graph    *graph.ThreeLayerGraph
	registry *graph.RoleRegistry
	seq      atomic.Int64
}

// NewSessionManager 创建会话管理器
func NewSessionManager(g *graph.ThreeLayerGraph, registry *graph.RoleRegistry) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*Session),
		graph:    g,
		registry: registry,
	}

	// 将工具回调注入图，使工具执行结果自动成为会话事件
	g.SetToolCallback(graph.ToolCallback(func(result *graph.ToolResult) {
		m.handleToolResult(result)
	}))

	return m
}

// handleToolResult 处理工具执行结果，将其广播到当前运行中的会话
func (m *SessionManager) handleToolResult(result *graph.ToolResult) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 找到当前运行中的会话
	for _, session := range m.sessions {
		if session.Status == "running" {
			m.addEvent(session, "tool_exec", "ToolExecutor", fmt.Sprintf("执行工具: %s", result.Tool),
				result.Tool, result.Path, result.Output, result.Error, result.Success)
		}
	}
}

// CreateSession 创建并启动新会话
func (m *SessionManager) CreateSession(ctx context.Context, goal string) *Session {
	sessionID := fmt.Sprintf("session-%d", m.seq.Add(1))

	session := &Session{
		ID:        sessionID,
		Goal:      goal,
		Status:    "running",
		StartedAt: time.Now(),
		Events:    make([]SessionEvent, 0),
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	// 异步执行
	go m.runSession(ctx, session)

	return session
}

// GetSession 获取会话
func (m *SessionManager) GetSession(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ListSessions 列出所有会话
func (m *SessionManager) ListSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s)
	}
	return result
}

func (m *SessionManager) runSession(ctx context.Context, session *Session) {
	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = session.Goal

	m.addEvent(session, "system", "MetaAgent", "会话启动，目标: "+session.Goal, "", "", "", "", false)

	result, err := m.graph.Invoke(ctx, state)
	if err != nil {
		m.mu.Lock()
		session.Status = "error"
		session.Result = err.Error()
		now := time.Now()
		session.EndedAt = &now
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "执行失败: "+err.Error(), "", "", "", "", false)
		return
	}

	m.mu.Lock()
	session.Status = "completed"
	session.Result = result.SessionSummary
	session.State = result
	now := time.Now()
	session.EndedAt = &now
	m.mu.Unlock()

	// 添加角色实例事件
	for _, inst := range m.registry.GetInstancesBySession(session.ID) {
		roleDef := m.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name
		}
		m.addEvent(session, "agent_done", name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", false)
	}

	m.addEvent(session, "system", "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", false)
}

func (m *SessionManager) addEvent(session *Session, eventType, agent, message, tool, toolPath, toolOutput, toolError string, success bool) {
	ev := SessionEvent{
		Type:       eventType,
		Agent:      agent,
		Message:    message,
		Tool:       tool,
		ToolPath:   toolPath,
		ToolOutput: toolOutput,
		ToolError:  toolError,
		Success:    success,
		Timestamp:  time.Now(),
	}
	m.mu.Lock()
	session.Events = append(session.Events, ev)
	m.mu.Unlock()

	// 广播SSE
	log.Printf("[%s] %s: %s", session.ID, agent, message)
}

// ---- HTTP Handlers ----

// HandleCreateSession POST /api/sessions
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Goal string `json:"goal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Goal == "" {
		http.Error(w, "goal is required", http.StatusBadRequest)
		return
	}

	session := m.CreateSession(r.Context(), req.Goal)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// HandleGetSession GET /api/sessions/{id}
func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sessions/"):]
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// HandleListSessions GET /api/sessions
func (m *SessionManager) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions := m.ListSessions()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

// HandleSessionStream GET /api/sessions/{id}/stream
func (m *SessionManager) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sessions/"):]
	id = id[:len(id)-len("/stream")]

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// 先发送当前状态
	data, _ := json.Marshal(session)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	// 轮询更新
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastEventCount := len(session.Events)

	for {
		select {
		case <-ticker.C:
			session = m.GetSession(id)
			if session == nil {
				return
			}

			if len(session.Events) > lastEventCount {
				for _, ev := range session.Events[lastEventCount:] {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				lastEventCount = len(session.Events)
				flusher.Flush()
			}

			if session.Status != "running" {
				data, _ := json.Marshal(map[string]string{"type": "done", "status": session.Status})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-r.Context().Done():
			return
		}
	}
}
