package server

// widgets_http_test.go 钉死界面部件 HTTP 面的契约：
//   - push/report 会话不存在静默 204（bridge 轮询场景不报错刷屏）、缺字段 400；
//   - 文件服务防护与 workspace 同口径（越界 400、缺失 404、nosniff/no-referrer）；
//   - 列表/详情/启停/重载走 widgets.Manager。
// 越界防护失守等于把部件目录外的任意文件暴露给 iframe。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/widgets"
)

// widgetTestAgent 提供 PushWidgetData/ReportWidgetError 鸭子类型能力
//（其余走 mockAgentForServer）；记录调用供断言。
type widgetTestAgent struct {
	*mockAgentForServer
	pushes  []string
	reports []string
	actions []string
}

func (w *widgetTestAgent) PushWidgetData(sessionID, widgetID, payload string) {
	w.pushes = append(w.pushes, sessionID+"|"+widgetID+"|"+payload)
}

func (w *widgetTestAgent) ReportWidgetError(sessionID, widgetID, errText string) {
	w.reports = append(w.reports, sessionID+"|"+widgetID+"|"+errText)
}

func (w *widgetTestAgent) ReportWidgetAction(sessionID, widgetID, action, payload string) {
	w.actions = append(w.actions, sessionID+"|"+widgetID+"|"+action+"|"+payload)
}

// newWidgetTestRouter 构造挂部件路由的测试路由器（与生产路由形态一致）。
func newWidgetTestRouter(h *APIHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.GET("/api/widgets", h.ListWidgetsHandler)
	r.GET("/api/widgets/:id", h.GetWidgetHandler)
	r.POST("/api/widgets/:id/enable", h.EnableWidgetHandler)
	r.POST("/api/widgets/:id/disable", h.DisableWidgetHandler)
	r.POST("/api/widgets/reload", h.ReloadWidgetsHandler)
	r.GET("/api/widgets/:id/files/*path", h.HandleWidgetFile)
	r.POST("/api/widgets/push", h.PushWidgetHandler)
	r.POST("/api/widgets/report", h.ReportWidgetHandler)
	r.POST("/api/widgets/action", h.WidgetActionHandler)
	r.DELETE("/api/widgets/:id", h.DeleteWidgetHandler)
	r.GET("/api/widgets/trash", h.ListWidgetTrashHandler)
	r.POST("/api/widgets/trash/:name/restore", h.RestoreWidgetTrashHandler)
	return r
}

// newWidgetTestEnv 构造 Manager + APIHandler + 记录型 agent 门面。
func newWidgetTestEnv(t *testing.T) (*gin.Engine, *widgets.Manager, *widgetTestAgent) {
	t.Helper()
	mgr, err := widgets.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	facade := &widgetTestAgent{mockAgentForServer: newMockAgentForServer()}
	h := NewAPIHandler(nil)
	h.SetWidgetManager(mgr)
	h.SetSessionManager(NewSessionManager(facade))
	return newWidgetTestRouter(h), mgr, facade
}

func doWidgetJSON(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestWidgetAPI_ListGetEnableDisable(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("wechat-qr", widgets.Manifest{Name: "微信二维码"},
		[]widgets.FileInput{{Path: "index.html", Content: "<html></html>"}}); err != nil {
		t.Fatal(err)
	}

	// 列表。
	rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Widgets []widgets.Info `json:"widgets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Widgets) != 1 || list.Widgets[0].ID != "wechat-qr" {
		t.Fatalf("列表内容错误: %+v", list.Widgets)
	}

	// 详情：Info JSON 字段齐全（跨端契约）。
	rec = doWidgetJSON(t, r, http.MethodGet, "/api/widgets/wechat-qr", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var info widgets.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Entry != "index.html" || info.Enabled || len(info.Files) != 1 || info.UpdatedAt.IsZero() {
		t.Fatalf("详情字段不符: %+v", info)
	}
	// 不存在 → 404。
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/ghost", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec.Code)
	}

	// 启停。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/wechat-qr/enable", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := mgr.Get("wechat-qr"); !got.Enabled {
		t.Fatal("应已启用")
	}
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/wechat-qr/disable", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if got, _ := mgr.Get("wechat-qr"); got.Enabled {
		t.Fatal("应已停用")
	}

	// 重载。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/reload", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d", rec.Code)
	}
}

func TestWidgetAPI_PushReportSilent204(t *testing.T) {
	r, _, facade := newWidgetTestEnv(t)

	// push：正常请求 → 204 且透传（会话不存在与否由 ReactService 内部静默处理）。
	rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/push",
		`{"session_id":"ghost","widget_id":"wechat-qr","payload":"{\"url\":\"x\"}"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push 应 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(facade.pushes) != 1 || facade.pushes[0] != `ghost|wechat-qr|{"url":"x"}` {
		t.Fatalf("push 透传不符: %v", facade.pushes)
	}
	// 缺字段 → 400；请求体非法 → 400。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/push", `{"widget_id":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 session_id 应 400, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/push", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400, got %d", rec.Code)
	}

	// report：正常请求 → 204 且透传；缺 error → 400。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/report",
		`{"session_id":"ghost","widget_id":"wechat-qr","error":"ReferenceError: x is not defined"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("report 应 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(facade.reports) != 1 || facade.reports[0] != "ghost|wechat-qr|ReferenceError: x is not defined" {
		t.Fatalf("report 透传不符: %v", facade.reports)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/report", `{"session_id":"s","widget_id":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 error 应 400, got %d", rec.Code)
	}
}

func TestWidgetAPI_FileServing(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{},
		[]widgets.FileInput{{Path: "index.html", Content: "<h1>hi</h1>"}, {Path: "css/a.css", Content: "body{}"}}); err != nil {
		t.Fatal(err)
	}
	// 部件目录外放一个哨兵文件：任何越界写法都不得读到它。
	outside := filepath.Join(mgr.Dir(), "outside-sentinel.txt")
	if err := os.WriteFile(outside, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 正常文件：200 + Content-Type + nosniff + no-referrer。
	rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/index.html", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type=%q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("应带 nosniff")
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("应带 no-referrer")
	}
	if rec.Body.String() != "<h1>hi</h1>" {
		t.Fatalf("响应体不符: %q", rec.Body.String())
	}

	// 越界：.. 写法与跨部件目录一律拒绝（400/404），且不得读到哨兵。
	cases := []string{
		"/api/widgets/card/files/..%2Foutside-sentinel.txt",
		"/api/widgets/card/files/css/../../outside-sentinel.txt",
		"/api/widgets/card/files/../../widgets/outside-sentinel.txt",
	}
	for _, p := range cases {
		rec := doWidgetJSON(t, r, http.MethodGet, p, "")
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Fatalf("%s 应被拒（400/404），got %d body=%s", p, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "SECRET") {
			t.Fatalf("%s 读到了部件目录外的文件", p)
		}
	}
	// 非法 id / 目录 / 缺失文件。
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/Bad_ID/files/index.html", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 应 400, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/css", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("目录应 404, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/nope.js", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("缺失文件应 404, got %d", rec.Code)
	}
}

// TestWidgetAPI_DeleteTrashRestore 钉死软删/回收站端点：DELETE 移入回收站（幂等）、
// trash 列表、restore 还原可见。
func TestWidgetAPI_DeleteTrashRestore(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{Name: "c", Entry: "index.html"},
		[]widgets.FileInput{{Path: "index.html", Content: "v1"}}); err != nil {
		t.Fatal(err)
	}

	// 软删。
	rec := doWidgetJSON(t, r, http.MethodDelete, "/api/widgets/card", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d body=%s", rec.Code, rec.Body.String())
	}
	var del struct {
		Deleted bool   `json:"deleted"`
		Trash   string `json:"trash_name"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &del)
	if !del.Deleted || del.Trash == "" {
		t.Fatalf("应带 deleted 与 trash_name: %+v", del)
	}
	if _, ok := mgr.Get("card"); ok {
		t.Fatal("软删后 Get 应未命中")
	}
	// 幂等删除。
	if rec := doWidgetJSON(t, r, http.MethodDelete, "/api/widgets/card", ""); rec.Code != http.StatusOK {
		t.Fatalf("重复删除应 200, got %d", rec.Code)
	}

	// 回收站列表。
	rec = doWidgetJSON(t, r, http.MethodGet, "/api/widgets/trash", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("trash list: %d", rec.Code)
	}
	var trash struct {
		Trash []widgets.TrashInfo `json:"trash"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trash); err != nil || len(trash.Trash) != 1 {
		t.Fatalf("trash 应有 1 条: %s err=%v", rec.Body.String(), err)
	}
	// 恢复。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/trash/"+trash.Trash[0].Name+"/restore", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore: %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := mgr.Get("card"); !ok {
		t.Fatal("恢复后应可见")
	}
	// 恢复不存在的条目 → 400。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/trash/card-del-1/restore", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("恢复不存在条目应 400, got %d", rec.Code)
	}
}

// TestWidgetAPI_ActionReporter 钉死 action 上行端点：转发 ReportWidgetAction；
// 缺字段 400；未接线 501。
func TestWidgetAPI_ActionReporter(t *testing.T) {
	r, _, facade := newWidgetTestEnv(t)

	rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/action",
		`{"session_id":"s1","widget_id":"qr","action":"submit","payload":"{\"x\":1}"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("action 应 204, got %d", rec.Code)
	}
	if len(facade.actions) != 1 || facade.actions[0] != `s1|qr|submit|{"x":1}` {
		t.Fatalf("应转发 ReportWidgetAction: %v", facade.actions)
	}

	// 缺字段 400。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/action", `{"session_id":"s1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺字段应 400, got %d", rec.Code)
	}

	// 未接线（无 ReportWidgetAction 能力的门面）→ 501。
	h2 := NewAPIHandler(nil)
	h2.SetSessionManager(NewSessionManager(newMockAgentForServer()))
	r2 := newWidgetTestRouter(h2)
	if rec := doWidgetJSON(t, r2, http.MethodPost, "/api/widgets/action", `{"session_id":"s1","widget_id":"w","action":"a"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线应 501, got %d", rec.Code)
	}
}

// TestWidgetAPI_FileCSP 钉死文件服务 CSP 加固：默认 CSP 全部响应携带；html/svg
// 活动文档叠加 sandbox 指令。
func TestWidgetAPI_FileCSP(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{},
		[]widgets.FileInput{{Path: "index.html", Content: "<h1>hi</h1>"}, {Path: "pic.svg", Content: "<svg/>"}, {Path: "app.js", Content: "//x"}}); err != nil {
		t.Fatal(err)
	}
	const base = "default-src 'self' 'unsafe-inline' data: blob:"

	check := func(path, want string) {
		t.Helper()
		rec := doWidgetJSON(t, r, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != want {
			t.Fatalf("%s CSP=%q want=%q", path, got, want)
		}
	}
	check("/api/widgets/card/files/app.js", base)
	check("/api/widgets/card/files/index.html", base+"; sandbox allow-scripts")
	check("/api/widgets/card/files/pic.svg", base+"; sandbox allow-scripts")
}
