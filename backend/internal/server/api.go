package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/blockmemory/agent/backend/pkg/types"
)

// APIHandler TUI API 处理器
type APIHandler struct {
	broadcaster *TUIBroadcaster
	snapshotMgr interface {
		Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
	}
	// graphControl 用于暂停/恢复 Graph
	paused map[string]bool
}

// NewAPIHandler 创建 API 处理器
func NewAPIHandler(broadcaster *TUIBroadcaster) *APIHandler {
	return &APIHandler{
		broadcaster: broadcaster,
		paused:      make(map[string]bool),
	}
}

// SetSnapshotManager 设置快照管理器
func (h *APIHandler) SetSnapshotManager(mgr interface {
	Load(ctx context.Context, agentID, topicID string) (*types.AgentSnapshot, error)
}) {
	h.snapshotMgr = mgr
}

// RetrieveHandler 手动检索记忆
func (h *APIHandler) RetrieveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
		Query   string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 广播检索请求事件
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "retrieve.request",
		Payload: map[string]string{"agent_id": req.AgentID, "query": req.Query},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// EventResolveHandler 标记 Event 已处理
func (h *APIHandler) EventResolveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		EventID string `json:"event_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type: "workspace.event",
		Payload: types.EventPayload{
			ID:     req.EventID,
			Status: "Done",
		},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resolved"})
}

// SnapshotInspectHandler 查看快照详情
func (h *APIHandler) SnapshotInspectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var snapshot *types.AgentSnapshot
	if h.snapshotMgr != nil {
		var err error
		snapshot, err = h.snapshotMgr.Load(r.Context(), req.AgentID, req.TopicID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"agent_id": req.AgentID,
		"snapshot": snapshot,
	})
}

// GraphPauseHandler 暂停 Graph
func (h *APIHandler) GraphPauseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.paused[req.TopicID] = true
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "pause"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "paused"})
}

// GraphResumeHandler 恢复 Graph
func (h *APIHandler) GraphResumeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TopicID string `json:"topic_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	delete(h.paused, req.TopicID)
	h.broadcaster.Broadcast(req.TopicID, types.UIEvent{
		Type:    "graph.control",
		Payload: map[string]string{"action": "resume"},
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
}

// IsPaused 检查话题是否已暂停
func (h *APIHandler) IsPaused(topicID string) bool {
	return h.paused[topicID]
}
