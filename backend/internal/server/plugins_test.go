package server

// plugins_test.go 覆盖插件管理 API（设计文档 §5、Phase 2.4）：
// httptest 走真实路由注册（与 bootstrap/mux.go 同款 AuthMiddleware 包装），
// 验证 enable → 工具出现在 Schema → disable → 工具消失的完整闭环。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/config"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/plugins"
	"github.com/google/jsonschema-go/jsonschema"
)

// apiTestTool 模拟插件工具。
type apiTestTool struct{ name string }

func (a *apiTestTool) Name() string      { return a.name }
func (a *apiTestTool) Aliases() []string { return nil }
func (a *apiTestTool) Description() string { return "api 测试工具" }
func (a *apiTestTool) InputSchema() *jsonschema.Schema { return nil }
func (a *apiTestTool) Execute(ctx context.Context, args map[string]any) *tool.Result {
	return &tool.Result{Tool: a.name, Success: true, Output: "ok"}
}

// apiFakePlugin 极简插件桩。
type apiFakePlugin struct {
	id   string
	stop bool
}

func (f *apiFakePlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{ID: f.id, Name: f.id, Kind: plugins.KindMCP, Roles: []string{"*"}}
}
func (f *apiFakePlugin) Init(ctx context.Context, deps plugins.Deps) error { return nil }
func (f *apiFakePlugin) Start(ctx context.Context) error                   { return nil }
func (f *apiFakePlugin) Stop(ctx context.Context) error                    { f.stop = true; return nil }
func (f *apiFakePlugin) Tools() []tool.Tool {
	return []tool.Tool{&apiTestTool{name: f.id + "_tool"}}
}

// newPluginTestMux 构造与生产同构的路由（插件端点 + AuthMiddleware 空 token 放行）。
func newPluginTestMux(h *APIHandler) *http.ServeMux {
	mux := http.NewServeMux()
	wrap := func(fn http.HandlerFunc) http.HandlerFunc {
		return AuthMiddleware("", nil, fn)
	}
	mux.HandleFunc("/api/plugins", wrap(h.ListPluginsHandler))
	mux.HandleFunc("/api/plugins/{id}", wrap(h.GetPluginHandler))
	mux.HandleFunc("/api/plugins/{id}/enable", wrap(h.EnablePluginHandler))
	mux.HandleFunc("/api/plugins/{id}/disable", wrap(h.DisablePluginHandler))
	mux.HandleFunc("/api/plugins/reload", wrap(h.ReloadPluginsHandler))
	return mux
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPluginAPIEnableDisableLoop 验证管理 API 闭环。
func TestPluginAPIEnableDisableLoop(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	enabledFlag := false
	mgr := plugins.NewManager(reg,
		plugins.WithMCPFactory(func(id string, settings map[string]any, deps plugins.Deps) (plugins.Plugin, error) {
			return &apiFakePlugin{id: id}, nil
		}),
		plugins.WithConfig(&config.PluginsConfig{Plugins: map[string]config.PluginConfig{
			"p": {Kind: "mcp", EnabledFlag: &enabledFlag, Settings: map[string]any{"command": "x"}},
		}}),
	)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	api := NewAPIHandler(nil)
	api.SetPluginManager(mgr)
	mux := newPluginTestMux(api)

	// 列表：1 个插件，registered 态。
	rec := doJSON(t, mux, http.MethodGet, "/api/plugins")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var list struct {
		Plugins []plugins.Info `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(list.Plugins) != 1 || list.Plugins[0].ID != "p" || list.Plugins[0].State != plugins.StateRegistered {
		t.Fatalf("列表内容错误: %+v", list.Plugins)
	}
	if len(list.Plugins[0].Tools) != 0 {
		t.Fatal("registered 态不应有工具")
	}

	// enable → 工具出现在 Registry/Schema。
	rec = doJSON(t, mux, http.MethodPost, "/api/plugins/p/enable")
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if !reg.Has("p_tool") {
		t.Fatal("enable 后工具应注册")
	}
	schemaHasTool := false
	for _, s := range reg.Schema() {
		if s.Name() == "p_tool" {
			schemaHasTool = true
		}
	}
	if !schemaHasTool {
		t.Fatal("enable 后工具应出现在 Schema")
	}
	var info plugins.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("enable json: %v", err)
	}
	if info.State != plugins.StateRunning || len(info.Tools) != 1 || info.Tools[0] != "p_tool" {
		t.Fatalf("enable 结果错误: %+v", info)
	}

	// 详情。
	rec = doJSON(t, mux, http.MethodGet, "/api/plugins/p")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"running"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}

	// disable → 工具消失。
	rec = doJSON(t, mux, http.MethodPost, "/api/plugins/p/disable")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d", rec.Code)
	}
	if reg.Has("p_tool") {
		t.Fatal("disable 后工具应摘除")
	}

	// 未知插件 404 / enable 不存在 400。
	rec = doJSON(t, mux, http.MethodGet, "/api/plugins/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知插件应 404，got %d", rec.Code)
	}
	rec = doJSON(t, mux, http.MethodPost, "/api/plugins/nope/enable")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知插件 enable 应 400，got %d", rec.Code)
	}
}

// TestPluginAPIReload 验证 reload 端点重读 plugins.yaml（新增插件出现、移除插件消失）。
func TestPluginAPIReload(t *testing.T) {
	reg := tool.NewBuiltinRegistry(t.TempDir(), nil, nil)
	cfgDir := t.TempDir()
	writeFile := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfgDir, "plugins.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("plugins:\n  a:\n    kind: mcp\n")
	created := map[string]bool{}
	mgr := plugins.NewManager(reg,
		plugins.WithMCPFactory(func(id string, settings map[string]any, deps plugins.Deps) (plugins.Plugin, error) {
			created[id] = true
			return &apiFakePlugin{id: id}, nil
		}),
		plugins.WithConfigDir(cfgDir),
	)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	api := NewAPIHandler(nil)
	api.SetPluginManager(mgr)
	mux := newPluginTestMux(api)

	// 重载前：仅有 a。
	rec := doJSON(t, mux, http.MethodGet, "/api/plugins")
	if !strings.Contains(rec.Body.String(), `"id":"a"`) {
		t.Fatalf("重载前应只有 a: %s", rec.Body.String())
	}
	// 磁盘配置改为 b，reload 后 a 消失、b 出现。
	writeFile("plugins:\n  b:\n    kind: mcp\n")
	rec = doJSON(t, mux, http.MethodPost, "/api/plugins/reload")
	if rec.Code != http.StatusOK {
		t.Fatalf("reload: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"b"`) || strings.Contains(rec.Body.String(), `"id":"a"`) {
		t.Fatalf("reload 后应只有 b: %s", rec.Body.String())
	}
	if !created["b"] {
		t.Fatal("reload 应装载 b")
	}
}
