package server

// self_claim.go C-06 自认领管理 API（默认关闭，未启用时 503）。
//
//   POST /api/team/self-claim/plan   冻结 self_claim 计划
//   GET  /api/team/self-claim/tasks?session_id=
//   POST /api/team/self-claim/run    立即触发一次 Worker 认领扫描

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/team"
)

// SetSelfClaim 注入 C-06 服务/存储/WorkerSupervisor。
func (h *APIHandler) SetSelfClaim(svc *team.ClaimService, store team.ClaimStore, sup *team.WorkerSupervisor) {
	h.selfClaim = svc
	h.selfClaimStore = store
	h.selfClaimSup = sup
}

func (h *APIHandler) selfClaimReady(c *gin.Context) bool {
	if h.selfClaim == nil || h.selfClaimStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "self claim disabled"})
		return false
	}
	return true
}

// SelfClaimPlanHandler POST /api/team/self-claim/plan。
func (h *APIHandler) SelfClaimPlanHandler(c *gin.Context) {
	if !h.selfClaimReady(c) {
		return
	}
	var env team.PlanEnvelope
	if err := c.ShouldBindJSON(&env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := h.selfClaim.InitPlan(c.Request.Context(), env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"plan_version": env.PlanVersion, "session_id": env.SessionID, "tasks": len(env.Tasks)})
}

// SelfClaimTasksHandler GET /api/team/self-claim/tasks?session_id=。
func (h *APIHandler) SelfClaimTasksHandler(c *gin.Context) {
	if !h.selfClaimReady(c) {
		return
	}
	sid := strings.TrimSpace(c.Query("session_id"))
	if sid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id required"})
		return
	}
	tasks, err := h.selfClaimStore.ListTasks(c.Request.Context(), sid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_id": sid, "tasks": tasks})
}

// SelfClaimRunHandler POST /api/team/self-claim/run — 手动触发一次认领扫描。
func (h *APIHandler) SelfClaimRunHandler(c *gin.Context) {
	if h.selfClaimSup == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "self claim supervisor not wired"})
		return
	}
	h.selfClaimSup.RunOnce(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
