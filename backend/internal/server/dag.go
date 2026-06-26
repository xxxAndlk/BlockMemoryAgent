// Package server dag.go 实现特性1 DAG 调度的 HTTP 接口。
//
// 路由：
//   - GET    /api/dag           列出全部 DAG
//   - POST   /api/dag           创建/更新 DAG（按 id upsert）
//   - GET    /api/dag/{id}      取单条 DAG
//   - DELETE /api/dag/{id}      删除 DAG
//   - POST   /api/dag/{id}/trigger  立即触发一次 DAG
//   - GET    /api/dag/running   查看运行中 DAG 快照
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/dag"
)

// DAGHandler DAG HTTP 处理器。
type DAGHandler struct {
	Store     dag.Store
	Scheduler *dag.Scheduler
}

// NewDAGHandler 创建处理器。store/scheduler 由 main.go 注入。
func NewDAGHandler(store dag.Store, sched *dag.Scheduler) *DAGHandler {
	return &DAGHandler{Store: store, Scheduler: sched}
}

// ServeHTTP 统一分发 /api/dag* 路径。
func (h *DAGHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/dag")
	path = strings.Trim(path, "/")

	// /api/dag/running
	if path == "running" && r.Method == http.MethodGet {
		snap := h.Scheduler.Snapshot()
		writeJSON(w, snap)
		return
	}

	// /api/dag 或 /api/dag/
	if path == "" {
		switch r.Method {
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			dags, err := h.Store.ListDAGs(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, dags)
			return
		case http.MethodPost:
			var d dag.DAG
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
				return
			}
			if d.ID == "" {
				http.Error(w, "id is required", http.StatusBadRequest)
				return
			}
			if d.HasCycle() {
				http.Error(w, "dag has cycle", http.StatusBadRequest)
				return
			}
			now := time.Now()
			if d.CreatedAt.IsZero() {
				d.CreatedAt = now
			}
			d.UpdatedAt = now
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := h.Store.SaveDAG(ctx, &d); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, d)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// /api/dag/{id}[/trigger]
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	if len(parts) == 2 && parts[1] == "trigger" && r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := h.Scheduler.Trigger(ctx, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"id": id, "triggered": true})
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			d, err := h.Store.GetDAG(ctx, id)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, d)
			return
		case http.MethodDelete:
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := h.Store.DeleteDAG(ctx, id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"id": id, "deleted": true})
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	http.NotFound(w, r)
}

// writeJSON 写入 JSON 响应。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
