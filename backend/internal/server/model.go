package server

// 模型动态切换 API：GET /api/models（目录）+ POST /api/models/switch（切换）
// + POST /api/models（新增条目）。逻辑全部委托 agent.ModelManager
// （ReactService → ModelFactory），本层只做 HTTP 编解码与超时控制；
// TUI 不走此端点（进程内直调同一接口）。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/gin-gonic/gin"
)

// SetModelManager 注入模型管理能力（bootstrap 传 app.Agent，其具体实现实现了该接口）。
func (h *APIHandler) SetModelManager(mgr agent.ModelManager) {
	h.modelMgr = mgr
}

// ListModelsHandler 处理 GET /api/models — 返回模型注册表清单 + 各角色当前模型状态。
// 响应为视图结构，不含 api_key。
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

// SwitchModelHandler 处理 POST /api/models/switch — body {role, model_id, thinking}。
// 切换含 60s 连通性探测（fail-closed），handler 超时给 70s 余量。
func (h *APIHandler) SwitchModelHandler(c *gin.Context) {
	if h.modelMgr == nil {
		c.String(http.StatusServiceUnavailable, "model manager not initialized")
		return
	}
	var req struct {
		Role     string `json:"role"`
		ModelID  string `json:"model_id"`
		Thinking string `json:"thinking"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.String(http.StatusBadRequest, "invalid body: %s", err.Error())
		return
	}
	if req.Role == "" || req.ModelID == "" {
		c.String(http.StatusBadRequest, "role and model_id are required")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 70*time.Second)
	defer cancel()
	cfg, err := h.modelMgr.SwitchModel(ctx, req.Role, req.ModelID, req.Thinking)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"status":   "ok",
		"role":     req.Role,
		"model_id": req.ModelID,
		"provider": cfg.Provider,
		"model":    cfg.Model,
	})
}

// AddModelHandler 处理 POST /api/models — 新增模型条目（落 config/models.json）。
// body {name?, provider, model, api_key, base_url, max_output_tokens?, description?}；
// id 缺省由 model 名 slug 化生成，冲突时追加 -2/-3 后缀。
func (h *APIHandler) AddModelHandler(c *gin.Context) {
	if h.modelMgr == nil {
		c.String(http.StatusServiceUnavailable, "model manager not initialized")
		return
	}
	var req struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		APIKey          string `json:"api_key"`
		BaseURL         string `json:"base_url"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Description     string `json:"description"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		c.String(http.StatusBadRequest, "invalid body: %s", err.Error())
		return
	}
	if req.Provider == "" || req.Model == "" {
		c.String(http.StatusBadRequest, "provider and model are required")
		return
	}
	id := req.ID
	if id == "" {
		id = h.slugModelID(h.modelMgr, req.Model)
	}
	entry := types.ModelEntry{
		ID:              id,
		Name:            req.Name,
		Provider:        req.Provider,
		Model:           req.Model,
		APIKey:          req.APIKey,
		BaseURL:         req.BaseURL,
		MaxOutputTokens: req.MaxOutputTokens,
		Description:     req.Description,
	}
	if err := h.modelMgr.AddModelEntry(entry); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{"status": "ok", "id": entry.ID})
}

// slugModelID 由 model 名生成条目 ID（小写、非法字符转 -），冲突追加数字后缀。
// 可用清单来自目录接口（不含 api_key，足以判重）。
func (h *APIHandler) slugModelID(mgr agent.ModelManager, modelName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(modelName)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "model"
	}
	// 判重：拉目录；目录不可用则退回直接尝试（Add 会因重复报错）。
	taken := map[string]bool{}
	if catalog, err := mgr.ListModels(context.Background()); err == nil && catalog != nil {
		for _, m := range catalog.Models {
			taken[m.ID] = true
		}
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if !taken[candidate] {
			return candidate
		}
	}
}
