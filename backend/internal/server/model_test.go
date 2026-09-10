package server

// model_test.go 覆盖模型动态切换 API：httptest 走与生产同构的路由注册，
// 验证 GET /api/models 目录（响应不含 api_key）+ POST /api/models/switch 切换
// + POST /api/models 新增条目（含非法输入 400）。
// ModelManager 由 ReactService 实现并通过类型断言注入，此处用最小桩模拟。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin" // Gin Web 框架（测试路由）

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// modelManagerStub 最小 ModelManager 桩：记录切换/新增调用，返回固定目录。
type modelManagerStub struct {
	switched  []string
	switchErr error
	added     []types.ModelEntry
	addErr    error
	models    []agent.ModelEntryView
	roles     []agent.RoleModelStatus
}

func (m *modelManagerStub) ListModels(ctx context.Context) (*agent.ModelCatalog, error) {
	return &agent.ModelCatalog{Models: m.models, Roles: m.roles}, nil
}

func (m *modelManagerStub) SwitchModel(ctx context.Context, roleID, modelID, thinking string) (types.AgentModelConfig, error) {
	if m.switchErr != nil {
		return types.AgentModelConfig{}, m.switchErr
	}
	m.switched = append(m.switched, roleID+"→"+modelID+"→"+thinking)
	return types.AgentModelConfig{Provider: "openai", Model: "glm-5.3-flash"}, nil
}

func (m *modelManagerStub) AddModelEntry(entry types.ModelEntry) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.added = append(m.added, entry)
	return nil
}

func newModelTestRouter(h *APIHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinAuthMiddleware("", nil))
	r.GET("/api/models", h.ListModelsHandler)
	r.POST("/api/models/switch", h.SwitchModelHandler)
	r.POST("/api/models", h.AddModelHandler)
	return r
}

func postJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestModelAPICatalogAndSwitch 验证目录 + 切换 + 非法输入。
func TestModelAPICatalogAndSwitch(t *testing.T) {
	mgr := &modelManagerStub{
		models: []agent.ModelEntryView{{ID: "glm-flash", Provider: "openai", Model: "glm-5.3-flash", BaseURL: "http://b"}},
		roles:  []agent.RoleModelStatus{{RoleID: "meta", Provider: "openai", Model: "meta-old", ModelID: "glm-flash", Bound: true}},
	}
	api := NewAPIHandler(nil)
	api.SetModelManager(mgr)
	r := newModelTestRouter(api)

	// 目录：结构与 api_key 不泄漏。
	rec := doJSON(t, r, http.MethodGet, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "api_key") {
		t.Fatalf("目录响应不得含 api_key: %s", body)
	}
	var catalog agent.ModelCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("catalog json: %v", err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != "glm-flash" || catalog.Models[0].BaseURL != "http://b" {
		t.Fatalf("models 错误: %+v", catalog.Models)
	}
	if len(catalog.Roles) != 1 || catalog.Roles[0].RoleID != "meta" || !catalog.Roles[0].Bound || catalog.Roles[0].ModelID != "glm-flash" {
		t.Fatalf("roles 错误: %+v", catalog.Roles)
	}

	// 切换（新 body：role/model_id/thinking）。
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","model_id":"glm-flash","thinking":"low"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status  string `json:"status"`
		Role    string `json:"role"`
		ModelID string `json:"model_id"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("switch json: %v", err)
	}
	if resp.Status != "ok" || resp.Role != "meta" || resp.ModelID != "glm-flash" || resp.Model != "glm-5.3-flash" {
		t.Fatalf("switch 结果错误: %+v", resp)
	}
	if len(mgr.switched) != 1 || mgr.switched[0] != "meta→glm-flash→low" {
		t.Fatalf("switched = %v", mgr.switched)
	}

	// 切换失败（如探测失败）→ 400。
	mgr.switchErr = &mockSwitchErr{}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","model_id":"glm-flash","thinking":"low"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("switch error 应 400，got %d", rec.Code)
	}
	mgr.switchErr = nil

	// 非法 body。
	rec = postJSON(t, r, "/api/models/switch", `not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body 应 400，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"","model_id":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing role 应 400，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model_id 应 400，got %d", rec.Code)
	}
}

// TestModelAPIAddModel 验证新增条目：显式 id 直传、缺 provider/model 400、
// 上游错误透传 400。
func TestModelAPIAddModel(t *testing.T) {
	mgr := &modelManagerStub{}
	api := NewAPIHandler(nil)
	api.SetModelManager(mgr)
	r := newModelTestRouter(api)

	// 显式 id。
	rec := postJSON(t, r, "/api/models", `{"id":"kimi","provider":"anthropic","model":"kimi-k3","api_key":"sk-1","base_url":"http://b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		ID     string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("add json: %v", err)
	}
	if resp.Status != "ok" || resp.ID != "kimi" {
		t.Fatalf("add 结果错误: %+v", resp)
	}
	if len(mgr.added) != 1 || mgr.added[0].ID != "kimi" || mgr.added[0].APIKey != "sk-1" || mgr.added[0].Model != "kimi-k3" {
		t.Fatalf("added = %+v", mgr.added)
	}

	// 缺 provider / model → 400。
	rec = postJSON(t, r, "/api/models", `{"provider":"","model":"m"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing provider 应 400，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models", `{"provider":"openai"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model 应 400，got %d", rec.Code)
	}
	// 坏 JSON。
	rec = postJSON(t, r, "/api/models", `{bad`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body 应 400，got %d", rec.Code)
	}
	// 上游错误（如 id 冲突）→ 400。
	mgr.addErr = &mockSwitchErr{}
	rec = postJSON(t, r, "/api/models", `{"id":"kimi","provider":"openai","model":"m"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("add error 应 400，got %d", rec.Code)
	}
}

// mockSwitchErr 满足 error 的固定错误。
type mockSwitchErr struct{}

func (*mockSwitchErr) Error() string { return "probe failed" }

// TestModelAPIWithoutManager 未注入能力时 503。
func TestModelAPIWithoutManager(t *testing.T) {
	api := NewAPIHandler(nil)
	r := newModelTestRouter(api)
	rec := doJSON(t, r, http.MethodGet, "/api/models")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 manager 应 503，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","model_id":"p"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 manager switch 应 503，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models", `{"provider":"p","model":"m"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 manager add 应 503，got %d", rec.Code)
	}
}
