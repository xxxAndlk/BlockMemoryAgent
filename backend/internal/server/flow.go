package server

// flow.go 可视化编排（flow）HTTP 端点，挂 /api/flow/*。
// 定义真相源 = 工作区 .bma/flows/<id>.json（经 GET/PUT 读写）；运行经 POST /flow/runs
// 发起、GET /flow/runs/:id 轮询 trace。全部端点走 AuthMiddleware（router.go 的 api 组）；
// 引擎未接线时 503（照 workflow.go 先例）。

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/flow"
)

// FlowHandler flow 端点处理器。
type FlowHandler struct {
	Engine *flow.Engine
	// workdirOf 按会话解析有效工作目录（可选接线；POST /runs 未带 workdir 时兜底）。
	workdirOf sessionWorkDirProvider
}

// NewFlowHandler 构造处理器。
func NewFlowHandler(eng *flow.Engine) *FlowHandler {
	return &FlowHandler{Engine: eng}
}

// SetSessionWorkDirProvider 注入会话工作目录解析器（bootstrap 传 agentSvc）。
func (h *FlowHandler) SetSessionWorkDirProvider(p sessionWorkDirProvider) {
	h.workdirOf = p
}

// RegisterRoutes 注册 /flow/* 路由（挂在 /api 鉴权组下）。
func (h *FlowHandler) RegisterRoutes(rg gin.IRouter) {
	rg.GET("/flow/flows", h.ListFlows)
	rg.GET("/flow/flows/:id", h.GetFlow)
	rg.PUT("/flow/flows/:id", h.SaveFlow)
	rg.POST("/flow/runs", h.CreateRun)
	rg.GET("/flow/runs", h.ListRuns)
	rg.GET("/flow/runs/:id", h.GetRun)
	rg.POST("/flow/runs/:id/abort", h.AbortRun)
	rg.POST("/flow/runs/:id/nodes/:nid/decide", h.DecideNode)
}

// guard 引擎未接线 → 503。
func (h *FlowHandler) guard(c *gin.Context) bool {
	if h.Engine == nil {
		c.String(http.StatusServiceUnavailable, "flow engine disabled")
		return false
	}
	return true
}

// statusForFlowError 错误映射：哨兵 → 404；其余按输入/状态问题 → 400。
func statusForFlowError(err error) (int, string) {
	if errors.Is(err, flow.ErrRunNotFound) || errors.Is(err, flow.ErrNodeRunNotFound) {
		return http.StatusNotFound, err.Error()
	}
	return http.StatusBadRequest, err.Error()
}

// ListFlows GET /flow/flows?workdir= —— 扫 .bma/flows/*.json 返回摘要。
func (h *FlowHandler) ListFlows(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	workdir := strings.TrimSpace(c.Query("workdir"))
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required")
		return
	}
	flows, err := flow.ListFlows(workdir)
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"flows": flows})
}

// GetFlow GET /flow/flows/:id?workdir= —— flow JSON 全文（含 version）；
// 文件损坏仍返回原文供人工修复（照 GetRecipe 先例）。
func (h *FlowHandler) GetFlow(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	workdir := strings.TrimSpace(c.Query("workdir"))
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required")
		return
	}
	id := c.Param("id")
	path, err := flow.FlowFilePath(workdir, id)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			c.String(http.StatusNotFound, "flow %s 不存在", id)
			return
		}
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	f, err := flow.ParseFlow(data)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"id": id, "json": string(data), "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": f.ID, "version": f.Version, "name": f.Name, "json": string(data)})
}

// SaveFlow PUT /flow/flows/:id —— body {workdir, json}；解析校验后落盘（版本自动递增）。
func (h *FlowHandler) SaveFlow(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		Workdir string `json:"workdir"`
		JSON    string `json:"json"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.String(http.StatusBadRequest, "invalid json: %s", err.Error())
		return
	}
	if strings.TrimSpace(body.Workdir) == "" || strings.TrimSpace(body.JSON) == "" {
		c.String(http.StatusBadRequest, "workdir/json required")
		return
	}
	f, err := flow.SaveFlowJSON(body.Workdir, c.Param("id"), body.JSON)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": f.ID, "version": f.Version, "name": f.Name})
}

// CreateRun POST /flow/runs —— body {workdir, flow_id, inputs, session_id?}。
func (h *FlowHandler) CreateRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		Workdir   string         `json:"workdir"`
		FlowID    string         `json:"flow_id"`
		Inputs    map[string]any `json:"inputs"`
		SessionID string         `json:"session_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.String(http.StatusBadRequest, "invalid json: %s", err.Error())
		return
	}
	if strings.TrimSpace(body.FlowID) == "" {
		c.String(http.StatusBadRequest, "flow_id required")
		return
	}
	workdir := strings.TrimSpace(body.Workdir)
	if workdir == "" && body.SessionID != "" && h.workdirOf != nil {
		workdir = h.workdirOf.SessionWorkDir(body.SessionID)
	}
	if workdir == "" {
		c.String(http.StatusBadRequest, "workdir required（或提供有效 session_id 解析）")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	run, err := h.Engine.CreateRun(ctx, flow.CreateSpec{
		Workdir: workdir, FlowID: body.FlowID, Inputs: body.Inputs, SessionID: body.SessionID,
	})
	if err != nil {
		status, msg := statusForFlowError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": run.ID, "status": run.Status})
}

// ListRuns GET /flow/runs?flow_id=&status=&limit=
func (h *FlowHandler) ListRuns(c *gin.Context) {
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
	runs, err := h.Engine.ListRuns(ctx, flow.RunFilter{
		FlowID: c.Query("flow_id"), Status: c.Query("status"), Limit: limit,
	})
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

// GetRun GET /flow/runs/:id —— run 行 + 全部 node_run（调试面板数据源）。
func (h *FlowHandler) GetRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	detail, err := h.Engine.GetRunDetail(ctx, c.Param("id"))
	if err != nil {
		status, msg := statusForFlowError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, detail)
}

// AbortRun POST /flow/runs/:id/abort —— 中止。
func (h *FlowHandler) AbortRun(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Engine.AbortRun(ctx, c.Param("id")); err != nil {
		status, msg := statusForFlowError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"aborted": true})
}

// DecideNode POST /flow/runs/:id/nodes/:nid/decide —— body {approved, comment?}；
// approval 节点裁决：通过继续走，拒绝 run 失败。
func (h *FlowHandler) DecideNode(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var body struct {
		Approved bool   `json:"approved"`
		Comment  string `json:"comment"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.String(http.StatusBadRequest, "invalid json: %s", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Engine.DecideNode(ctx, c.Param("id"), c.Param("nid"), body.Approved, body.Comment); err != nil {
		status, msg := statusForFlowError(err)
		c.String(status, "%s", msg)
		return
	}
	c.JSON(http.StatusOK, gin.H{"decided": true})
}
