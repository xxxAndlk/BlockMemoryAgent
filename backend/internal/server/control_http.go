package server

import (
	"encoding/json"
	"net/http"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// HandleSessionClarify POST /api/sessions/{id}/clarify.
func (m *SessionManager) HandleSessionClarify(w http.ResponseWriter, r *http.Request) {
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
		Answer     string `json:"answer"`
		QuestionID string `json:"question_id"`
	}](r)
	if err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Answer == "" {
		http.Error(w, "答复内容不能为空", http.StatusBadRequest)
		return
	}

	if err := m.agent.Control(r.Context(), id, agent.ControlCommand{
		Op: agent.ControlOpClarify,
		Args: map[string]any{
			"answer":      req.Answer,
			"question_id": req.QuestionID,
		},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"status":     "running",
	})
}

// HandleSessionInterrupt POST /api/sessions/{id}/interrupt.
func (m *SessionManager) HandleSessionInterrupt(w http.ResponseWriter, r *http.Request) {
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

	if err := m.agent.Control(r.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpInterrupt,
		Args: map[string]any{"content": req.Content},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionEnqueue POST /api/sessions/{id}/enqueue.
func (m *SessionManager) HandleSessionEnqueue(w http.ResponseWriter, r *http.Request) {
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

	if err := m.agent.Control(r.Context(), id, agent.ControlCommand{
		Op:   agent.ControlOpEnqueue,
		Args: map[string]any{"content": req.Content},
	}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionCancel POST /api/sessions/{id}/cancel.
func (m *SessionManager) HandleSessionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	if err := m.agent.Control(r.Context(), id, agent.ControlCommand{Op: agent.ControlOpCancel}); err != nil {
		msg, status := agentErrorStatus(err)
		http.Error(w, msg, status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "error"})
}
