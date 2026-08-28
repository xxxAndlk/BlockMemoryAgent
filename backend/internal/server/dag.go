// Package server dag.go 实现特性1 DAG 调度的 HTTP 接口（Gin 版）。
//
// 路由（由 RegisterRoutes 注册，路径相对传入的 gin.IRouter）：
//   - GET    /dag           列出全部 DAG
//   - POST   /dag           创建/更新 DAG（按 id upsert）
//   - GET    /dag/running   查看运行中 DAG 快照
//   - GET    /dag/{id}      取单条 DAG
//   - DELETE /dag/{id}      删除 DAG
//   - POST   /dag/{id}/trigger  立即触发一次 DAG
package server

import (
	"context" // 请求上下文与超时
	"time"    // 超时与更新时间

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/dag" // DAG 领域对象与调度器
)

// DAGHandler DAG HTTP 处理器。
type DAGHandler struct {
	Store     dag.Store      // DAG 持久化存储
	Scheduler *dag.Scheduler // DAG 调度器
}

// NewDAGHandler 创建处理器。store/scheduler 由 bootstrap 注入。
// 参数 store：DAG 存储；sched：调度器。
// 返回值：*DAGHandler。
func NewDAGHandler(store dag.Store, sched *dag.Scheduler) *DAGHandler {
	return &DAGHandler{Store: store, Scheduler: sched}
}

// RegisterRoutes 把 DAG 全部端点注册到指定路由器（组或引擎）。
// 路径使用相对形式（/dag...），调用方决定是否拼接 /api 前缀与鉴权中间件。
// 参数 rg：Gin 路由器（*gin.Engine 或 *gin.RouterGroup）。
func (h *DAGHandler) RegisterRoutes(rg gin.IRouter) {
	// /dag/running 必须与 /dag/{id} 同级注册：Gin 的路由树静态段优先于参数段，
	// "running" 不会被误当作 id。
	rg.GET("/dag", h.ListDAGs)
	rg.POST("/dag", h.SaveDAG)
	rg.GET("/dag/running", h.RunningSnapshot)
	rg.GET("/dag/:id", h.GetDAG)
	rg.DELETE("/dag/:id", h.DeleteDAG)
	rg.POST("/dag/:id/trigger", h.TriggerDAG)
}

// guard 可用性守卫：DAG 未启用时 scheduler 为 nil，避免 nil 解引用 panic。
// 返回值：bool - true 表示可用；false 表示已写出 503 响应，调用方应立即返回。
func (h *DAGHandler) guard(c *gin.Context) bool {
	if h.Scheduler == nil {
		c.String(503, "dag scheduler disabled")
		return false
	}
	return true
}

// RunningSnapshot 处理 GET /api/dag/running — 返回运行中 DAG 快照。
func (h *DAGHandler) RunningSnapshot(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	c.JSON(200, h.Scheduler.Snapshot())
}

// ListDAGs 处理 GET /api/dag — 列出全部 DAG。
func (h *DAGHandler) ListDAGs(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	dags, err := h.Store.ListDAGs(ctx)
	if err != nil {
		c.String(500, "%s", err.Error())
		return
	}
	c.JSON(200, dags)
}

// SaveDAG 处理 POST /api/dag — 创建/更新 DAG（按 d.ID upsert）。
func (h *DAGHandler) SaveDAG(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	var d dag.DAG
	if err := c.ShouldBindJSON(&d); err != nil {
		c.String(400, "invalid json: %s", err.Error())
		return
	}
	if d.ID == "" {
		c.String(400, "id is required")
		return
	}
	// 环检测：拒绝持久化带环的 DAG，避免调度器进入死锁
	if d.HasCycle() {
		c.String(400, "dag has cycle")
		return
	}
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now // 每次 upsert 都刷新，作为 cron 触发判断的基准时间
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.Store.SaveDAG(ctx, &d); err != nil {
		c.String(500, "%s", err.Error())
		return
	}
	c.JSON(200, d)
}

// GetDAG 处理 GET /api/dag/{id} — 取单条 DAG。
func (h *DAGHandler) GetDAG(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	d, err := h.Store.GetDAG(ctx, id)
	if err != nil {
		c.String(404, "%s", err.Error())
		return
	}
	c.JSON(200, d)
}

// DeleteDAG 处理 DELETE /api/dag/{id} — 删除 DAG。
func (h *DAGHandler) DeleteDAG(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := h.Store.DeleteDAG(ctx, id); err != nil {
		c.String(500, "%s", err.Error())
		return
	}
	c.JSON(200, map[string]any{"id": id, "deleted": true})
}

// TriggerDAG 处理 POST /api/dag/{id}/trigger — 跳过 cron 检查立即派发一次。
func (h *DAGHandler) TriggerDAG(c *gin.Context) {
	if !h.guard(c) {
		return
	}
	id := c.Param("id")
	// trigger 内部可能阻塞（HasCycle+dispatch），给 5 秒超时
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := h.Scheduler.Trigger(ctx, id); err != nil {
		c.String(500, "%s", err.Error())
		return
	}
	c.JSON(200, map[string]any{"id": id, "triggered": true})
}
