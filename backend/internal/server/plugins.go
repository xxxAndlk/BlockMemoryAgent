package server

// plugins.go 实现插件管理 API（设计文档 §5，Gin 版）：
//   - GET  /api/plugins                  — 列表（状态、工具清单、缺失 env 提示）
//   - GET  /api/plugins/{id}            — 单个插件详情
//   - POST /api/plugins/{id}/enable     — 热启用
//   - POST /api/plugins/{id}/disable    — 热停用
//   - POST /api/plugins/reload          — 重读 plugins.yaml + 重扫 plugins.d/
//
// 全部端点经 GinAuthMiddleware 保护（bootstrap/router.go 统一挂组中间件）。

import (
	"context" // 请求上下文
	"time"    // 请求超时

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/plugins" // 插件管理器
)

// SetPluginManager 注入插件管理器（bootstrap 装配后调用）。
func (h *APIHandler) SetPluginManager(mgr *plugins.Manager) {
	h.pluginMgr = mgr
}

// ListPluginsHandler 处理 GET /api/plugins — 返回全部插件信息。
func (h *APIHandler) ListPluginsHandler(c *gin.Context) {
	if h.pluginMgr == nil {
		c.String(503, "plugin manager not initialized")
		return
	}
	c.JSON(200, map[string]any{"plugins": h.pluginMgr.List()})
}

// GetPluginHandler 处理 GET /api/plugins/{id} — 返回单个插件详情。
func (h *APIHandler) GetPluginHandler(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.String(400, "plugin id required")
		return
	}
	info, ok := h.pluginMgr.Get(id)
	if !ok {
		c.String(404, "plugin not found")
		return
	}
	c.JSON(200, info)
}

// EnablePluginHandler 处理 POST /api/plugins/{id}/enable — 热启用插件。
func (h *APIHandler) EnablePluginHandler(c *gin.Context) {
	h.mutatePlugin(c, true)
}

// DisablePluginHandler 处理 POST /api/plugins/{id}/disable — 热停用插件。
func (h *APIHandler) DisablePluginHandler(c *gin.Context) {
	h.mutatePlugin(c, false)
}

// mutatePlugin enable/disable 共用实现：执行后返回更新后的插件信息。
func (h *APIHandler) mutatePlugin(c *gin.Context, enable bool) {
	if h.pluginMgr == nil {
		c.String(503, "plugin manager not initialized")
		return
	}
	id := c.Param("id")
	if id == "" {
		c.String(400, "plugin id required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	var err error
	if enable {
		err = h.pluginMgr.Enable(ctx, id)
	} else {
		err = h.pluginMgr.Disable(ctx, id)
	}
	if err != nil {
		c.String(400, "%s", err.Error())
		return
	}
	info, _ := h.pluginMgr.Get(id)
	c.JSON(200, info)
}

// ReloadPluginsHandler 处理 POST /api/plugins/reload — 重读配置 + 重扫 plugins.d/。
func (h *APIHandler) ReloadPluginsHandler(c *gin.Context) {
	if h.pluginMgr == nil {
		c.String(503, "plugin manager not initialized")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	if err := h.pluginMgr.Reload(ctx); err != nil {
		c.String(400, "%s", err.Error())
		return
	}
	c.JSON(200, map[string]any{"status": "ok", "plugins": h.pluginMgr.List()})
}
