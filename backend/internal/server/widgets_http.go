package server

// widgets_http.go 界面部件（Widget）管理 API（Gin 版）：
//   - GET  /api/widgets                 — 列表（Info JSON：id/name/version/description/entry/enabled/updated_at/files）
//   - GET  /api/widgets/{id}           — 单个部件详情
//   - POST /api/widgets/{id}/enable    — 热启用
//   - POST /api/widgets/{id}/disable   — 热停用
//   - POST /api/widgets/reload         — 重扫 widgets/ 目录
//   - GET  /api/widgets/{id}/files/*   — 部件文件服务（卡片 iframe 的渲染入口与附属资源）
//   - POST /api/widgets/push           — 推送瞬时数据 {session_id, widget_id, payload}
//   - POST /api/widgets/report         — 部件渲染错误回报 {session_id, widget_id, error}
//
// 全部端点经 GinAuthMiddleware 保护（bootstrap/router.go 统一挂组中间件）。
// push/report 面向 bridge 等进程内插件轮询调用：会话不存在一律静默 204，
// 推送方无需感知会话生命周期，也不会因会话结束而报错刷屏。

import (
	"net/http"      // HTTP 状态码
	"os"            // 文件 Stat/Open
	"path/filepath" // 路径清洗与越界校验
	"strings"       // 路径前缀判定

	"github.com/gin-gonic/gin" // Gin Web 框架

	"github.com/blockmemory/agent/backend/internal/widgets" // 部件目录包管理器
)

// widgetDataPusher 是"向会话部件瞬时数据通道写入"能力的鸭子类型接口
// （同 sessionWorkDirProvider 模式：只依赖一个方法，不动 agent.Agent 门面与测试 mock）。
// 由 *agent.ReactService 实现。
type widgetDataPusher interface {
	PushWidgetData(sessionID, widgetID, payload string)
}

// widgetErrorReporter 是"部件渲染错误回报"能力的鸭子类型接口（System 事件 + 邮箱 FYI）。
type widgetErrorReporter interface {
	ReportWidgetError(sessionID, widgetID, errText string)
}

// SetWidgetManager 注入部件管理器（bootstrap 装配后调用）。
func (h *APIHandler) SetWidgetManager(mgr *widgets.Manager) {
	h.widgetMgr = mgr
}

// ListWidgetsHandler 处理 GET /api/widgets — 返回全部部件信息。
func (h *APIHandler) ListWidgetsHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	c.JSON(http.StatusOK, map[string]any{"widgets": h.widgetMgr.List()})
}

// GetWidgetHandler 处理 GET /api/widgets/{id} — 返回单个部件详情（Info JSON）。
func (h *APIHandler) GetWidgetHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	info, ok := h.widgetMgr.Get(c.Param("id"))
	if !ok {
		c.String(http.StatusNotFound, "widget not found")
		return
	}
	c.JSON(http.StatusOK, info)
}

// EnableWidgetHandler 处理 POST /api/widgets/{id}/enable — 热启用部件。
func (h *APIHandler) EnableWidgetHandler(c *gin.Context) {
	h.mutateWidget(c, true)
}

// DisableWidgetHandler 处理 POST /api/widgets/{id}/disable — 热停用部件。
func (h *APIHandler) DisableWidgetHandler(c *gin.Context) {
	h.mutateWidget(c, false)
}

// mutateWidget enable/disable 共用实现：执行后返回更新后的部件信息。
func (h *APIHandler) mutateWidget(c *gin.Context, enable bool) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	info, err := h.widgetMgr.SetEnabled(c.Param("id"), enable)
	if err != nil {
		c.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	c.JSON(http.StatusOK, info)
}

// ReloadWidgetsHandler 处理 POST /api/widgets/reload — 重扫 widgets/ 目录。
func (h *APIHandler) ReloadWidgetsHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	c.JSON(http.StatusOK, map[string]any{"status": "ok", "widgets": h.widgetMgr.Reload()})
}

// HandleWidgetFile 处理 GET /api/widgets/{id}/files/*path — 部件文件服务。
//
// 防护完全照搬 workspace_http.go（HandleSessionWorkspace）：Clean/IsAbs/.. 双重校验、
// nosniff、no-referrer、http.ServeContent 流式返回——部件 iframe 以
// sandbox="allow-scripts" 加载，服务端这层必须保证只能读到部件目录内的普通文件。
func (h *APIHandler) HandleWidgetFile(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	id := c.Param("id")
	// id 先过形态校验：:id 虽是单段参数，".." 与奇异字符仍须显式拒掉。
	if widgets.ValidateID(id) != nil {
		c.String(http.StatusBadRequest, "非法部件 id")
		return
	}
	root, err := filepath.Abs(h.widgetMgr.Dir())
	if err != nil {
		c.String(http.StatusInternalServerError, "部件目录无效")
		return
	}

	// gin 的通配参数含前导 "/"；统一转成路径分隔符后清洗。
	rel := strings.TrimPrefix(c.Param("path"), "/")
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || rel == "." {
		c.String(http.StatusBadRequest, "缺少文件路径")
		return
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		c.String(http.StatusBadRequest, "非法路径")
		return
	}

	abs := filepath.Join(root, id, rel)
	// 越界二次校验：Join 后必须仍位于该部件目录内（防 "a/../../x"、"C:\x" 等写法）。
	if inside, err := filepath.Rel(filepath.Join(root, id), abs); err != nil || inside == ".." ||
		strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		c.String(http.StatusBadRequest, "非法路径")
		return
	}

	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		c.String(http.StatusNotFound, "文件不存在")
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		c.String(http.StatusNotFound, "文件不存在")
		return
	}
	defer f.Close()

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	// CSP（2026-10 安全加固）：部件卡片需要内联脚本与 data:/blob: 图像，默认源收紧到
	// self/inline/data/blob；活动文档（html/htm/svg 可被顶层导航直接打开）叠加 sandbox
	// 指令——仍允许脚本（部件需要），但禁表单提交/顶层导航/弹窗，防钓鱼跳转。
	csp := "default-src 'self' 'unsafe-inline' data: blob:"
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".html", ".htm", ".svg":
		csp += "; sandbox allow-scripts"
	}
	c.Header("Content-Security-Policy", csp)
	// ServeContent 自行按扩展名推断 Content-Type 并处理 Range；
	// 未知扩展名回落 application/octet-stream（浏览器会下载而非执行）。
	http.ServeContent(c.Writer, c.Request, filepath.Base(abs), modTimeOf(info), f)
}

// widgetPushRequest 是 POST /api/widgets/push 的请求体。
type widgetPushRequest struct {
	SessionID string `json:"session_id"`
	WidgetID  string `json:"widget_id"`
	Payload   string `json:"payload"`
}

// PushWidgetHandler 处理 POST /api/widgets/push — 向会话部件瞬时数据通道推送最新值。
// 会话不存在静默 204（bridge 轮询场景不报错刷屏）；仅请求体缺字段才 400。
func (h *APIHandler) PushWidgetHandler(c *gin.Context) {
	var req widgetPushRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "请求体非法: %v", err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.WidgetID) == "" {
		c.String(http.StatusBadRequest, "session_id 与 widget_id 必填")
		return
	}
	pusher, ok := h.widgetPusher()
	if !ok {
		c.String(http.StatusNotImplemented, "部件数据通道未接线")
		return
	}
	pusher.PushWidgetData(req.SessionID, req.WidgetID, req.Payload)
	c.Status(http.StatusNoContent)
}

// widgetReportRequest 是 POST /api/widgets/report 的请求体。
type widgetReportRequest struct {
	SessionID string `json:"session_id"`
	WidgetID  string `json:"widget_id"`
	Error     string `json:"error"`
}

// ReportWidgetHandler 处理 POST /api/widgets/report — 部件渲染错误回报
// （System 会话事件用户可见；会话运行中额外注入 MetaAgent 邮箱让 Agent 可见）。
// 会话不存在静默 204；仅请求体缺字段才 400。
func (h *APIHandler) ReportWidgetHandler(c *gin.Context) {
	var req widgetReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "请求体非法: %v", err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.WidgetID) == "" || strings.TrimSpace(req.Error) == "" {
		c.String(http.StatusBadRequest, "session_id、widget_id 与 error 必填")
		return
	}
	reporter, ok := h.widgetReporter()
	if !ok {
		c.String(http.StatusNotImplemented, "部件错误回报通道未接线")
		return
	}
	reporter.ReportWidgetError(req.SessionID, req.WidgetID, req.Error)
	c.Status(http.StatusNoContent)
}

// widgetPusher 从会话门面取部件数据推送能力（未接线/测试桩缺能力返回 false）。
func (h *APIHandler) widgetPusher() (widgetDataPusher, bool) {
	if h.sessionMgr == nil {
		return nil, false
	}
	p, ok := h.sessionMgr.agent.(widgetDataPusher)
	return p, ok
}

// widgetReporter 从会话门面取部件错误回报能力。
func (h *APIHandler) widgetReporter() (widgetErrorReporter, bool) {
	if h.sessionMgr == nil {
		return nil, false
	}
	r, ok := h.sessionMgr.agent.(widgetErrorReporter)
	return r, ok
}

// widgetActionReporter 是"部件上行操作回报"能力的鸭子类型接口
//（System 会话事件 + 运行中会话邮箱注入，同 widgetErrorReporter 模式）。
type widgetActionReporter interface {
	ReportWidgetAction(sessionID, widgetID, action, payload string)
}

// widgetActionReporter 从会话门面取部件操作上行能力。
func (h *APIHandler) widgetActionReporter() (widgetActionReporter, bool) {
	if h.sessionMgr == nil {
		return nil, false
	}
	r, ok := h.sessionMgr.agent.(widgetActionReporter)
	return r, ok
}

// DeleteWidgetHandler 处理 DELETE /api/widgets/:id — 软删部件：目录移入回收站
// widgets/.trash/<id>-del-<ts>/（不物理删除，可恢复）。不存在时幂等返回 deleted=false。
func (h *APIHandler) DeleteWidgetHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	entry, err := h.widgetMgr.Delete(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	if entry == "" {
		c.JSON(http.StatusOK, map[string]any{"deleted": false, "message": "widget not found（幂等，无变化）"})
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"deleted":    true,
		"trash_name": entry,
		"restore":    "POST /api/widgets/trash/" + entry + "/restore",
	})
}

// ListWidgetTrashHandler 处理 GET /api/widgets/trash — 回收站条目列表（del=软删 / snap=覆写前快照）。
func (h *APIHandler) ListWidgetTrashHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	c.JSON(http.StatusOK, map[string]any{"trash": h.widgetMgr.TrashList()})
}

// RestoreWidgetTrashHandler 处理 POST /api/widgets/trash/:name/restore — 恢复回收站条目
//（del 条目还原部件；snap 条目即回滚到覆写前版本；现存同名部件先入回收站不覆盖丢失）。
func (h *APIHandler) RestoreWidgetTrashHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	info, err := h.widgetMgr.Restore(c.Param("name"))
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	c.JSON(http.StatusOK, info)
}

// widgetActionRequest 是 POST /api/widgets/action 的请求体。
type widgetActionRequest struct {
	SessionID string `json:"session_id"`
	WidgetID  string `json:"widget_id"`
	Action    string `json:"action"`
	Payload   string `json:"payload"`
}

// WidgetActionHandler 处理 POST /api/widgets/action — 部件卡片内用户交互上行
//（仿 report：落 System 会话事件"【部件操作】"，运行中会话额外注入 MetaAgent 邮箱）。
// 会话不在内存静默 204；仅请求体缺字段才 400。
func (h *APIHandler) WidgetActionHandler(c *gin.Context) {
	var req widgetActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "请求体非法: %v", err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.WidgetID) == "" || strings.TrimSpace(req.Action) == "" {
		c.String(http.StatusBadRequest, "session_id、widget_id 与 action 必填")
		return
	}
	reporter, ok := h.widgetActionReporter()
	if !ok {
		c.String(http.StatusNotImplemented, "部件操作上行通道未接线")
		return
	}
	reporter.ReportWidgetAction(req.SessionID, req.WidgetID, req.Action, req.Payload)
	c.Status(http.StatusNoContent)
}
