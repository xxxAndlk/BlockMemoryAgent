package server

// workflow.go 工作流量产 HTTP 端点（开发文档 §8，挂 /api/workflow/*）。
// 页面是一级入口而非会话子资源；产物经 GET /api/sessions/:id/workspace/*path 既有端点服务。
// 全部端点走 AuthMiddleware（router.go 的 api 组）；引擎未接线时 503（照 dag.go 先例）。

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/workflow"
)

// WorkflowHandler 工作流量产端点处理器。
type WorkflowHandler struct {
	Engine *workflow.Engine
	// workdirOf 按会话解析有效工作目录（可选接线；POST /runs 未带 workdir 时兜底）。
	workdirOf sessionWorkDirProvider
}

// NewWorkflowHandler 构造处理器。
func NewWorkflowHandler(eng *workflow.Engine) *WorkflowHandler {
	return &WorkflowHandler{Engine: eng}
}

// SetSessionWorkDirProvider 注入会话工作目录解析器（bootstrap 传 agentSvc）。
func (h *WorkflowHandler) SetSessionWorkDirProvider(p sessionWorkDirProvider) {
	h.workdirOf = p
}

// RegisterRoutes 注册 /workflow/* 路由（挂在 /api 鉴权组下）。
func (h *WorkflowHandler) RegisterRoutes(rg gin.IRouter) {
	rg.GET("/workflow/recipes", h.ListRecipes)
	rg.GET("/workflow/recipes/:id", h.GetRecipe)
	rg.PUT("/workflow/recipes/:id", h.SaveRecipe)
	rg.POST("/workflow/runs", h.CreateRun)
	rg.GET("/workflow/runs", h.ListRuns)
	rg.GET("/workflow/runs/:id", h.GetRun)
	rg.POST("/workflow/runs/:id/pause", h.PauseRun)
	rg.POST("/workflow/runs/:id/resume", h.ResumeRun)
	rg.POST("/workflow/runs/:id/abort", h.AbortRun)
	rg.POST("/workflow/runs/:id/feedback", h.Feedback)
	rg.POST("/workflow/units/:uid/:action", h.UnitAction)
}

// guard 引擎未接线 → 503。
func (h *WorkflowHandler) guard(c *gin.Context) bool {
	if h.Engine == nil {
		c.String(http.StatusServiceUnavailable, "workflow engine disabled")
		return false
	}
	return true
}

// statusForError 错误映射：哨兵 → 404；其余按输入/状态问题 → 400。
func statusForError(err error) (int, string) {
	if errors.Is(err, workflow.ErrRunNotFound) || errors.Is(err, workflow.ErrUnitNotFound) {
		return http.StatusNotFound, err.Error()
	}
	return http.StatusBadRequest, err.Error()
}

// ListRecipes GET /workflow/recipes?workdir= —— 扫 .bma/workflows/*.yaml 返回摘要。
func (h *WorkflowHandler) ListRecipes(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	workdir := strings.TrimSpace(c.Query("workdir"))
	if workdir == "" && h.workdirOf != nil {
		workdir = c.Query("session_id")
		if workdir != "" {
			workdir = h.workdirOf.SessionWorkDir(workdir)
		}
	}
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required")
		return
	}
	recipes, err := workflow.ListRecipes(workdir)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"recipes": recipes})
}

// GetRecipe GET /workflow/recipes/:id?workdir= —— 配方 YAML 全文（含 version）。
func (h *WorkflowHandler) GetRecipe(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	workdir := strings.TrimSpace(c.Query("workdir"))
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required")
		return
	}
	id := c.Param("id")
	path, err := workflow.RecipeFilePath(workdir, id)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.String(http.StatusNotFound, "配方 %s 不存在", id)
			return
		}
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	recipe, err := workflow.ParseRecipe(data)
	if err != nil {
		// 文件存在但损坏：仍返回原文供人工修复。
		c.JSON(http.StatusOK, gin.H{"id": id, "yaml": string(data), "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": recipe.ID, "version": recipe.Version, "title": recipe.Title, "yaml": string(data)})
}

// SaveRecipe PUT /workflow/recipes/:id —— body {workdir, yaml}；落盘（版本自动递增）。
func (h *WorkflowHandler) SaveRecipe(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		Workdir string `json:"workdir"`
		YAML    string `json:"yaml"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.String(http.StatusBadRequest, "invalid json: %s", err.Error())
		return
	}
	if strings.TrimSpace(body.Workdir) == "" || strings.TrimSpace(body.YAML) == "" {
		c.String(http.StatusBadRequest, "workdir/yaml required")
		return
	}
	recipe, err := workflow.SaveRecipeYAML(body.Workdir, body.YAML)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	if recipe.ID != c.Param("id") {
		c.String(http.StatusBadRequest, "YAML 的 id=%s 与路径 :id=%s 不一致", recipe.ID, c.Param("id"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": recipe.ID, "version": recipe.Version, "title": recipe.Title})
}

// CreateRun POST /workflow/runs —— 建 run（web/脚本入口；对话内发起走 workflow_run 工具带审批门）。
func (h *WorkflowHandler) CreateRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		SessionID   string `json:"session_id"`
		Workdir     string `json:"workdir"`
		RecipeID    string `json:"recipe_id"`
		Roster      string `json:"roster"`
		BudgetCap   int    `json:"budget_cap"`
		Concurrency int    `json:"concurrency"`
		FailFast    *bool  `json:"fail_fast"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.String(http.StatusBadRequest, "invalid json: %s", err.Error())
		return
	}
	if strings.TrimSpace(body.SessionID) == "" || strings.TrimSpace(body.RecipeID) == "" {
		c.String(http.StatusBadRequest, "session_id/recipe_id required")
		return
	}
	workdir := strings.TrimSpace(body.Workdir)
	if workdir == "" && h.workdirOf != nil {
		workdir = h.workdirOf.SessionWorkDir(body.SessionID)
	}
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required（或提供有效 session_id 解析）")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	spec := workflow.CreateSpec{
		SessionID: body.SessionID, Workdir: workdir, RecipeID: body.RecipeID,
		RosterRaw: body.Roster, BudgetCap: body.BudgetCap, Concurrency: body.Concurrency, FailFast: body.FailFast,
	}
	run, est, err := h.Engine.CreateRun(ctx, spec)
	if err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": run.ID, "estimate": est})
}

// ListRuns GET /workflow/runs?session_id=&status=&limit=
// 每行附 counts（cell 口径最新 attempt 状态计数），列表页进度展示用。
func (h *WorkflowHandler) ListRuns(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	runs, err := h.Engine.ListRuns(ctx, workflow.RunFilter{
		SessionID: c.Query("session_id"), Status: c.Query("status"), Limit: limit,
	})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	type runWithCounts struct {
		*workflow.Run
		Counts map[string]int `json:"counts"`
	}
	out := make([]runWithCounts, 0, len(runs))
	for _, r := range runs {
		counts, cerr := h.Engine.UnitCounts(ctx, r.ID)
		if cerr != nil {
			counts = map[string]int{}
		}
		out = append(out, runWithCounts{Run: r, Counts: counts})
	}
	c.JSON(http.StatusOK, gin.H{"runs": out})
}

// GetRun GET /workflow/runs/:id —— run 行 + 全部 units（名册×阶段网格数据源）+ 配方快照。
func (h *WorkflowHandler) GetRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	detail, err := h.Engine.GetRunDetail(ctx, c.Param("id"))
	if err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": detail.Run, "units": detail.Units})
}

// PauseRun POST /workflow/runs/:id/pause
func (h *WorkflowHandler) PauseRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Engine.PauseRun(ctx, c.Param("id")); err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "paused": true})
}

// ResumeRun POST /workflow/runs/:id/resume —— body 可带 extra_budget（预算暂停提额）。
func (h *WorkflowHandler) ResumeRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		ExtraBudget int `json:"extra_budget"`
	}
	_ = c.ShouldBindJSON(&body) // 允许空 body
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Engine.ResumeRun(ctx, c.Param("id"), body.ExtraBudget); err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "resumed": true})
}

// AbortRun POST /workflow/runs/:id/abort
func (h *WorkflowHandler) AbortRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Engine.AbortRun(ctx, c.Param("id")); err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "aborted": true})
}

// UnitAction POST /workflow/units/:uid/:action —— accept / reject / reroll / skip（reject 必带 reason）。
func (h *WorkflowHandler) UnitAction(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	action := c.Param("action")
	switch action {
	case workflow.UnitActionAccept, workflow.UnitActionReject, workflow.UnitActionReroll, workflow.UnitActionSkip:
	default:
		c.String(http.StatusBadRequest, "未知 unit 操作: %s（accept|reject|reroll|skip）", action)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&body)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.Engine.UnitAction(ctx, c.Param("uid"), action, body.Reason); err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"unit_id": c.Param("uid"), "action": action})
}

// Feedback POST /workflow/runs/:id/feedback —— 聚合驳回理由 → 配方 v+1 草案（未落盘）。
func (h *WorkflowHandler) Feedback(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	// 草案生成含一次轻量模型调用：给足超时（模型侧单次 30s×重试）。
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	runID := c.Param("id")
	draft, err := h.Engine.FeedbackDraft(ctx, runID)
	if err != nil {
		status, msg := statusForError(err)
		c.String(status, "%s", msg)
		return
	}
	detail, err := h.Engine.GetRunDetail(ctx, runID)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": runID, "draft": draft, "current": detail.Run.RecipeYAML})
}
