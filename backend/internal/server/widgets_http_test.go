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
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/widgets"
)

// widgetTestAgent 提供 PushWidgetData/ReportWidgetError 鸭子类型能力
//（其余走 mockAgentForServer）；记录调用供断言。
type widgetTestAgent struct {
	*mockAgentForServer
	pushes     []string
	reports    []string
	actions    []string
	broadcasts []string
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

// BroadcastWidgetData 模拟 ReactService 的部件数据全会话广播（连接器出口断言用）。
func (w *widgetTestAgent) BroadcastWidgetData(widgetID, payload string) {
	w.broadcasts = append(w.broadcasts, widgetID+"|"+payload)
}

// newWidgetTestRouter 构造挂部件路由的测试路由器（与生产路由形态一致）。
func newWidgetTestRouter(h *APIHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.GET("/api/widgets", h.ListWidgetsHandler)
	r.POST("/api/widgets", h.CreateWidgetHandler)
	r.GET("/api/widgets/:id", h.GetWidgetHandler)
	r.PUT("/api/widgets/:id/connector", h.UpdateWidgetConnectorHandler)
	r.POST("/api/widgets/:id/poll", h.PollWidgetHandler)
	r.POST("/api/widgets/:id/hook/:token", h.WidgetHookHandler)
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

// ---- 连接器（2026-10-17）----

var widgetHookTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// widgetHookResp 解析创建/再生/详情响应（裸 Info + 顶层 hook_url）。
type widgetHookResp struct {
	widgets.Info
	HookURL string `json:"hook_url"`
}

// createHookWidget 经 POST /api/widgets 建一个 webhook 裸接件，返回响应与 token。
func createHookWidget(t *testing.T, r *gin.Engine, id string) widgetHookResp {
	t.Helper()
	body := `{"name":"推送接件","connector":{"type":"webhook"}}`
	if id != "" {
		body = `{"id":"` + id + `","name":"推送接件","connector":{"type":"webhook"}}`
	}
	rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp widgetHookResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestWidgetAPI_ManifestNotServed 钉死安全硬前提：widget.json（含 webhook token）
// 与点开头文件不得经文件服务泄露——泄露 = 任何人可推数据。
func TestWidgetAPI_ManifestNotServed(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)
	if _, err := mgr.Save("card", widgets.Manifest{Name: "c"},
		[]widgets.FileInput{{Path: "index.html", Content: "<h1>hi</h1>"}}); err != nil {
		t.Fatal(err)
	}
	// 手工放一个隐藏文件（Save 本身拒绝点开头路径，模拟运维手放杂物）。
	hidden := filepath.Join(mgr.Dir(), "card", ".secret-env")
	if err := os.WriteFile(hidden, []byte("TOKEN=SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 清单：403，且不泄露内容。
	rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/widget.json", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("widget.json 应 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"entry"`) || strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("响应不得含清单内容: %s", rec.Body.String())
	}
	// 隐藏文件：403；隐藏目录内的文件同样 403。
	rec = doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/.secret-env", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("隐藏文件应 403, got %d", rec.Code)
	}
	if err := os.MkdirAll(filepath.Join(mgr.Dir(), "card", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mgr.Dir(), "card", ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/.git/config", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("隐藏目录内文件应 403, got %d", rec.Code)
	}
	// 普通文件不受影响。
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/card/files/index.html", ""); rec.Code != http.StatusOK {
		t.Fatalf("普通文件应 200, got %d", rec.Code)
	}
}

// TestWidgetAPI_CreateConnector 钉死 POST /api/widgets：webhook 裸接件返回 token 明文 +
// 顶层 hook_url 且不生成 index.html；id 缺省生成 conn- 前缀；校验失败 400。
func TestWidgetAPI_CreateConnector(t *testing.T) {
	r, mgr, _ := newWidgetTestEnv(t)

	// webhook 裸接件。
	resp := createHookWidget(t, r, "conn-push")
	if resp.ID != "conn-push" || resp.Connector == nil || resp.Connector.Type != "webhook" {
		t.Fatalf("响应 Info 不符: %+v", resp)
	}
	if !widgetHookTokenPattern.MatchString(resp.Connector.Token) {
		t.Fatalf("token 应为 32 位 hex: %q", resp.Connector.Token)
	}
	wantHook := "/api/widgets/conn-push/hook/" + resp.Connector.Token
	if resp.HookURL != wantHook {
		t.Fatalf("hook_url 不符: %q want %q", resp.HookURL, wantHook)
	}
	// 裸接件：无 index.html（文件服务 404），清单在册。
	info, ok := mgr.Get("conn-push")
	if !ok || len(info.Files) != 0 {
		t.Fatalf("裸接件不应有文件: %+v ok=%v", info, ok)
	}
	if rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/conn-push/files/index.html", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("裸接件 index.html 应 404, got %d", rec.Code)
	}
	// 详情 GET 也带 hook_url（顶层）。
	rec := doWidgetJSON(t, r, http.MethodGet, "/api/widgets/conn-push", "")
	var detail widgetHookResp
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || detail.HookURL != wantHook {
		t.Fatalf("详情应带 hook_url: %s err=%v", rec.Body.String(), err)
	}

	// id 缺省生成 conn-<8位小写字母数字>。
	resp2 := createHookWidget(t, r, "")
	if !regexp.MustCompile(`^conn-[a-z0-9]{8}$`).MatchString(resp2.ID) {
		t.Fatalf("缺省 id 形态不符: %q", resp2.ID)
	}

	// 400 族：坏 id / 缺 name / 缺 connector / 坏 connector / 外部 token。
	cases := []string{
		`{"id":"Bad_ID","name":"x","connector":{"type":"webhook"}}`,
		`{"connector":{"type":"webhook"}}`,                               // 缺 name
		`{"name":"x"}`,                                                   // 缺 connector
		`{"name":"x","connector":{"type":"sms"}}`,                        // 坏 type
		`{"name":"x","connector":{"type":"api"}}`,                        // api 缺 target
		`{"name":"x","connector":{"type":"webhook","token":"deadbeef"}}`, // 外部 token
	}
	for i, body := range cases {
		if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("case %d 应 400, got %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets", `{bad`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400, got %d", rec.Code)
	}

	// 带 files 创建（api 型）：文件落盘继承 Manager.Save 校验与上限。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets", `{
		"id":"conn-quotes","name":"行情","connector":{"type":"api","target":"https://example.com/q","interval_sec":3},
		"files":[{"path":"index.html","content":"<html/>"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("带 files 创建: %d body=%s", rec.Code, rec.Body.String())
	}
	info, ok = mgr.Get("conn-quotes")
	if !ok || len(info.Files) != 1 || info.Connector.IntervalSec != 10 {
		t.Fatalf("files/钳制不符: %+v ok=%v", info, ok)
	}
}

// TestWidgetAPI_UpdateConnector 钉死 PUT connector：regen_token 换 token（旧地址 401、
// 新地址 200），非 regen 续用；404/400 语义。
func TestWidgetAPI_UpdateConnector(t *testing.T) {
	r, mgr, facade := newWidgetTestEnv(t)
	created := createHookWidget(t, r, "conn-upd")
	oldToken := created.Connector.Token

	// 非 regen：token 续用。
	rec := doWidgetJSON(t, r, http.MethodPut, "/api/widgets/conn-upd/connector",
		`{"type":"webhook"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp widgetHookResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connector.Token != oldToken {
		t.Fatal("非 regen 应续用 token")
	}

	// regen：新 token + 新 hook_url。
	rec = doWidgetJSON(t, r, http.MethodPut, "/api/widgets/conn-upd/connector",
		`{"type":"webhook","regen_token":true}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Connector.Token == oldToken || resp.HookURL == "" {
		t.Fatalf("regen 应换 token: %+v", resp)
	}
	// 旧地址 401，新地址 200 且广播。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-upd/hook/"+oldToken, `{"a":1}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧 token 应 401, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-upd/hook/"+resp.Connector.Token, `{"a":1}`); rec.Code != http.StatusOK {
		t.Fatalf("新 token 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(facade.broadcasts) != 1 || facade.broadcasts[0] != `conn-upd|{"a":1}` {
		t.Fatalf("应广播 webhook payload: %v", facade.broadcasts)
	}

	// 换 api 型：token 清空、target 写入、其余清单字段保留。
	// 注意：每次都用新变量解析——json.Unmarshal 合并且不清零已存在的指针字段。
	rec = doWidgetJSON(t, r, http.MethodPut, "/api/widgets/conn-upd/connector",
		`{"type":"api","target":"https://example.com/feed","interval_sec":15}`)
	var apiResp widgetHookResp
	if err := json.Unmarshal(rec.Body.Bytes(), &apiResp); err != nil {
		t.Fatal(err)
	}
	if apiResp.Connector.Type != widgets.ConnectorTypeAPI || apiResp.Connector.Token != "" ||
		apiResp.Connector.Target != "https://example.com/feed" || apiResp.Connector.IntervalSec != 15 {
		t.Fatalf("api 化不符: %+v", apiResp.Connector)
	}
	info, _ := mgr.Get("conn-upd")
	if info.Name != "推送接件" {
		t.Fatalf("name 应保留: %+v", info)
	}

	// 404/400。
	if rec := doWidgetJSON(t, r, http.MethodPut, "/api/widgets/ghost/connector", `{"type":"webhook"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPut, "/api/widgets/conn-upd/connector", `{"type":"carrier"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 connector 应 400, got %d", rec.Code)
	}
}

// TestWidgetAPI_Hook 钉死 webhook 回调：对 token 推数据 → 200 {"ok":true} 且广播；
// 错 token → 401；非 webhook / 不存在 → 404；超限 → 429。
func TestWidgetAPI_Hook(t *testing.T) {
	r, mgr, facade := newWidgetTestEnv(t)
	created := createHookWidget(t, r, "conn-hook")

	// 正常推送：200 + 广播原文。
	rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-hook/hook/"+created.Connector.Token,
		`{"event":"order.created","id":42}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Fatalf("hook 应 200 {\"ok\":true}: %d %s", rec.Code, rec.Body.String())
	}
	if len(facade.broadcasts) != 1 || facade.broadcasts[0] != `conn-hook|{"event":"order.created","id":42}` {
		t.Fatalf("应广播原文 payload: %v", facade.broadcasts)
	}

	// 错 token → 401（与不存在部件同一文案，不区分存在性）。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-hook/hook/00000000000000000000000000000000", `x`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错 token 应 401, got %d", rec.Code)
	}
	// 不存在部件 → 404。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/ghost/hook/"+created.Connector.Token, `x`); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在部件应 404, got %d", rec.Code)
	}
	// 非 webhook 部件 → 404。
	if _, err := mgr.Save("plain", widgets.Manifest{Name: "p"},
		[]widgets.FileInput{{Path: "index.html", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/plain/hook/"+created.Connector.Token, `x`); rec.Code != http.StatusNotFound {
		t.Fatalf("非 webhook 应 404, got %d", rec.Code)
	}

	// 限流：新建接件连续推 61 次 → 第 61 次 429（前 60 次 200）。
	burst := createHookWidget(t, r, "conn-burst")
	for i := 0; i < 60; i++ {
		if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-burst/hook/"+burst.Connector.Token, `x`); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次应 200, got %d", i+1, rec.Code)
		}
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-burst/hook/"+burst.Connector.Token, `x`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 61 次应 429, got %d", rec.Code)
	}
}

// TestWidgetAPI_Poll 钉死立即执行：api 用 httptest 真 HTTP 源断言 data 与广播；
// webhook/url → 400；不存在 → 404。
func TestWidgetAPI_Poll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"price":1024}`))
	}))
	defer srv.Close()

	r, _, facade := newWidgetTestEnv(t)
	rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets", `{
		"id":"conn-poll","name":"行情","connector":{"type":"api","target":"`+srv.URL+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", rec.Code, rec.Body.String())
	}

	// poll：{"ok":true,"data":"..."} 且照常广播。
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-poll/poll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll: %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK   bool   `json:"ok"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.Data != `{"price":1024}` {
		t.Fatalf("poll 响应不符: %s err=%v", rec.Body.String(), err)
	}
	if len(facade.broadcasts) != 1 || facade.broadcasts[0] != `conn-poll|{"price":1024}` {
		t.Fatalf("poll 应广播: %v", facade.broadcasts)
	}

	// webhook/url 型 → 400。
	hook := createHookWidget(t, r, "conn-wh")
	_ = hook
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-wh/poll", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("webhook poll 应 400, got %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets", `{
		"id":"conn-link","name":"外链","connector":{"type":"url","target":"https://example.com"}}`); rec.Code != http.StatusOK {
		t.Fatalf("create url: %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-link/poll", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("url poll 应 400, got %d", rec.Code)
	}
	// 不存在 → 404。
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/ghost/poll", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rec.Code)
	}
	// api 源失败 → 500。
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer bad.Close()
	rec = doWidgetJSON(t, r, http.MethodPost, "/api/widgets", `{
		"id":"conn-bad","name":"坏源","connector":{"type":"api","target":"`+bad.URL+`"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create bad: %d", rec.Code)
	}
	if rec := doWidgetJSON(t, r, http.MethodPost, "/api/widgets/conn-bad/poll", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("源失败应 500, got %d", rec.Code)
	}
}
