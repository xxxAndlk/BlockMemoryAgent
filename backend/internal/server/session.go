package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
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
	Messages  []types.ChatMessage   `json:"messages"`
}

// SessionEvent 会话事件
type SessionEvent struct {
	Type       string    `json:"type"`
	Agent      string    `json:"agent"`
	Message    string    `json:"message"`
	Kind       string    `json:"kind,omitempty"` // progress 子类型: think/intend/llm/tool_call/tool_result/wait/error/prompt/agent_created/token_usage/graph_step
	Tool       string    `json:"tool,omitempty"`
	ToolPath   string    `json:"tool_path,omitempty"`
	ToolOutput string    `json:"tool_output,omitempty"`
	ToolError  string    `json:"tool_error,omitempty"`
	Success    bool      `json:"success,omitempty"`
	Timestamp  time.Time `json:"timestamp"`

	// ---- 调试扩展字段（v3 debug） ----
	Prompt       string `json:"prompt,omitempty"`        // 发送给 LLM 的 prompt（截断）
	InputTokens  int    `json:"input_tokens,omitempty"`  // 输入 token 估算
	OutputTokens int    `json:"output_tokens,omitempty"` // 输出 token 估算
	DetailJSON   string `json:"detail_json,omitempty"`   // 结构化详情（Agent 创建参数、图步骤状态等）
}

// SessionManager 会话管理器
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	graph    *graph.ThreeLayerGraph
	registry *graph.RoleRegistry
	seq      atomic.Int64
	pgStore  *store.PostgresStore
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

	// 将进度回调注入图，把 Agent 的思考/意图/工具调用实时推给会话事件流
	g.SetProgressCallback(graph.ProgressCallback(func(ev graph.ProgressEvent) {
		m.handleProgress(ev)
	}))

	return m
}

// SetPostgresStore 注入 Postgres 存储，用于会话结束时落历史
func (m *SessionManager) SetPostgresStore(pg *store.PostgresStore) {
	m.pgStore = pg
}

// handleToolResult 处理工具执行结果，将其广播到当前运行中的会话
func (m *SessionManager) handleToolResult(result *graph.ToolResult) {
	// 注意：不能在持有 m.mu.RLock 的情况下调用 addEvent（addEvent 内部取 Lock，
	// 同 goroutine RLock+Lock 会自死锁，导致会话卡死）。先在 RLock 下收集目标
	// 会话指针，释放后再写事件。
	m.mu.RLock()
	targets := make([]*Session, 0, 1)
	for _, session := range m.sessions {
		if session.Status == "running" {
			targets = append(targets, session)
		}
	}
	m.mu.RUnlock()

	for _, session := range targets {
		m.addEvent(session, "tool_exec", "ToolExecutor", fmt.Sprintf("执行工具: %s", result.Tool),
			"", result.Tool, result.Path, result.Output, result.Error, result.Success)
	}
}

// handleProgress 把 graph 的进度事件转为会话事件，推到当前运行中的会话。
// 事件类型映射:
//
//	think / intend / llm / wait -> "progress"
//	tool_call / tool_result     -> "progress"（tool_exec 仍由 handleToolResult 单独发）
//	error                       -> "progress"（标记 success=false）
//	prompt / agent_created / token_usage / graph_step -> 同上，但扩展字段携带调试信息
func (m *SessionManager) handleProgress(ev graph.ProgressEvent) {
	msg := ev.Message
	detail := ev.Detail
	if detail != "" {
		if len(detail) > 500 {
			detail = detail[:500] + "..."
		}
		msg += "\n" + detail
	}
	success := ev.Kind != "error"

	// 提取调试信息
	var prompt string
	var inputTokens, outputTokens int
	switch ev.Kind {
	case "prompt":
		prompt = ev.Detail // 原始 prompt 摘要
	case "token_usage":
		inputTokens, outputTokens = parseTokenUsage(ev.Message)
	}

	m.mu.RLock()
	targets := make([]*Session, 0, 1)
	for _, session := range m.sessions {
		if session.Status == "running" {
			targets = append(targets, session)
		}
	}
	m.mu.RUnlock()

	for _, session := range targets {
		m.addEventDebug(session, "progress", ev.Agent, msg, ev.Kind, "", "", "", "", success, prompt, inputTokens, outputTokens, detail)
	}
}

// parseTokenUsage 从 token_usage 消息中解析 in/out token 数
// 格式: "[caller] Token 消耗: in=N out=M dur=X"
func parseTokenUsage(msg string) (in, out int) {
	fmt.Sscanf(msg, "%*s Token 消耗: in=%d out=%d", &in, &out)
	return
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
		Messages: []types.ChatMessage{
			{Role: "system", Content: "Goal: " + goal, Timestamp: time.Now()},
		},
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
	// 全局超时5分钟，防止LLM调用无限挂起
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = session.Goal

	m.addEvent(session, "system", "MetaAgent", "会话启动，目标: "+session.Goal, "", "", "", "", "", false)

	result, err := m.graph.Invoke(ctx, state)
	if err != nil {
		m.mu.Lock()
		session.Status = "error"
		session.Result = err.Error()
		now := time.Now()
		session.EndedAt = &now
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
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
		m.addEvent(session, "agent_done", name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	m.addEvent(session, "system", "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)

	// 持久化会话历史（跨会话记忆基础）
	m.persistHistory(session)

	// 报告 LLM 统计
	if metaNode, ok := m.graph.GetNode("MetaAgent"); ok {
		if ma, ok := metaNode.(*graph.MetaAgentNode); ok {
			calls, timeouts, avg, max := ma.TimeoutStats()
			inTotal, outTotal := ma.LLMTracker().TokenTotals()
			if calls > 0 {
				m.addEvent(session, "stats", "System",
					fmt.Sprintf("LLM统计: 调用%d次, 超时%d次, 平均%v, 最长%v, 输入Token=%d, 输出Token=%d",
						calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), inTotal, outTotal),
					"", "", "", "", "", false)
			}
		}
	}
}

// persistHistory 把会话目标/总结/工具调用结果写入 session_history 表
func (m *SessionManager) persistHistory(session *Session) {
	if m.pgStore == nil {
		return
	}
	toolResults := make([]map[string]any, 0, len(session.Events))
	for _, ev := range session.Events {
		if ev.Type != "tool_exec" {
			continue
		}
		toolResults = append(toolResults, map[string]any{
			"tool":   ev.Tool,
			"path":   ev.ToolPath,
			"output": truncate(ev.ToolOutput, 500),
			"error":  ev.ToolError,
			"ok":     ev.Success,
		})
	}
	rec := &store.SessionHistoryRecord{
		SessionID:   session.ID,
		Goal:        session.Goal,
		Summary:     truncate(session.Result, 2000),
		ToolResults: toolResults,
		CreatedAt:   time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.pgStore.SaveSessionHistory(ctx, rec); err != nil {
		log.Printf("[%s] persist history: %v", session.ID, err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

func (m *SessionManager) addEvent(session *Session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool) {
	m.addEventDebug(session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError, success, "", 0, 0, "")
}

func (m *SessionManager) addEventDebug(session *Session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool, prompt string, inputTokens, outputTokens int, detailJSON string) {
	ev := SessionEvent{
		Type:       eventType,
		Agent:      agent,
		Message:    message,
		Kind:       kind,
		Tool:       tool,
		ToolPath:   toolPath,
		ToolOutput: toolOutput,
		ToolError:  toolError,
		Success:    success,
		Timestamp:  time.Now(),
		Prompt:       prompt,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		DetailJSON:   detailJSON,
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

	session := m.CreateSession(context.Background(), req.Goal)

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

// HandleSessionMessage POST /api/sessions/{id}/message
func (m *SessionManager) HandleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/api/sessions/"):]
	id = id[:len(id)-len("/message")]
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      "user",
		Content:   req.Content,
		Timestamp: time.Now(),
	})
	m.addEvent(session, "user_message", "User", req.Content, "", "", "", "", "", true)

	if session.Status != "running" {
		session.Status = "running"
		session.EndedAt = nil
		go m.resumeSession(session)
	}
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// resumeSession 基于历史消息恢复会话执行
func (m *SessionManager) resumeSession(session *Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 构建对话上下文（最近10条消息）
	var history strings.Builder
	start := 0
	if len(session.Messages) > 10 {
		start = len(session.Messages) - 10
	}
	for _, msg := range session.Messages[start:] {
		history.WriteString(fmt.Sprintf("%s: %s\n", msg.Role, msg.Content))
	}

	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = history.String()
	state.SessionSummary = session.Result
	state.Messages = session.Messages

	m.addEvent(session, "system", "MetaAgent", "继续会话，新消息已纳入上下文", "", "", "", "", "", false)

	result, err := m.graph.Invoke(ctx, state)
	if err != nil {
		m.mu.Lock()
		session.Status = "error"
		session.Result = err.Error()
		now := time.Now()
		session.EndedAt = &now
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
		return
	}

	m.mu.Lock()
	session.Status = "completed"
	session.Result = result.SessionSummary
	session.State = result
	now := time.Now()
	session.EndedAt = &now
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      "assistant",
		Content:   result.SessionSummary,
		Timestamp: now,
	})
	m.mu.Unlock()

	for _, inst := range m.registry.GetInstancesBySession(session.ID) {
		roleDef := m.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name
		}
		m.addEvent(session, "agent_done", name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	m.addEvent(session, "system", "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)
	m.persistHistory(session)
}
