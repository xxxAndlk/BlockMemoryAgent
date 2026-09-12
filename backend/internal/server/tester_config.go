package server

// 测试助手验收开关的工作目录级 API（测试助手大改，2026-09-12）：
// 读写 <workDir>/.bma/tester.yaml，workDir 解析方式仿项目偏好
//（SaveProjectPreferencesHandler：可选 work_dir 参数与 /api/sessions 同规则）。

import (
	"net/http" // HTTP 状态码

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/domain/subagent" // TesterConfig 与 .bma/tester.yaml 读写
	"github.com/blockmemory/agent/backend/internal/domain/tool"     // WorkDirFromContext
)

// testerWorkDir 解析 tester.yaml 所属工作目录：可选 work_dir 参数经 prefsWorkDirCtx
// 校验并注入 ctx（非空转绝对路径，不存在/非目录已在此处返回 400）；为空时回落
// 进程默认工作目录（bootstrap 经 SetDefaultWorkDir 注入）。
func (h *APIHandler) testerWorkDir(c *gin.Context, dir string) (string, bool) {
	ctx, ok := prefsWorkDirCtx(c, dir)
	if !ok {
		return "", false
	}
	wd := tool.WorkDirFromContext(ctx)
	if wd == "" {
		wd = h.defaultWorkDir
	}
	if wd == "" {
		c.String(http.StatusInternalServerError, "work dir not resolved")
		return "", false
	}
	return wd, true
}

// TesterConfigHandler 处理 GET /api/project/tester-config — 返回当前 workDir 的
// 测试助手开关（缺文件时返回默认值 {mode:off, auto_prompt:"", max_rounds:2}）。
func (h *APIHandler) TesterConfigHandler(c *gin.Context) {
	wd, ok := h.testerWorkDir(c, c.Query("work_dir"))
	if !ok {
		return
	}
	cfg := subagent.LoadTesterConfig(wd)
	c.JSON(http.StatusOK, map[string]any{
		"mode":        cfg.Mode,
		"auto_prompt": cfg.AutoPrompt,
		"max_rounds":  cfg.MaxRounds,
	})
}

// SaveTesterConfigHandler 处理 PUT /api/project/tester-config — 全量覆盖测试助手开关。
// body：{mode: off|auto|on, auto_prompt: string, max_rounds: int, work_dir?: string}。
func (h *APIHandler) SaveTesterConfigHandler(c *gin.Context) {
	var req struct {
		Mode       string `json:"mode"`
		AutoPrompt string `json:"auto_prompt"`
		MaxRounds  int    `json:"max_rounds"`
		WorkDir    string `json:"work_dir,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	wd, ok := h.testerWorkDir(c, req.WorkDir)
	if !ok {
		return
	}
	cfg := subagent.TesterConfig{Mode: req.Mode, AutoPrompt: req.AutoPrompt, MaxRounds: req.MaxRounds}
	if err := subagent.SaveTesterConfig(wd, cfg); err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, map[string]any{"ok": true})
}
