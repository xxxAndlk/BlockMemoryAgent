package server

// widgets_http.go 界面部件（Widget）管理 API（Gin 版）：
//   - GET  /api/widgets                 — 列表（Info JSON：id/name/version/description/entry/enabled/updated_at/files[/connector]）
//   - POST /api/widgets                 — 新建部件（含连接器；webhook 响应带 token 明文 + 顶层 hook_url）
//   - GET  /api/widgets/{id}           — 单个部件详情（webhook 附顶层 hook_url）
//   - PUT  /api/widgets/{id}/connector — 更新连接器段（可 regen_token 再生 webhook token）
//   - POST /api/widgets/{id}/poll      — 连接器立即执行一次（api/mcp），同步返回 {"ok":true,"data":...}
//   - POST /api/widgets/{id}/hook/{token} — webhook 回调（无会话鉴权；限 256KB + 单部件 60s/60 次限流）
//   - POST /api/widgets/{id}/enable    — 热启用
//   - POST /api/widgets/{id}/disable   — 热停用
//   - POST /api/widgets/reload         — 重扫 widgets/ 目录
//   - GET  /api/widgets/{id}/files/*   — 部件文件服务（卡片 iframe 的渲染入口与附属资源；
//                                         widget.json 清单与点开头文件拒绝服务——token 不能泄露）
//   - POST /api/widgets/push           — 推送瞬时数据 {session_id, widget_id, payload}
//   - POST /api/widgets/report         — 部件渲染错误回报 {session_id, widget_id, error}
//
// 除 hook 外全部端点经 GinAuthMiddleware 保护（bootstrap/router.go 统一挂组中间件）；
// hook 端点注册在无鉴权组：外部系统凭 id+token 直推（部件 id + token 即凭据）。
// push/report 面向 bridge 等进程内插件轮询调用：会话不存在一律静默 204，
// 推送方无需感知会话生命周期，也不会因会话结束而报错刷屏。

import (
	"context"       // mcp 连接器工具调用的 ctx
	"crypto/rand"   // 接件缺省 id 随机化
	"fmt"           // 错误文案
	"io"            // webhook 请求体限长读取
	"log"           // webhook 截断日志（APIHandler 无注入 logger，回退标准库）
	"net/http"      // HTTP 状态码
	"os"            // 文件 Stat/Open
	"path/filepath" // 路径清洗与越界校验
	"strings"       // 路径前缀判定
	"time"          // 缺省 id 回退时间戳

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

// widgetBroadcaster 是"部件数据全会话广播"能力的鸭子类型接口：连接器
// （webhook 回调/调度器/poll）没有单一目标会话，广播保证打开该部件的所有
// 会话都收到 widget_data 帧。由 *agent.ReactService 实现（BroadcastWidgetData）。
type widgetBroadcaster interface {
	BroadcastWidgetData(widgetID, payload string)
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

// GetWidgetHandler 处理 GET /api/widgets/{id} — 返回单个部件详情（Info JSON；
// webhook 连接器附顶层 hook_url 回调地址，前端拼 origin 展示）。
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
	h.respondWidgetInfo(c, info)
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
	// 清单与点开头文件拒绝服务（安全钉死）：widget.json 含 webhook token 等元数据，
	// 经文件服务泄露 = 任何人可推数据（connector 明文 token 在清单里）；点开头文件
	// 是原子写残留/手工杂物，无对外服务意义。
	if rel == widgetManifestFile || hasHiddenPathSegment(rel) {
		c.String(http.StatusForbidden, "禁止访问清单与隐藏文件")
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

// ---- 连接器（2026-10-17 Widget「连接器」：万物皆可外接，见 widgets/connector.go）----

// widgetManifestFile 是部件清单文件名（与 widgets 包 manifestFile 同字面量）：
// 清单含 webhook token 等元数据，文件服务必须拒发（widgets.Save 也拒绝同名文件写入）。
const widgetManifestFile = "widget.json"

// hasHiddenPathSegment 判断清洗后的相对路径是否含点开头段（隐藏文件/目录）。
func hasHiddenPathSegment(rel string) bool {
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// widgetInfoWithHook 是部件响应线型：裸 Info JSON + webhook 的顶层 hook_url
//（回调相对地址 "/api/widgets/<id>/hook/<token>"，前端拼 origin 展示；与前端
// WidgetInfo 契约对齐，非 webhook 不输出该字段）。
type widgetInfoWithHook struct {
	*widgets.Info
	HookURL string `json:"hook_url,omitempty"`
}

// respondWidgetInfo 回部件 Info JSON；webhook 连接器附顶层 hook_url。
func (h *APIHandler) respondWidgetInfo(c *gin.Context, info *widgets.Info) {
	if info.Connector != nil && info.Connector.Type == widgets.ConnectorTypeWebhook && info.Connector.Token != "" {
		c.JSON(http.StatusOK, widgetInfoWithHook{
			Info:    info,
			HookURL: "/api/widgets/" + info.ID + "/hook/" + info.Connector.Token,
		})
		return
	}
	c.JSON(http.StatusOK, info)
}

// widgetCreateRequest 是 POST /api/widgets（新建含连接器的部件）的请求体。
type widgetCreateRequest struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Connector   *widgets.ConnectorSpec `json:"connector"`
	Files       []widgetFileInput      `json:"files"`
}

// widgetFileInput 是创建请求体的单个待写文件。
type widgetFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// CreateWidgetHandler 处理 POST /api/widgets — 新建含连接器的部件（「接件」）。
// id 缺省生成 conn-<8位随机小写字母数字>；name 必填；connector 走保存期同款校验
//（webhook 的 token 服务端生成）；files 非空走 Manager.Save 同款落盘（单部件 32 文件
///1MB 上限天然继承），files 为空不生成 index.html——裸接件由前端通用数据卡/外链渲染。
// 响应为裸 Info JSON（webhook 时 connector.token 明文）+ 顶层 hook_url。
func (h *APIHandler) CreateWidgetHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	var req widgetCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "请求体非法: %v", err)
		return
	}
	if req.Connector == nil {
		c.String(http.StatusBadRequest, "connector 必填（url/api/mcp/webhook 四选一）")
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newConnectorWidgetID()
	}
	if err := widgets.ValidateID(id); err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.String(http.StatusBadRequest, "name 必填")
		return
	}
	files := make([]widgets.FileInput, 0, len(req.Files))
	for _, f := range req.Files {
		files = append(files, widgets.FileInput{Path: f.Path, Content: f.Content})
	}
	mf := widgets.Manifest{
		Name:        name,
		Description: strings.TrimSpace(req.Description),
		Connector:   req.Connector,
	}
	info, err := h.widgetMgr.Save(id, mf, files)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	h.respondWidgetInfo(c, info)
}

// newConnectorWidgetID 生成 conn-<8位随机小写字母数字> 的接件缺省 id
//（crypto/rand；不可用时退回时间戳，仍满足 id 形态校验）。
func newConnectorWidgetID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("conn-%08x", time.Now().UnixNano())
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return "conn-" + string(buf)
}

// widgetConnectorUpdateRequest 是 PUT /api/widgets/:id/connector 的请求体：
// connector 各字段 + regen_token（true 且 webhook 时重新生成 token，旧回调地址即刻失效）。
type widgetConnectorUpdateRequest struct {
	widgets.ConnectorSpec
	RegenToken bool `json:"regen_token"`
}

// UpdateWidgetConnectorHandler 处理 PUT /api/widgets/:id/connector — 更新部件连接器段
//（Manager.SaveConnector 原子改写清单，热载缓存一致；其余清单字段原样保留）。
// 响应同 POST /api/widgets（裸 Info JSON；webhook 带 token 明文 + 顶层 hook_url）。
func (h *APIHandler) UpdateWidgetConnectorHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	id := c.Param("id")
	if _, ok := h.widgetMgr.Get(id); !ok {
		c.String(http.StatusNotFound, "widget not found")
		return
	}
	var req widgetConnectorUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.String(http.StatusBadRequest, "请求体非法: %v", err)
		return
	}
	info, err := h.widgetMgr.SaveConnector(id, &req.ConnectorSpec, req.RegenToken)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)
		return
	}
	h.respondWidgetInfo(c, info)
}

// WidgetHookHandler 处理 POST /api/widgets/:id/hook/:token — webhook 连接器回调
//（外部系统直推，注册在无鉴权组：部件 id + token 即凭据）。无 connector 或非
// webhook 类型一律 404；token 不匹配 401（统一文案不区分存在性）；单部件 60s/60 次
// 限流超限 429。通过则请求体（限 256KB，超出截断记日志）经部件数据总线广播到
// 打开该部件的所有会话（widget_data 帧，与 POST /widgets/push 同款出口）。
func (h *APIHandler) WidgetHookHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	info, ok := h.widgetMgr.Get(c.Param("id"))
	if !ok {
		c.String(http.StatusNotFound, "widget not found")
		return
	}
	conn := info.Connector
	if conn == nil || conn.Type != widgets.ConnectorTypeWebhook {
		c.String(http.StatusNotFound, "widget not found") // 统一文案：不对外区分"无连接器"
		return
	}
	if conn.Token == "" || c.Param("token") != conn.Token {
		c.String(http.StatusUnauthorized, "token 无效")
		return
	}
	if h.hookLimiter == nil {
		h.hookLimiter = widgets.NewHookRateLimiter()
	}
	if !h.hookLimiter.Allow(info.ID) {
		c.String(http.StatusTooManyRequests, "回调过于频繁（单部件限 60 秒 60 次）")
		return
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, widgets.ConnectorPayloadMaxBytes+1))
	if err != nil {
		c.String(http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	if len(data) > widgets.ConnectorPayloadMaxBytes {
		data = data[:widgets.ConnectorPayloadMaxBytes]
		log.Printf("[widgets] webhook payload 超 %d 字节已截断: widget=%s",
			widgets.ConnectorPayloadMaxBytes, info.ID)
	}
	b, ok := h.widgetBroadcaster()
	if !ok {
		c.String(http.StatusNotImplemented, "部件数据通道未接线")
		return
	}
	b.BroadcastWidgetData(info.ID, string(data))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// PollWidgetHandler 处理 POST /api/widgets/:id/poll — 立即执行一次连接器（api/mcp），
// 同步返回 {"ok":true,"data":"..."}（data 为最新拉取结果文本）并照常经数据总线分发；
// webhook/url 类型（或无连接器）→ 400；执行失败 → 500。
func (h *APIHandler) PollWidgetHandler(c *gin.Context) {
	if h.widgetMgr == nil {
		c.String(http.StatusServiceUnavailable, "widget manager not initialized")
		return
	}
	id := c.Param("id")
	info, ok := h.widgetMgr.Get(id)
	if !ok {
		c.String(http.StatusNotFound, "widget not found")
		return
	}
	conn := info.Connector
	if conn == nil || (conn.Type != widgets.ConnectorTypeAPI && conn.Type != widgets.ConnectorTypeMCP) {
		c.String(http.StatusBadRequest, "该部件无 api/mcp 连接器，不支持立即执行")
		return
	}
	payload, err := h.widgetMgr.ExecuteConnector(c.Request.Context(), id, h.connectorToolCaller())
	if err != nil {
		c.String(http.StatusInternalServerError, "连接器执行失败: %v", err)
		return
	}
	if b, ok := h.widgetBroadcaster(); ok {
		b.BroadcastWidgetData(id, payload)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "data": payload})
}

// widgetBroadcaster 从会话门面取部件数据广播能力（未接线/测试桩缺能力返回 false）。
func (h *APIHandler) widgetBroadcaster() (widgetBroadcaster, bool) {
	if h.sessionMgr == nil {
		return nil, false
	}
	b, ok := h.sessionMgr.agent.(widgetBroadcaster)
	return b, ok
}

// widgetConnectorToolCaller 是 mcp 连接器工具调用能力的鸭子类型：poll 端点与
// 调度器执行 mcp 连接器时经它调本进程工具（由 *agent.ReactService 实现，
// 内部走 tool.Registry.Dispatch 并注入主工作目录）。
type widgetConnectorToolCaller interface {
	CallConnectorTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// connectorToolCaller 从会话门面取 mcp 连接器工具调用能力（未接线返回 nil）。
func (h *APIHandler) connectorToolCaller() widgets.MCPToolCaller {
	if h.sessionMgr == nil {
		return nil
	}
	if c, ok := h.sessionMgr.agent.(widgetConnectorToolCaller); ok {
		return connectorToolCallerFunc(c.CallConnectorTool)
	}
	return nil
}

// connectorToolCallerFunc 把方法签名适配成 widgets.MCPToolCaller 接口。
type connectorToolCallerFunc func(ctx context.Context, name string, args map[string]any) (string, error)

// CallTool 实现 widgets.MCPToolCaller。
func (f connectorToolCallerFunc) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	return f(ctx, name, args)
}
