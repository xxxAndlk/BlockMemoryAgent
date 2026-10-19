package server

// directive.go 会话"指令模式"（goal 目标 / loop 循环）HTTP 处理器（2026-10-18）。
// 挂在 /api/sessions/:id/directive（GET 查询 / PUT 切换），管理器未接线时 503。

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/goal"
)

// DirectiveHandler 是会话指令模式的 HTTP 处理器。
type DirectiveHandler struct {
	svc *goal.Manager
}

// NewDirectiveHandler 构造处理器；svc 为 nil 时端点返回 503。
func NewDirectiveHandler(svc *goal.Manager) *DirectiveHandler {
	return &DirectiveHandler{svc: svc}
}

// RegisterRoutes 注册 /sessions/:id/directive 到给定路由组。
func (h *DirectiveHandler) RegisterRoutes(rg gin.IRouter) {
	rg.GET("/sessions/:id/directive", h.Get)
	rg.PUT("/sessions/:id/directive", h.Set)
}

// guard 未接线时 503。
func (h *DirectiveHandler) guard(c *gin.Context) bool {
	if h == nil || h.svc == nil {
		c.String(http.StatusServiceUnavailable, "指令模式未接线（goal 管理器未装配）")
		return false
	}
	return true
}

// Get 查询会话当前指令 + 未触发提醒。
func (h *DirectiveHandler) Get(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	v, err := h.svc.GetView(ctx, c.Param("id"))
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, v)
}

// Set 应用/切换会话指令模式。body 同 agent.DirectiveSpec；
// mode=normal 停止指令；goal/loop 必填 objective 由调用方（前端）或消息内容回落。
func (h *DirectiveHandler) Set(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	spec, err := DecodeBody[agent.DirectiveSpec](c.Request)
	if err != nil {
		c.String(http.StatusBadRequest, "请求体无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.svc.ApplyDirective(ctx, c.Param("id"), &spec); err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	v, err := h.svc.GetView(ctx, c.Param("id"))
	if err != nil {
		c.String(http.StatusInternalServerError, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, v)
}
