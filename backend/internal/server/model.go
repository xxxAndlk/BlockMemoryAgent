package server

// 模型动态切换 API：GET /api/models（目录）+ POST /api/models/switch（切换）。
// 逻辑全部委托 agent.ModelManager（ReactService → ModelFactory），本层只做
// HTTP 编解码与超时控制；TUI 不走此端点（进程内直调同一接口）。

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/gin-gonic/gin"
)

// SetModelManager 注入模型管理能力（bootstrap 传 app.Agent，其具体实现实现了该接口）。
func (h *APIHandler) SetModelManager(mgr agent.ModelManager) {
	h.modelMgr = mgr
}

// ListModelsHandler 处理 GET /api/models — 返回可切换预设 + 各角色当前模型状态。
func (h *APIHandler) ListModelsHandler(c *gin.Context) {
	if h.modelMgr == nil {
		c.String(http.StatusServiceUnavailable, "model manager not initialized")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	catalog, err := h.modelMgr.ListModels(ctx)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, catalog)
}

// SwitchModelHandler 处理 POST /api/models/switch — body {role, preset}。
// 切换含 60s 连通性探测（fail-closed），handler 超时给 70s 余量。
func (h *APIHandler) SwitchModelHandler(c *gin.Context) {
	if h.modelMgr == nil {
		c.String(http.StatusServiceUnavailable, "model manager not initialized")
		return
	}
	var req struct {
		Role   string `json:"role"`
		Preset string `json:"preset"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.String(http.StatusBadRequest, "invalid body: %s", err.Error())
		return
	}
	if req.Role == "" || req.Preset == "" {
		c.String(http.StatusBadRequest, "role and preset are required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 70*time.Second)
	defer cancel()
	cfg, err := h.modelMgr.SwitchModel(ctx, req.Role, req.Preset)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"status":   "ok",
		"role":     req.Role,
		"preset":   req.Preset,
		"provider": cfg.Provider,
		"model":    cfg.Model,
	})
}
