package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// HandleCreateSession POST /api/sessions.
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	req, err := DecodeBody[struct {
		Goal string `json:"goal"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Goal == "" {
		http.Error(w, "目标 (goal) 不能为空", http.StatusBadRequest)
		return
	}

	session, err := m.agent.CreateSession(r.Context(), agent.CreateRequest{Goal: req.Goal})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}

// HandleGetSession GET /api/sessions/{id}.
func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}

// HandleListSessions GET /api/sessions.
func (m *SessionManager) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := m.agent.List(r.Context(), agent.Filter{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	all := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		all = append(all, ToServerSession(s))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(all)
}

// HandleSessionBoard GET /api/sessions/{id}/board.
func (m *SessionManager) HandleSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindBoard})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"board":      res.Data,
	})
}

// agentNode is the on-the-wire shape for the agents endpoint.
type agentNode struct {
	InstID    string `json:"inst_id"`
	RoleDefID string `json:"role_def_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Domain    string `json:"domain"`
	Status    string `json:"status"`
	ParentID  string `json:"parent_id"`
	Goal      string `json:"goal,omitempty"`
	BlockID   string `json:"block_id,omitempty"`
}

// HandleSessionAgents GET /api/sessions/{id}/agents.
func (m *SessionManager) HandleSessionAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	blockGoalByDomain := make(map[string]string)
	blockIDByDomain := make(map[string]string)
	for _, b := range session.ActiveBlocks {
		blockGoalByDomain[b.Domain] = b.Goal
		blockIDByDomain[b.Domain] = b.ID
	}

	instances, err := m.agent.ListAgents(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nodes := make([]agentNode, 0, len(instances))
	for _, inst := range instances {
		goal := ""
		blockID := ""
		if inst.Domain != "" {
			goal = blockGoalByDomain[inst.Domain]
			blockID = blockIDByDomain[inst.Domain]
		}
		nodes = append(nodes, agentNode{
			InstID:    inst.ModuleID,
			RoleDefID: inst.RoleDefID,
			Name:      inst.Name,
			Type:      inst.Role,
			Domain:    inst.Domain,
			Status:    inst.Status,
			ParentID:  "",
			Goal:      goal,
			BlockID:   blockID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"agents":     nodes,
	})
}

// HandleSessionTopic POST /api/sessions/{id}/topic.
func (m *SessionManager) HandleSessionTopic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	req, err := DecodeBody[struct {
		Name string `json:"name"`
		Goal string `json:"goal,omitempty"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "话题名称 (name) 不能为空", http.StatusBadRequest)
		return
	}

	// Prefer the dedicated SwitchTopic method when available so non-running
	// sessions correctly return the newly created session object.
	type topicSwitcher interface {
		SwitchTopic(ctx context.Context, sessionID, name, goal string) (*agent.Session, error)
	}

	var switched *agent.Session
	var topicErr error
	if ts, ok := m.agent.(topicSwitcher); ok {
		switched, topicErr = ts.SwitchTopic(r.Context(), id, req.Name, req.Goal)
	} else {
		topicErr = m.agent.Control(r.Context(), id, agent.ControlCommand{
			Op: agent.ControlOpTopic,
			Args: map[string]any{
				"name": req.Name,
				"goal": req.Goal,
			},
		})
	}
	if topicErr != nil {
		msg, status := agentErrorStatus(topicErr)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if switched != nil && switched.ID != id {
		// Non-running session: a new session was created to continue the topic.
		json.NewEncoder(w).Encode(ToServerSession(switched))
	} else {
		json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running", "topic": req.Name})
	}
}

// HandleSessionMessage POST /api/sessions/{id}/message.
func (m *SessionManager) HandleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	req, err := DecodeBody[struct {
		Content string `json:"content"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	if err := m.agent.Send(r.Context(), id, agent.Message{Content: req.Content, Timestamp: time.Now()}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	session, err := m.agent.Get(r.Context(), id)
	if err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToServerSession(session))
}
