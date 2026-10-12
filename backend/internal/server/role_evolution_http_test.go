package server

// role_evolution_http_test.go 验证角色自进化准则 API：
// PUT /api/roles/:id/evolution-prompt（503 未接线 / 404 未知角色 / 400 超限且文案含字数 /
// 200 恰好 300 字 / 200 空串清空）；GET /api/roles/evolution-prompts（注册表全集 +
// 存储孤立 role_id，role_name 正确映射）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// fakeRoleEvolutionStore 内存版 roleEvolutionStoreOps（真实现裹 *sql.DB 无法单测直造）。
type fakeRoleEvolutionStore struct {
	mu   sync.Mutex
	data map[string]store.RoleEvolutionPrompt
}

func newFakeRoleEvolutionStore() *fakeRoleEvolutionStore {
	return &fakeRoleEvolutionStore{data: map[string]store.RoleEvolutionPrompt{}}
}

func (f *fakeRoleEvolutionStore) Get(_ context.Context, roleID string) (*store.RoleEvolutionPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.data[roleID]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

func (f *fakeRoleEvolutionStore) Upsert(_ context.Context, rec *store.RoleEvolutionPrompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[rec.RoleID] = *rec
	return nil
}

func (f *fakeRoleEvolutionStore) List(_ context.Context) ([]store.RoleEvolutionPrompt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.RoleEvolutionPrompt, 0, len(f.data))
	for _, rec := range f.data {
		out = append(out, rec)
	}
	return out, nil
}

// newRoleEvolutionRouter 构造挂真实 Registry（meta/domain 合成 + 一个 fixed 角色）与
// fake 存储的测试路由（同包测试直接塞私有字段，绕过 *store.RoleEvolutionStore 具体类型）。
func newRoleEvolutionRouter(t *testing.T) (*gin.Engine, *fakeRoleEvolutionStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := newFakeRoleEvolutionStore()
	cfg := &config.RoleConfigFile{
		MetaAgent: config.MetaAgentConfig{SystemPrompt: "meta prompt"},
		FixedRoles: []types.RoleDefinition{
			{ID: "code_assistant", Name: "代码助手", Type: enums.RoleTypeFixed, CanBeCalled: true},
		},
	}
	h := NewAPIHandler(nil)
	h.roleEvolution = fake
	h.roleRegistry = role.NewRegistry(cfg)
	r := gin.New()
	r.GET("/api/roles/evolution-prompts", h.ListRoleEvolutionPromptsHandler)
	r.PUT("/api/roles/:id/evolution-prompt", h.SaveRoleEvolutionPromptHandler)
	return r, fake
}

func doRoleEvolutionReq(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	return rec
}

// TestSaveRoleEvolutionPrompt 覆盖 PUT 的 200（300 字整/空串清空/内置角色）与 400/404 分支。
func TestSaveRoleEvolutionPrompt(t *testing.T) {
	r, fake := newRoleEvolutionRouter(t)
	put := func(path, body string) *httptest.ResponseRecorder {
		return doRoleEvolutionReq(r, http.MethodPut, path, body)
	}

	// 1) 200：恰好 300 字保存成功并回显记录。
	content300 := strings.Repeat("准", 300)
	rec := put("/api/roles/code_assistant/evolution-prompt", `{"content":"`+content300+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("300 字应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var saved store.RoleEvolutionPrompt
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if saved.RoleID != "code_assistant" || saved.Content != content300 {
		t.Fatalf("回显不符: %+v", saved)
	}
	if got := fake.data["code_assistant"].Content; got != content300 {
		t.Fatalf("存储内容不符: %q", got)
	}

	// 2) 400：301 字超限，文案含当前字数。
	content301 := strings.Repeat("字", 301)
	rec = put("/api/roles/code_assistant/evolution-prompt", `{"content":"`+content301+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("301 字应 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "301") {
		t.Fatalf("超限文案应含当前字数, got %s", rec.Body.String())
	}

	// 3) 400：缺 content 字段 / content 非字符串。
	if rec = put("/api/roles/code_assistant/evolution-prompt", `{"foo":"bar"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 content 字段应 400, got %d", rec.Code)
	}
	if rec = put("/api/roles/code_assistant/evolution-prompt", `{"content":123}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("content 非字符串应 400, got %d", rec.Code)
	}

	// 4) 404：未知角色。
	if rec = put("/api/roles/ghost/evolution-prompt", `{"content":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("未知角色应 404, got %d", rec.Code)
	}

	// 5) 200：空串 = 清空该角色准则。
	rec = put("/api/roles/code_assistant/evolution-prompt", `{"content":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("空串清空应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := fake.data["code_assistant"].Content; got != "" {
		t.Fatalf("清空后应为空串, got %q", got)
	}

	// 6) meta/domain 内置角色同样可编辑。
	if rec = put("/api/roles/meta/evolution-prompt", `{"content":"先提交计划再动手"}`); rec.Code != http.StatusOK {
		t.Fatalf("meta 应可编辑, got %d", rec.Code)
	}
}

// TestRoleEvolutionNotWired 未注入存储/注册表时两个端点均 503。
func TestRoleEvolutionNotWired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(nil)
	r := gin.New()
	r.GET("/api/roles/evolution-prompts", h.ListRoleEvolutionPromptsHandler)
	r.PUT("/api/roles/:id/evolution-prompt", h.SaveRoleEvolutionPromptHandler)

	if rec := doRoleEvolutionReq(r, http.MethodPut, "/api/roles/meta/evolution-prompt", `{"content":"x"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线 PUT 应 503, got %d", rec.Code)
	}
	if rec := doRoleEvolutionReq(r, http.MethodGet, "/api/roles/evolution-prompts", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接线 GET 应 503, got %d", rec.Code)
	}
}

// TestListRoleEvolutionPrompts 覆盖 GET：注册表全集 + 存储孤立 role_id，role_name 映射/回落。
func TestListRoleEvolutionPrompts(t *testing.T) {
	r, fake := newRoleEvolutionRouter(t)
	// 预置：code_assistant 有准则；ghost_role 为存储孤立 role_id（注册表没有）。
	fake.data["code_assistant"] = store.RoleEvolutionPrompt{
		RoleID: "code_assistant", Content: "先跑测试再交付",
		SourceSession: "session-9", UpdatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	fake.data["ghost_role"] = store.RoleEvolutionPrompt{RoleID: "ghost_role", Content: "遗留准则"}

	rec := doRoleEvolutionReq(r, http.MethodGet, "/api/roles/evolution-prompts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Prompts []roleEvolutionPromptItem `json:"prompts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	byID := map[string]roleEvolutionPromptItem{}
	for _, p := range resp.Prompts {
		byID[p.RoleID] = p
	}
	// 覆盖全集 = meta/domain（配置合成）+ code_assistant（fixed）+ ghost_role（存储孤立）。
	for _, id := range []string{"meta", "domain", "code_assistant", "ghost_role"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("缺少角色 %s: %+v", id, resp.Prompts)
		}
	}
	if got := byID["meta"].RoleName; got != "MetaAgent" {
		t.Errorf("meta role_name = %q, 应 MetaAgent", got)
	}
	if got := byID["domain"].RoleName; got != "DomainAgent" {
		t.Errorf("domain role_name = %q, 应 DomainAgent", got)
	}
	if got := byID["code_assistant"].RoleName; got != "代码助手" {
		t.Errorf("code_assistant role_name = %q, 应 代码助手", got)
	}
	if got := byID["code_assistant"].Content; got != "先跑测试再交付" {
		t.Errorf("code_assistant content = %q", got)
	}
	if got := byID["code_assistant"].SourceSession; got != "session-9" {
		t.Errorf("code_assistant source_session = %q", got)
	}
	// 孤立 role_id：role_name 回落 role_id。
	if got := byID["ghost_role"].RoleName; got != "ghost_role" {
		t.Errorf("孤立 role_name 应回落 role_id, got %q", got)
	}
	// 无准则角色：content 空串照常返回（前端显示「暂无准则」）。
	if got := byID["domain"].Content; got != "" {
		t.Errorf("无准则角色 content 应为空串, got %q", got)
	}
}
