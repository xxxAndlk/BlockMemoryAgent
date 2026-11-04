package server

// team_admin.go C-03 自动调度管理 API。
//
//   GET  /api/team/attempts?session_id=&status=&limit=
//   POST /api/team/reconcile  {"session_id":"..."}  // 手动触发一次确定性调度

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/team"
)

// SetTeamDispatch 注入 C-03 attempt 存储与调度器（未启用自动调度时为 nil，端点 503）。
func (h *APIHandler) SetTeamDispatch(store team.AttemptStore, scheduler *team.ReadyScheduler) {
	h.teamAttempts = store
	h.teamScheduler = scheduler
}

func (h *APIHandler) teamAttemptsReady(c *gin.Context) bool {
	if h.teamAttempts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auto dispatch disabled or attempt store not wired"})
		return false
	}
	return true
}

// ListTeamAttemptsHandler GET /api/team/attempts — 列出会话的自动派发 attempt。
func (h *APIHandler) ListTeamAttemptsHandler(c *gin.Context) {
	if !h.teamAttemptsReady(c) {
		return
	}
	sid := strings.TrimSpace(c.Query("session_id"))
	if sid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}
	all, err := h.teamAttempts.ListBySession(c.Request.Context(), sid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	limit := 200
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	out := make([]team.DispatchAttempt, 0, len(all))
	for _, a := range all {
		if status != "" && a.Status != status {
			continue
		}
		out = append(out, a)
		if len(out) >= limit {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"session_id": sid, "count": len(out), "attempts": out})
}

// ApproveTeamPlanHandler POST /api/team/approve — 人工批准当前计划版本。
// write_plan 只落计划；未批准的版本不会被 ReadyScheduler 自动执行。
func (h *APIHandler) ApproveTeamPlanHandler(c *gin.Context) {
	if h.rt == nil || h.rt.Boards == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "board manager not wired"})
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = c.ShouldBindJSON(&body)
	sid := strings.TrimSpace(body.SessionID)
	if sid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}
	b := h.rt.Boards.GetRestored(sid)
	if b == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "board not found"})
		return
	}
	if !b.ApprovePlan() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plan empty or not ready for approval"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_id": sid, "plan_version": b.Snapshot().PlanVersion, "approved": true})
}

// ReconcileTeamHandler POST /api/team/reconcile — 手动触发一次调度（调试/运维）。
func (h *APIHandler) ReconcileTeamHandler(c *gin.Context) {
	if h.teamScheduler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auto dispatch disabled"})
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = c.ShouldBindJSON(&body)
	sid := strings.TrimSpace(body.SessionID)
	if sid != "" {
		if err := h.teamScheduler.Reconcile(c.Request.Context(), sid); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"reconciled": []string{sid}})
		return
	}
	var done []string
	for _, s := range h.teamScheduler.Sessions() {
		if err := h.teamScheduler.Reconcile(c.Request.Context(), s); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "session_id": s})
			return
		}
		done = append(done, s)
	}
	c.JSON(http.StatusOK, gin.H{"reconciled": done})
}
