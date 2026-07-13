package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

// HandleSessionStream GET /api/sessions/{id}/stream.
func (m *SessionManager) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式输出", http.StatusInternalServerError)
		return
	}

	snapshot := ToServerSession(session)
	data, _ := json.Marshal(snapshot)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastEventCount := len(snapshot.Events)

	for {
		select {
		case <-ticker.C:
			session, err := m.agent.Get(r.Context(), id)
			if err != nil {
				return
			}

			snapshot = ToServerSession(session)
			currentEvents := snapshot.Events

			if lastEventCount > len(currentEvents) {
				lastEventCount = len(currentEvents)
			}

			if len(currentEvents) > lastEventCount {
				for _, ev := range currentEvents[lastEventCount:] {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				lastEventCount = len(currentEvents)
				flusher.Flush()
			}

			if snapshot.Status == enums.SessionStatusAwaitingClarify {
				pending := ""
				qid := ""
				if snapshot.State != nil && snapshot.State.PendingClarify != nil {
					pending = snapshot.State.PendingClarify.Question
					qid = snapshot.State.PendingClarify.ID
				}
				data, _ := json.Marshal(map[string]string{
					"type":        "awaiting_clarify",
					"status":      string(snapshot.Status),
					"question":    pending,
					"question_id": qid,
				})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}

			if snapshot.Status != enums.SessionStatusRunning && snapshot.Status != enums.SessionStatusAwaitingClarify {
				data, _ := json.Marshal(map[string]string{"type": "done", "status": string(snapshot.Status)})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-r.Context().Done():
			return
		}
	}
}
