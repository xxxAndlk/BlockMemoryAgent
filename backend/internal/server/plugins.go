package server

// plugins.go 实现插件管理 API（设计文档 §5）：
//   - GET  /api/plugins                  — 列表（状态、工具清单、缺失 env 提示）
//   - GET  /api/plugins/{id}            — 单个插件详情
//   - POST /api/plugins/{id}/enable     — 热启用
//   - POST /api/plugins/{id}/disable    — 热停用
//   - POST /api/plugins/reload          — 重读 plugins.yaml + 重扫 plugins.d/
//
// 全部端点经 AuthMiddleware 保护（bootstrap/mux.go 统一包装）。

import (
	"context" // 请求上下文
	"net/http" // HTTP 处理器
	"time"    // 请求超时

	"github.com/blockmemory/agent/backend/internal/plugins" // 插件管理器
)

// SetPluginManager 注入插件管理器（bootstrap 装配后调用）。
func (h *APIHandler) SetPluginManager(mgr *plugins.Manager) {
	h.pluginMgr = mgr
}

// ListPluginsHandler 处理 GET /api/plugins — 返回全部插件信息。
func (h *APIHandler) ListPluginsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.pluginMgr == nil {
		http.Error(w, "plugin manager not initialized", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"plugins": h.pluginMgr.List()})
}

// GetPluginHandler 处理 GET /api/plugins/{id} — 返回单个插件详情。
func (h *APIHandler) GetPluginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "plugin id required", http.StatusBadRequest)
		return
	}
	info, ok := h.pluginMgr.Get(id)
	if !ok {
		http.Error(w, "plugin not found", http.StatusNotFound)
		return
	}
	writeJSON(w, info)
}

// EnablePluginHandler 处理 POST /api/plugins/{id}/enable — 热启用插件。
func (h *APIHandler) EnablePluginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mutatePlugin(w, r, true)
}

// DisablePluginHandler 处理 POST /api/plugins/{id}/disable — 热停用插件。
func (h *APIHandler) DisablePluginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mutatePlugin(w, r, false)
}

// mutatePlugin enable/disable 共用实现：执行后返回更新后的插件信息。
func (h *APIHandler) mutatePlugin(w http.ResponseWriter, r *http.Request, enable bool) {
	if h.pluginMgr == nil {
		http.Error(w, "plugin manager not initialized", http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "plugin id required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var err error
	if enable {
		err = h.pluginMgr.Enable(ctx, id)
	} else {
		err = h.pluginMgr.Disable(ctx, id)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, _ := h.pluginMgr.Get(id)
	writeJSON(w, info)
}

// ReloadPluginsHandler 处理 POST /api/plugins/reload — 重读配置 + 重扫 plugins.d/。
func (h *APIHandler) ReloadPluginsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.pluginMgr == nil {
		http.Error(w, "plugin manager not initialized", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := h.pluginMgr.Reload(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "plugins": h.pluginMgr.List()})
}
