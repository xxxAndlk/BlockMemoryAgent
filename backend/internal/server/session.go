package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Session represents the runtime state of a single conversation.
// It remains the on-the-wire DTO for the HTTP API; the canonical lifecycle owner
// is now agent.Service.
type Session struct {
	ID        string                 `json:"id"`
	Goal      string                 `json:"goal"`
	Status    enums.SessionStatus    `json:"status"`
	Result    string                 `json:"result,omitempty"`
	State     *types.ThreeLayerState `json:"state,omitempty"`
	StartedAt time.Time              `json:"started_at"`
	EndedAt   *time.Time             `json:"ended_at,omitempty"`
	Events    []SessionEvent         `json:"events"`
	Messages  []types.ChatMessage    `json:"messages"`
	TempDir   string                 `json:"temp_dir,omitempty"`
}

// SessionEvent is a single event in the session event stream.
type SessionEvent struct {
	Type         string    `json:"type"`
	Agent        string    `json:"agent"`
	Message      string    `json:"message"`
	Kind         string    `json:"kind,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	ToolPath     string    `json:"tool_path,omitempty"`
	ToolOutput   string    `json:"tool_output,omitempty"`
	ToolError    string    `json:"tool_error,omitempty"`
	Success      bool      `json:"success,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	Prompt       string    `json:"prompt,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	DetailJSON   string    `json:"detail_json,omitempty"`
}

// SessionManager is a thin HTTP adapter around agent.Agent.
// All session mutations and read-only queries are delegated to the Agent facade.
type SessionManager struct {
	agent agent.Agent
}

// NewSessionManager creates an HTTP adapter that delegates to the supplied Agent.
func NewSessionManager(agentFacade agent.Agent) *SessionManager {
	return &SessionManager{agent: agentFacade}
}

// SetPostgresStore is retained for interface compatibility but is now a no-op;
// persistence is configured on agent.Service.
func (m *SessionManager) SetPostgresStore(pg *store.PostgresStore) {}

// SetModelFactory is retained for interface compatibility but is now a no-op;
// the model factory is configured on agent.Service.
func (m *SessionManager) SetModelFactory(mf *model.ModelFactory) {}

// LaunchSession implements dag.SessionLauncher.
func (m *SessionManager) LaunchSession(goal string) string {
	s, err := m.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: goal})
	if err != nil || s == nil {
		return ""
	}
	return s.ID
}

// RestoreSessions delegates to the Agent facade.
func (m *SessionManager) RestoreSessions(ctx context.Context, limit int) int {
	type restorer interface {
		RestoreSessions(ctx context.Context, limit int) int
	}
	if r, ok := m.agent.(restorer); ok {
		return r.RestoreSessions(ctx, limit)
	}
	return 0
}

// GetSession returns a server.Session by ID, converting from the Agent DTO.
func (m *SessionManager) GetSession(id string) *Session {
	s, err := m.agent.Get(context.Background(), id)
	if err != nil || s == nil {
		return nil
	}
	return ToServerSession(s)
}

// LLMStats returns aggregated LLM call statistics via the Agent facade.
func (m *SessionManager) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	type statsProvider interface {
		LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration)
	}
	if sp, ok := m.agent.(statsProvider); ok {
		return sp.LLMStats()
	}
	res, err := m.agent.Query(context.Background(), "", agent.Query{Kind: agent.QueryKindLLMStats})
	if err != nil {
		return 0, 0, 0, 0
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		return 0, 0, 0, 0
	}
	callCount, _ = data["calls"].(int)
	timeoutCount, _ = data["timeouts"].(int)
	if avgStr, ok := data["avg_duration"].(string); ok {
		avgDur, _ = time.ParseDuration(avgStr)
	}
	if maxStr, ok := data["max_duration"].(string); ok {
		maxDur, _ = time.ParseDuration(maxStr)
	}
	return
}

// SessionCount returns the number of sessions currently held in memory.
func (m *SessionManager) SessionCount() int {
	type counter interface {
		SessionCount() int
	}
	if c, ok := m.agent.(counter); ok {
		return c.SessionCount()
	}
	res, err := m.agent.Query(context.Background(), "", agent.Query{Kind: agent.QueryKindSessionCount})
	if err != nil {
		return 0
	}
	if n, ok := res.Data.(int); ok {
		return n
	}
	return 0
}

// ListSessions returns all sessions, converted from the Agent DTO.
func (m *SessionManager) ListSessions() []*Session {
	sessions, err := m.agent.List(context.Background(), agent.Filter{})
	if err != nil {
		log.Printf("ListSessions error: %v", err)
		return nil
	}
	out := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, ToServerSession(s))
	}
	return out
}

// Shutdown cancels all running sessions.
func (m *SessionManager) Shutdown() {
	_ = m.agent.Shutdown(context.Background())
}

// SnapshotSession returns a deep copy of the session in server.Session form.
func (m *SessionManager) SnapshotSession(id string) *Session {
	return m.GetSession(id)
}

// ClearSessionChat delegates to the Agent facade.
func (m *SessionManager) ClearSessionChat(id string) bool {
	type clearer interface {
		ClearSessionChat(id string) bool
	}
	if c, ok := m.agent.(clearer); ok {
		return c.ClearSessionChat(id)
	}
	return false
}

// ToServerSession converts an agent.Session DTO to the server.Session wire type.
//
// Legacy bridge: the ThreeLayerState shape is retained only for frontend/API
// compatibility. It will be removed once the frontend stops depending on
// current_domain / active_blocks.
func ToServerSession(a *agent.Session) *Session {
	if a == nil {
		return nil
	}

	// Legacy bridge: map the new ReAct Session DTO back to the old ThreeLayerState
	// wire shape so existing HTTP clients keep working.
	state := &types.ThreeLayerState{
		CurrentDomain: a.State,
	}
	if len(a.ActiveBlocks) > 0 {
		state.ActiveBlocks = make(map[string]*types.SessionBlock, len(a.ActiveBlocks))
		for _, b := range a.ActiveBlocks {
			state.ActiveBlocks[b.ID] = &types.SessionBlock{
				ID:     b.ID,
				Domain: b.Domain,
				Goal:   b.Goal,
			}
		}
	}
	if a.PendingClarify != nil {
		req := a.PendingClarify
		state.PendingClarify = &types.ClarifyRequest{
			ID:         req.ID,
			Question:   req.Question,
			Context:    req.Context,
			AgentID:    req.AgentID,
			CreatedAt:  req.CreatedAt,
			Answer:     req.Answer,
			AnsweredAt: req.AnsweredAt,
		}
	}
	if a.State == "" && len(a.ActiveBlocks) == 0 && a.PendingClarify == nil {
		state = nil
	}

	var endedAt *time.Time
	if !a.EndedAt.IsZero() {
		t := a.EndedAt
		endedAt = &t
	}

	events := make([]SessionEvent, len(a.Events))
	for i, e := range a.Events {
		events[i] = SessionEvent{
			Type:         e.Type,
			Agent:        e.Agent,
			Message:      e.Message,
			Kind:         e.Kind,
			Tool:         e.Tool,
			ToolPath:     e.ToolPath,
			ToolOutput:   e.ToolOutput,
			ToolError:    e.ToolError,
			Success:      e.Success,
			Timestamp:    e.Timestamp,
			Prompt:       e.Prompt,
			InputTokens:  e.InputTokens,
			OutputTokens: e.OutputTokens,
			DetailJSON:   e.DetailJSON,
		}
	}

	messages := make([]types.ChatMessage, len(a.Messages))
	for i, msg := range a.Messages {
		messages[i] = types.ChatMessage{
			Role:      enums.ChatRole(msg.Role),
			Content:   msg.Content,
			Timestamp: msg.Timestamp,
		}
	}

	return &Session{
		ID:        a.ID,
		Goal:      a.Goal,
		Status:    enums.SessionStatus(a.Status),
		Result:    a.Result,
		State:     state,
		StartedAt: a.StartedAt,
		EndedAt:   endedAt,
		Events:    events,
		Messages:  messages,
		TempDir:   a.TempDir,
	}
}

// HandleSessionMetrics GET /api/sessions/{id}/metrics.
func (m *SessionManager) HandleSessionMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindMetrics})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// HandleSessionWatchdog GET /api/sessions/{id}/watchdog.
func (m *SessionManager) HandleSessionWatchdog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindWatchdog})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"decisions":  res.Data,
	})
}

// HandleSessionMailbox GET /api/sessions/{id}/mailbox.
func (m *SessionManager) HandleSessionMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindMailbox})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"messages":   res.Data,
	})
}

// HandleSessionLogs GET /api/sessions/{id}/logs.
func (m *SessionManager) HandleSessionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	res, err := m.agent.Query(r.Context(), id, agent.Query{
		Kind: agent.QueryKindLogs,
		Args: map[string]any{
			"agent":  r.URL.Query().Get("agent"),
			"level":  r.URL.Query().Get("level"),
			"limit":  limit,
			"offset": offset,
		},
	})
	if err != nil {
		log.Printf("[SessionManager] 查询 session_logs 失败: session=%s err=%v", id, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// HandleSessionTokenMetrics GET /api/sessions/{id}/token-metrics.
func (m *SessionManager) HandleSessionTokenMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindTokenMetrics})
	if err != nil {
		log.Printf("[SessionManager] 聚合 token 消耗失败: session=%s err=%v", id, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// agentErrorStatus maps agent sentinel errors to HTTP status codes.
// Unknown errors are returned as 500 with their message.
func agentErrorStatus(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		return "会话不存在", http.StatusNotFound
	case errors.Is(err, agent.ErrQueueFull):
		return "命令队列已满", http.StatusServiceUnavailable
	case errors.Is(err, agent.ErrSessionFinished), errors.Is(err, agent.ErrInvalidSessionState):
		return err.Error(), http.StatusBadRequest
	case errors.Is(err, agent.ErrPostgresUnavailable):
		return "存储后端不可用", http.StatusServiceUnavailable
	default:
		return err.Error(), http.StatusInternalServerError
	}
}
