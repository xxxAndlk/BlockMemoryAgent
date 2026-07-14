package server

import (
	"encoding/json" // JSON 编解码
	"net/http"      // HTTP 处理器与状态码

	"github.com/blockmemory/agent/backend/internal/agent" // Agent 门面与控制命令
)

// HandleSessionClarify 处理 POST /api/sessions/{id}/clarify。
// 职责：接收用户对澄清问题的回答，转发给 Agent 继续会话。
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

// HandleSessionInterrupt 处理 POST /api/sessions/{id}/interrupt。
// 职责：向会话发送中断内容，打断当前 Agent 执行。
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

// HandleSessionEnqueue 处理 POST /api/sessions/{id}/enqueue。
// 职责：将用户内容入队，供会话后续处理。
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

// HandleSessionCancel 处理 POST /api/sessions/{id}/cancel。
// 职责：取消会话当前任务。
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
