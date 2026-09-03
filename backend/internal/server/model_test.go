package server

// model_test.go 覆盖模型动态切换 API：httptest 走与生产同构的路由注册，
// 验证 GET /api/models 目录 + POST /api/models/switch 切换闭环（含非法输入 400）。
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

// modelManagerStub 最小 ModelManager 桩：记录切换调用，返回固定目录。
type modelManagerStub struct {
	switched  []string
	switchErr error
	presets   []types.ModelPreset
	roles     []agent.RoleModelStatus
}

func (m *modelManagerStub) ListModels(ctx context.Context) (*agent.ModelCatalog, error) {
	return &agent.ModelCatalog{Presets: m.presets, Roles: m.roles}, nil
}

func (m *modelManagerStub) SwitchModel(ctx context.Context, roleID, presetID string) (types.AgentModelConfig, error) {
	if m.switchErr != nil {
		return types.AgentModelConfig{}, m.switchErr
	}
	m.switched = append(m.switched, roleID+"→"+presetID)
	return types.AgentModelConfig{Provider: "openai", Model: "glm-5.3-flash"}, nil
}

func newModelTestRouter(h *APIHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(GinAuthMiddleware("", nil))
	r.GET("/api/models", h.ListModelsHandler)
	r.POST("/api/models/switch", h.SwitchModelHandler)
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
		presets: []types.ModelPreset{{ID: "glm-flash", Provider: "openai", Model: "glm-5.3-flash"}},
		roles:   []agent.RoleModelStatus{{RoleID: "meta", Provider: "openai", Model: "meta-old"}},
	}
	api := NewAPIHandler(nil)
	api.SetModelManager(mgr)
	r := newModelTestRouter(api)

	// 目录
	rec := doJSON(t, r, http.MethodGet, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d", rec.Code)
	}
	var catalog agent.ModelCatalog
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("catalog json: %v", err)
	}
	if len(catalog.Presets) != 1 || catalog.Presets[0].ID != "glm-flash" {
		t.Fatalf("presets 错误: %+v", catalog.Presets)
	}
	if len(catalog.Roles) != 1 || catalog.Roles[0].RoleID != "meta" || catalog.Roles[0].Model != "meta-old" {
		t.Fatalf("roles 错误: %+v", catalog.Roles)
	}

	// 切换
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","preset":"glm-flash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("switch json: %v", err)
	}
	if resp.Status != "ok" || resp.Model != "glm-5.3-flash" {
		t.Fatalf("switch 结果错误: %+v", resp)
	}
	if len(mgr.switched) != 1 || mgr.switched[0] != "meta→glm-flash" {
		t.Fatalf("switched = %v", mgr.switched)
	}

	// 切换失败（如探测失败）→ 400
	mgr.switchErr = &mockSwitchErr{}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","preset":"glm-flash"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("switch error 应 400，got %d", rec.Code)
	}

	// 非法 body
	rec = postJSON(t, r, "/api/models/switch", `not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid body 应 400，got %d", rec.Code)
	}
	rec = postJSON(t, r, "/api/models/switch", `{"role":"","preset":"x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing role 应 400，got %d", rec.Code)
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
	rec = postJSON(t, r, "/api/models/switch", `{"role":"meta","preset":"p"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无 manager switch 应 503，got %d", rec.Code)
	}
}
