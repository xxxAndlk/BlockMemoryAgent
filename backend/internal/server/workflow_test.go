package server

// workflow_test.go 工作流端点的接线测试（M4 验收面）：未接线 503、输入校验 400、unknown action 400。
// 全端点行为（状态迁移/人审操作）由 internal/workflow 包测试覆盖（真引擎 + 内存 store）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/workflow"
)

func newWorkflowTestRouter(eng *workflow.Engine) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewWorkflowHandler(eng)
	h.RegisterRoutes(r.Group(""))
	return r
}

func doReq(t *testing.T, r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWorkflowHandlerNilEngine503(t *testing.T) {
	r := newWorkflowTestRouter(nil)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/workflow/recipes?workdir=/tmp"},
		{"GET", "/workflow/runs"},
		{"POST", "/workflow/runs"},
		{"POST", "/workflow/runs/wf-1/pause"},
		{"POST", "/workflow/units/u1/accept"},
	} {
		w := doReq(t, r, tc.method, tc.path, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s 应 503, got %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestWorkflowHandlerValidation(t *testing.T) {
	// 引擎可达（store 为 nil —— 校验分支在建 run 前返回，不触达 store）。
	eng := workflow.NewEngine(workflow.Config{}, nil, nil, nil)
	r := newWorkflowTestRouter(eng)

	// recipes 缺 workdir → 400。
	if w := doReq(t, r, "GET", "/workflow/recipes", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("recipes 缺 workdir 应 400, got %d", w.Code)
	}
	// create run 缺字段 → 400。
	if w := doReq(t, r, "POST", "/workflow/runs", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("缺 session_id/recipe_id 应 400, got %d body=%s", w.Code, w.Body.String())
	}
	// 配方不存在 → 400（EstimateRun 在 store 之前失败）。
	body, _ := json.Marshal(map[string]any{
		"session_id": "s1", "workdir": t.TempDir(), "recipe_id": "nope", "roster": `[{"id":"a"}]`,
	})
	w := doReq(t, r, "POST", "/workflow/runs", string(body))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不存在") {
		t.Fatalf("配方不存在应 400, got %d body=%s", w.Code, w.Body.String())
	}
	// 未知 unit 操作 → 400。
	if w := doReq(t, r, "POST", "/workflow/units/u1/explode", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("未知操作应 400, got %d", w.Code)
	}
	// PUT recipe 缺 yaml → 400。
	if w := doReq(t, r, "PUT", "/workflow/recipes/x", `{"workdir":"/tmp"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("缺 yaml 应 400, got %d", w.Code)
	}
}
