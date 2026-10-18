package widgets

// connector_test.go 钉死 Widget「连接器」契约：connector 段校验表（各 type 缺字段/
// 钳制/限长/拒外部 token）、Save/SaveConnector 透传与 webhook token 生成/再生/继承、
// 调度器（到点执行/未到点跳过/连续失败退避/类型过滤）、hook 限流滑窗、api 执行
// （方法/头插值/非 2xx 失败/截断）。这条链路是"外部数据进部件卡片"的源头。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestValidateConnector 校验表：覆盖各 type 缺字段/非法值/钳制/限长/拒外部 token。
func TestValidateConnector(t *testing.T) {
	bad := []*ConnectorSpec{
		nil,
		{Type: ""},    // type 必填
		{Type: "ftp"}, // 非法 type
		{Type: "url"}, // url 缺 target
		{Type: "url", Target: "ftp://example.com/x"},     // url 非 http(s)
		{Type: "api", Target: "not-a-url"},               // api 非 URL
		{Type: "api", Target: "http://x", Method: "PUT"}, // method 仅 GET/POST
		{Type: "mcp"},                   // mcp 缺工具名
		{Type: "mcp", Target: "  "},     // mcp 空白工具名
		{Type: "webhook", Token: "abc"}, // token 拒外部传入
		{Type: "api", Target: "http://x",
			Headers: map[string]string{strings.Repeat("k", 65): "v"}}, // 键超长
		{Type: "api", Target: "http://x",
			Headers: map[string]string{"k": strings.Repeat("v", 257)}}, // 值超长
	}
	for i, c := range bad {
		if err := ValidateConnector(c); err == nil {
			t.Fatalf("case %d 应拒绝: %+v", i, c)
		}
	}

	// webhook 忽略 target；method 大小写规整。
	w := &ConnectorSpec{Type: "Webhook", Target: "whatever", Method: "post"}
	if err := ValidateConnector(w); err != nil {
		t.Fatalf("webhook 应忽略 target: %v", err)
	}
	if w.Type != "webhook" || w.Method != "POST" {
		t.Fatalf("应规整大小写: %+v", w)
	}

	// 钳制与缺省：interval [10,3600] 缺省 30；timeout [1,60] 缺省 10。
	c := &ConnectorSpec{Type: "api", Target: "https://example.com/data"}
	if err := ValidateConnector(c); err != nil {
		t.Fatal(err)
	}
	if c.IntervalSec != 30 || c.TimeoutSec != 10 {
		t.Fatalf("缺省值不符: %+v", c)
	}
	c = &ConnectorSpec{Type: "api", Target: "https://example.com", IntervalSec: 5, TimeoutSec: 999}
	if err := ValidateConnector(c); err != nil {
		t.Fatal(err)
	}
	if c.IntervalSec != 10 || c.TimeoutSec != 60 {
		t.Fatalf("钳制不符: %+v", c)
	}
	c = &ConnectorSpec{Type: "api", Target: "https://example.com", IntervalSec: 99999, TimeoutSec: -3}
	if err := ValidateConnector(c); err != nil {
		t.Fatal(err)
	}
	if c.IntervalSec != 3600 || c.TimeoutSec != 1 {
		t.Fatalf("边界钳制不符: %+v", c)
	}
}

var connectorTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestManager_SaveConnectorPassthrough 钉死：Save 带 connector 后 List/Get 原样透传；
// 裸接件（零文件）合法且不生成 index.html；webhook token 服务端生成（32 位 hex）；
// 再次保存（未给 connector）整段保留；SetEnabled 不丢 connector/token。
func TestManager_SaveConnectorPassthrough(t *testing.T) {
	m := newTestManager(t)

	// 裸接件：零文件 + webhook connector，不生成 index.html。
	info, err := m.Save("conn-push", Manifest{Name: "推送接件", Connector: &ConnectorSpec{
		Type: ConnectorTypeWebhook,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 0 || info.Entry != defaultEntry {
		t.Fatalf("裸接件不应有文件: %+v", info)
	}
	if info.Connector == nil || info.Connector.Type != ConnectorTypeWebhook ||
		!connectorTokenPattern.MatchString(info.Connector.Token) {
		t.Fatalf("webhook token 应由服务端生成(32 hex): %+v", info.Connector)
	}
	// 清单落盘含 connector 段（token 在盘上，所以文件服务必须拒发 widget.json）。
	data, err := os.ReadFile(filepath.Join(m.Dir(), "conn-push", manifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var mf Manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.Connector == nil || mf.Connector.Token != info.Connector.Token {
		t.Fatalf("清单 connector 段不符: %+v", mf.Connector)
	}

	// api 裸接件 + 钳制后的数值透传。
	info2, err := m.Save("conn-api", Manifest{Name: "行情", Connector: &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: "https://example.com/quotes",
		IntervalSec: 3, TimeoutSec: 99,
		Headers: map[string]string{"Authorization": "${QUOTES_KEY}"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Connector.IntervalSec != 10 || info2.Connector.TimeoutSec != 60 ||
		info2.Connector.Headers["Authorization"] != "${QUOTES_KEY}" {
		t.Fatalf("api connector 透传/钳制不符: %+v", info2.Connector)
	}

	// List/Get 透传。
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("应列出 2 个部件: %d", len(list))
	}
	got, ok := m.Get("conn-push")
	if !ok || got.Connector == nil || got.Connector.Token != info.Connector.Token {
		t.Fatalf("Get 应透传 connector: %+v ok=%v", got, ok)
	}

	// 再次全量保存（未给 connector）：整段保留，token 不变。
	if _, err := m.Save("conn-push", Manifest{Name: "推送接件 v2"},
		[]FileInput{{Path: "index.html", Content: "<html/>"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Get("conn-push")
	if got.Connector == nil || got.Connector.Token != info.Connector.Token {
		t.Fatalf("再保存应保留 connector/token: %+v", got.Connector)
	}

	// SetEnabled 不丢 connector/token（热启停安全）。
	got, err = m.SetEnabled("conn-push", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Connector == nil || got.Connector.Token != info.Connector.Token || got.Enabled {
		t.Fatalf("SetEnabled 应保留 connector/token: %+v", got)
	}

	// 增量保存未给 connector 同样保留。
	if _, err := m.SaveIncremental("conn-push", Manifest{},
		[]FileInput{{Path: "app.js", Content: "// x"}}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Get("conn-push")
	if got.Connector == nil || got.Connector.Token != info.Connector.Token {
		t.Fatalf("增量保存应保留 connector/token: %+v", got.Connector)
	}

	// Save 显式传 token 一律拒绝。
	if _, err := m.Save("conn-evil", Manifest{Name: "x", Connector: &ConnectorSpec{
		Type: ConnectorTypeWebhook, Token: "deadbeef",
	}}, nil); err == nil {
		t.Fatal("外部传入 token 应拒绝")
	}
}

// TestManager_SaveConnectorUpdate 钉死 SaveConnector：字段原子改写、保留其余清单字段、
// token 再生/续用/类型切换语义。
func TestManager_SaveConnectorUpdate(t *testing.T) {
	m := newTestManager(t)
	info, err := m.Save("w1", Manifest{Name: "接件", Description: "d", Connector: &ConnectorSpec{
		Type: ConnectorTypeWebhook,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldToken := info.Connector.Token

	// 更新 interval（webhook 不写 interval 也行，这里换 api 型更实际）：
	// 换 api 型 → token 清空、其余清单字段保留。
	upd, err := m.SaveConnector("w1", &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: "https://example.com/feed", IntervalSec: 15,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Connector.Type != ConnectorTypeAPI || upd.Connector.Token != "" || upd.Connector.IntervalSec != 15 {
		t.Fatalf("api 化不符: %+v", upd.Connector)
	}
	if upd.Name != "接件" || upd.Description != "d" || upd.Enabled {
		t.Fatalf("其余清单字段应保留: %+v", upd)
	}

	// 换回 webhook → 生成新 token（旧 token 不复活）。
	upd2, err := m.SaveConnector("w1", &ConnectorSpec{Type: ConnectorTypeWebhook}, false)
	if err != nil {
		t.Fatal(err)
	}
	if upd2.Connector.Type != ConnectorTypeWebhook || !connectorTokenPattern.MatchString(upd2.Connector.Token) {
		t.Fatalf("webhook 化应生成新 token: %+v", upd2.Connector)
	}

	// 非 regen 再保存 webhook → token 续用。
	upd3, err := m.SaveConnector("w1", &ConnectorSpec{Type: ConnectorTypeWebhook}, false)
	if err != nil {
		t.Fatal(err)
	}
	if upd3.Connector.Token != upd2.Connector.Token {
		t.Fatal("非 regen 应续用 token")
	}

	// regen → token 更换。
	upd4, err := m.SaveConnector("w1", &ConnectorSpec{Type: ConnectorTypeWebhook}, true)
	if err != nil {
		t.Fatal(err)
	}
	if upd4.Connector.Token == upd2.Connector.Token {
		t.Fatal("regen 应更换 token")
	}
	_ = oldToken

	// 不存在部件 → 报错。
	if _, err := m.SaveConnector("ghost", &ConnectorSpec{Type: ConnectorTypeWebhook}, false); err == nil {
		t.Fatal("不存在部件应报错")
	}
	// 外部传入 token → 拒绝。
	if _, err := m.SaveConnector("w1", &ConnectorSpec{Type: ConnectorTypeWebhook, Token: "x"}, false); err == nil {
		t.Fatal("外部 token 应拒绝")
	}
}

// TestConnectorHookRateLimiter 钉死滑窗：60 秒内 60 次放行、第 61 次拒绝；窗口滑动后放行。
func TestConnectorHookRateLimiter(t *testing.T) {
	l := NewHookRateLimiter()
	start := time.Now()
	for i := 0; i < hookRateMax; i++ {
		if !l.AllowAt("w", start.Add(time.Duration(i)*time.Millisecond)) {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	if l.AllowAt("w", start.Add(60*time.Millisecond)) {
		t.Fatal("第 61 次应拒绝")
	}
	// 窗口滑动：首命中滑出 60s 后放行。
	if !l.AllowAt("w", start.Add(hookRateWindow+time.Second)) {
		t.Fatal("窗口滑动后应放行")
	}
	// 不同部件互不影响。
	if !l.AllowAt("other", start) {
		t.Fatal("他部件不应被限")
	}
}

// fakeMCPCaller 记录调用的 MCPToolCaller 桩。
type fakeMCPCaller struct {
	mu    sync.Mutex
	calls []string
	out   string
	err   error
}

func (f *fakeMCPCaller) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.out, f.err
}

// TestConnectorScheduler 钉死调度器：api 到点执行/未到点跳过、mcp 到点执行、
// url/webhook/停用部件跳过、连续失败 5 次退避 10 tick。
func TestConnectorScheduler(t *testing.T) {
	m := newTestManager(t)
	var dispatched []string
	var dMu sync.Mutex
	dispatch := func(widgetID, payload string) {
		dMu.Lock()
		defer dMu.Unlock()
		dispatched = append(dispatched, widgetID+"|"+payload)
	}
	s := NewConnectorScheduler(m, dispatch)
	s.tick = time.Second // 退避时长 = 10s，测试可控

	// api 源：两次调用返回不同值，验证"到点才执行"。
	var apiCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		fmt.Fprintf(w, "data-%d", apiCalls)
	}))
	defer api.Close()

	mustSave := func(id string, conn *ConnectorSpec) {
		t.Helper()
		if _, err := m.Save(id, Manifest{Name: id, Enabled: true, Connector: conn}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mustSave("api-w", &ConnectorSpec{Type: ConnectorTypeAPI, Target: api.URL, IntervalSec: 10})
	caller := &fakeMCPCaller{out: "mcp-out"}
	s.SetMCPToolCaller(caller)
	mustSave("mcp-w", &ConnectorSpec{Type: ConnectorTypeMCP, Target: "my_tool", IntervalSec: 10})
	mustSave("hook-w", &ConnectorSpec{Type: ConnectorTypeWebhook})
	mustSave("url-w", &ConnectorSpec{Type: ConnectorTypeURL, Target: "https://example.com"})
	if _, err := m.SetEnabled("url-w", false); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	s.runOnce(context.Background(), t0)
	dMu.Lock()
	if len(dispatched) != 2 || dispatched[0] != "api-w|data-1" || dispatched[1] != "mcp-w|mcp-out" {
		t.Fatalf("首轮应执行 api+mcp: %v", dispatched)
	}
	dMu.Unlock()
	if len(caller.calls) != 1 || caller.calls[0] != "my_tool" {
		t.Fatalf("mcp 工具调用不符: %v", caller.calls)
	}

	// 未到 interval（10s）：全部跳过。
	s.runOnce(context.Background(), t0.Add(5*time.Second))
	dMu.Lock()
	if len(dispatched) != 2 {
		t.Fatalf("未到点不应执行: %v", dispatched)
	}
	dMu.Unlock()

	// 到 interval（10s）：api 与 mcp 都再次执行；webhook/url 仍跳过。
	s.runOnce(context.Background(), t0.Add(10*time.Second))
	dMu.Lock()
	if len(dispatched) != 4 || dispatched[2] != "api-w|data-2" || dispatched[3] != "mcp-w|mcp-out" {
		t.Fatalf("到点应再执行 api+mcp: %v", dispatched)
	}
	dMu.Unlock()
	if len(caller.calls) != 2 {
		t.Fatalf("mcp 工具应被调 2 次: %v", caller.calls)
	}
}

// TestConnectorSchedulerFailureBackoff 用请求计数精确钉死退避：连失 5 次 → 10 tick
// 内零请求 → 退避结束后恢复请求。
func TestConnectorSchedulerFailureBackoff(t *testing.T) {
	m := newTestManager(t)
	s := NewConnectorScheduler(m, func(string, string) {})
	s.tick = time.Second

	var reqCount int
	var mu sync.Mutex
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqCount++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failSrv.Close()

	if _, err := m.Save("flaky", Manifest{Name: "f", Enabled: true, Connector: &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: failSrv.URL, IntervalSec: 10,
	}}, nil); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	for i := 0; i < connectorMaxFailures; i++ {
		s.runOnce(context.Background(), t0.Add(time.Duration(i)*10*time.Second))
	}
	mu.Lock()
	if reqCount != connectorMaxFailures {
		t.Fatalf("连失应请求 %d 次, got %d", connectorMaxFailures, reqCount)
	}
	mu.Unlock()

	// 退避 10 tick = 10s：期间（第 5 次失败 t0+40s → t0+50s 前）不再请求。
	s.runOnce(context.Background(), t0.Add(45*time.Second))
	mu.Lock()
	if reqCount != connectorMaxFailures {
		t.Fatalf("退避期内不应再请求: %d", reqCount)
	}
	mu.Unlock()

	// 退避结束（t0+50s，[until) 语义）后恢复执行（继续失败，但不 panic）。
	s.runOnce(context.Background(), t0.Add(50*time.Second))
	mu.Lock()
	if reqCount != connectorMaxFailures+1 {
		t.Fatalf("退避结束应恢复请求: %d", reqCount)
	}
	mu.Unlock()
}

// TestConnectorSchedulerDisabled 钉死：停用部件不执行；无 dispatch 空转安全。
func TestConnectorSchedulerDisabled(t *testing.T) {
	m := newTestManager(t)
	called := 0
	s := NewConnectorScheduler(m, func(string, string) { called++ })
	var srvCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvCalls++
	}))
	defer api.Close()
	if _, err := m.Save("off", Manifest{Name: "off", Connector: &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: api.URL, IntervalSec: 10,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetEnabled("off", false); err != nil {
		t.Fatal(err)
	}
	s.runOnce(context.Background(), time.Now())
	if called != 0 || srvCalls != 0 {
		t.Fatalf("停用部件不应执行: called=%d srv=%d", called, srvCalls)
	}

	// 未接线空转：nil mgr/dispatch 不 panic。
	empty := NewConnectorScheduler(nil, nil)
	empty.runOnce(context.Background(), time.Now())
}

// TestRunAPIConnector 钉死 api 执行：method/headers(${VAR} 插值)/body 透传、
// 非 2xx 失败、超时钳制生效（用缺省值即可，不额外造慢源）。
func TestRunAPIConnector(t *testing.T) {
	t.Setenv("WIDGET_TEST_KEY", "secret-1")
	var gotMethod, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"ok":1}`))
	}))
	defer srv.Close()

	out, err := RunAPIConnector(context.Background(), &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: srv.URL + "/path",
		Method:  "post",
		Headers: map[string]string{"Authorization": "${WIDGET_TEST_KEY}"},
		Body:    `{"q":1}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":1}` || gotMethod != "POST" || gotAuth != "secret-1" || gotBody != `{"q":1}` {
		t.Fatalf("执行不符: out=%q method=%s auth=%s body=%s", out, gotMethod, gotAuth, gotBody)
	}

	// GET 缺省 + 未设置变量的插值回落空串。
	out, err = RunAPIConnector(context.Background(), &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: srv.URL,
		Headers: map[string]string{"X-Token": "${WIDGET_TEST_MISSING:}"},
	})
	if err != nil || gotMethod != "GET" {
		t.Fatalf("GET 缺省不符: err=%v method=%s", err, gotMethod)
	}

	// 非 2xx → 失败。
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fail.Close()
	if _, err := RunAPIConnector(context.Background(), &ConnectorSpec{
		Type: ConnectorTypeAPI, Target: fail.URL,
	}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("非 2xx 应失败: %v", err)
	}
}

// TestManager_ExecuteConnector 钉死单次执行入口：类型过滤与 404 语义。
func TestManager_ExecuteConnector(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.ExecuteConnector(context.Background(), "ghost", nil); err == nil {
		t.Fatal("不存在部件应报错")
	}
	if _, err := m.Save("plain", Manifest{Name: "p"}, []FileInput{{Path: "index.html", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecuteConnector(context.Background(), "plain", nil); err == nil {
		t.Fatal("无连接器应报错")
	}
	if _, err := m.Save("hook", Manifest{Name: "h", Connector: &ConnectorSpec{Type: ConnectorTypeWebhook}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExecuteConnector(context.Background(), "hook", nil); err == nil {
		t.Fatal("webhook 不支持立即执行")
	}
	caller := &fakeMCPCaller{out: "tool-output"}
	if _, err := m.Save("mcp-w", Manifest{Name: "m", Connector: &ConnectorSpec{
		Type: ConnectorTypeMCP, Target: "some_tool",
	}}, nil); err != nil {
		t.Fatal(err)
	}
	out, err := m.ExecuteConnector(context.Background(), "mcp-w", caller)
	if err != nil || out != "tool-output" {
		t.Fatalf("mcp 执行不符: out=%q err=%v", out, err)
	}
}
