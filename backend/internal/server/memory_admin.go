package server

// memory_admin.go —— 长期记忆条目管理 API（2026-10-24 长期记忆撤回与污染修复 §9.1）。
//
// 端点（全部走鉴权组）：
//   GET  /api/memory/items?scope=&status=&project_key=&limit=
//   GET  /api/memory/items/:id
//   GET  /api/memory/items/:id/events
//   POST /api/memory/items/:id/retract      {"reason":"..."}
//   POST /api/memory/items/:id/quarantine   {"reason":"..."}
//   POST /api/memory/items/:id/restore      {"reason":"..."}
//
// 设计约束（与计划一致）：
//   - 所有写操作记 actor/reason，落 long_term_memory_events（只追加，不物理删历史）；
//   - restore 只针对具体条目，不提供"清空全部墓碑"；
//   - 不提供物理删除接口；页面可区分有效/候选/已隔离/已替换/已撤回。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/store"
)

// SetMemoryLifecycle 注入长期记忆生命周期服务（bootstrap 装配；未注入时端点 503）。
func (h *APIHandler) SetMemoryLifecycle(lc *agent.MemoryLifecycle) {
	h.memoryLifecycle = lc
}

// memoryOwner 当前单租户 owner 为空串（多租户预留：后续从鉴权主体取）。
func memoryOwner(c *gin.Context) string {
	if v := strings.TrimSpace(c.Query("owner")); v != "" {
		return v
	}
	return ""
}

// memoryLifecycleReady 校验服务已装配；未装配写 503 并返回 false。
func (h *APIHandler) memoryLifecycleReady(c *gin.Context) bool {
	if h.memoryLifecycle == nil || !h.memoryLifecycle.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "长期记忆生命周期服务未装配或已关闭（memory.retraction_mode=off）"})
		return false
	}
	return true
}

// ListMemoryItemsHandler GET /api/memory/items — 列出条目（默认全部状态）。
func (h *APIHandler) ListMemoryItemsHandler(c *gin.Context) {
	if !h.memoryLifecycleReady(c) {
		return
	}
	scope := strings.TrimSpace(c.Query("scope"))
	projectKey := strings.TrimSpace(c.Query("project_key"))
	if scope == store.MemoryScopeProject && projectKey != "" {
		projectKey = store.NormalizeProjectKey(projectKey)
	}
	var statuses []string
	if s := strings.TrimSpace(c.Query("status")); s != "" {
		for _, part := range strings.Split(s, ",") {
			if p := strings.TrimSpace(part); p != "" {
				statuses = append(statuses, p)
			}
		}
	}
	limit := 200
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	items, err := h.memoryLifecycle.ListItems(c.Request.Context(), store.MemoryQuery{
		Owner: memoryOwner(c), Scope: scope, ProjectKey: projectKey,
		Statuses: statuses, Limit: limit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []store.MemoryItem{}
	}
	c.JSON(http.StatusOK, gin.H{"count": len(items), "items": items,
		"revision": h.memoryLifecycle.Revision(c.Request.Context(), memoryOwner(c))})
}

// GetMemoryItemHandler GET /api/memory/items/:id — 单条详情。
func (h *APIHandler) GetMemoryItemHandler(c *gin.Context) {
	if !h.memoryLifecycleReady(c) {
		return
	}
	owner := memoryOwner(c)
	repo := h.memoryLifecycle.Repo()
	item, err := repo.GetItem(c.Request.Context(), owner, c.Param("id"))
	if err != nil {
		if errors.Is(err, store.ErrMemoryItemNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "条目不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"item": item})
}

// ListMemoryItemEventsHandler GET /api/memory/items/:id/events — 事件时间线。
func (h *APIHandler) ListMemoryItemEventsHandler(c *gin.Context) {
	if !h.memoryLifecycleReady(c) {
		return
	}
	events, err := h.memoryLifecycle.ListEvents(c.Request.Context(), memoryOwner(c), c.Param("id"), 200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if events == nil {
		events = []store.MemoryEvent{}
	}
	c.JSON(http.StatusOK, gin.H{"count": len(events), "events": events})
}

// RetractMemoryItemHandler POST /api/memory/items/:id/retract — 撤回条目。
func (h *APIHandler) RetractMemoryItemHandler(c *gin.Context) {
	h.memoryStatusHandler(c, store.MemoryStatusRetracted)
}

// QuarantineMemoryItemHandler POST /api/memory/items/:id/quarantine — 隔离条目（立即不注入）。
func (h *APIHandler) QuarantineMemoryItemHandler(c *gin.Context) {
	h.memoryStatusHandler(c, store.MemoryStatusQuarantined)
}

// RestoreMemoryItemHandler POST /api/memory/items/:id/restore — 恢复条目（针对具体条目）。
func (h *APIHandler) RestoreMemoryItemHandler(c *gin.Context) {
	h.memoryStatusHandler(c, store.MemoryStatusActive)
}

// memoryStatusHandler 统一状态迁移入口（撤回/隔离/恢复），写事件后返回最新条目。
func (h *APIHandler) memoryStatusHandler(c *gin.Context, status string) {
	if !h.memoryLifecycleReady(c) {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	// 允许空 body（页面一键撤回时可以不填原因）。
	_ = c.ShouldBindJSON(&body)
	reason := strings.TrimSpace(body.Reason)
	if reason == "" {
		reason = "管理接口操作"
	}
	owner, id := memoryOwner(c), c.Param("id")
	var (
		item *store.MemoryItem
		err  error
	)
	switch status {
	case store.MemoryStatusRetracted:
		item, err = h.memoryLifecycle.RetractByID(c.Request.Context(), owner, id, reason)
	case store.MemoryStatusQuarantined:
		item, err = h.memoryLifecycle.QuarantineByID(c.Request.Context(), owner, id, reason)
	case store.MemoryStatusActive:
		item, err = h.memoryLifecycle.RestoreByID(c.Request.Context(), owner, id, reason)
	}
	if err != nil {
		if errors.Is(err, store.ErrMemoryItemNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "条目不存在"})
			return
		}
		if errors.Is(err, store.ErrMemoryRevisionConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "条目已被其他会话修改（revision 冲突），请刷新后重试"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "item": item})
}
