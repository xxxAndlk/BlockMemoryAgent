package server

// external_agents.go 实现外部 Agent 管理 API（doc/计划_外部Agent调用.md §4.5）：
//   - GET  /api/external-agents         — 注册表清单（含 exec.LookPath 安装状态）
//   - POST /api/external-agents/reload  — 重读 external_agents.yaml + SetExternalAgentDefs 热更新
//
// 全部端点经 GinAuthMiddleware 保护（bootstrap/router.go 统一挂组中间件）。

import (
	"os/exec"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// ExternalAgentView 是 GET /api/external-agents 返回的单条视图。
type ExternalAgentView struct {
	ID          string   `json:"id"`
	Command     string   `json:"command"`
	Args        []string `json:"args"`
	TimeoutSec  int      `json:"timeout_sec"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	// Installed 是 exec.LookPath(command) 结果：用户一眼看出哪台机器没装。
	Installed bool `json:"installed"`
}

// SetExternalAgentProvider 注入外部 Agent 清单读取与热更新闭包（bootstrap 装配；nil 时端点 503）。
func (h *APIHandler) SetExternalAgentProvider(list func() []tool.ExternalAgentDef, reload func() ([]tool.ExternalAgentDef, error)) {
	h.externalAgentList = list
	h.externalAgentReload = reload
}

// ListExternalAgentsHandler 处理 GET /api/external-agents — 返回注册表清单（含 disabled 条目）。
func (h *APIHandler) ListExternalAgentsHandler(c *gin.Context) {
	if h.externalAgentList == nil {
		c.String(503, "external agents not initialized")
		return
	}
	c.JSON(200, map[string]any{"agents": externalAgentViews(h.externalAgentList())})
}

// ReloadExternalAgentsHandler 处理 POST /api/external-agents/reload：
// 重读 yaml + 热更新注册表（Schema 描述下轮生效），返回最新清单。
func (h *APIHandler) ReloadExternalAgentsHandler(c *gin.Context) {
	if h.externalAgentReload == nil {
		c.String(503, "external agents reload not initialized")
		return
	}
	defs, err := h.externalAgentReload()
	if err != nil {
		c.String(400, "%s", err.Error())
		return
	}
	c.JSON(200, map[string]any{"status": "ok", "agents": externalAgentViews(defs)})
}

// externalAgentViews 转换条目并附 PATH 安装状态。
func externalAgentViews(defs []tool.ExternalAgentDef) []ExternalAgentView {
	out := make([]ExternalAgentView, 0, len(defs))
	for _, d := range defs {
		installed := false
		if d.Command != "" {
			if _, err := exec.LookPath(d.Command); err == nil {
				installed = true
			}
		}
		out = append(out, ExternalAgentView{
			ID: d.ID, Command: d.Command, Args: d.Args, TimeoutSec: d.TimeoutSec,
			Enabled: d.Enabled, Description: d.Description, Installed: installed,
		})
	}
	return out
}
