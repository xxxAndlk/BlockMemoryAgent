package server

// session_list_http_test.go 覆盖 GET /api/sessions 的列表线型：
//   - 只下发摘要字段（events/messages/state 不下发——单会话事件流实测可达数 MB，
//     整包下发会把首页首屏拖慢并把前端内存顶起来；详情页走 GET /sessions/{id}）；
//   - ?limit= 透传到 agent.Filter.Limit，非法值回落默认条数。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// newListTestRouter 构造挂载列表端点的测试路由与 mock agent。
func newListTestRouter(t *testing.T) (*gin.Engine, *mockAgentForServer) {
	t.Helper()
	mock := newMockAgentForServer()
	mgr := NewSessionManager(mock)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/sessions", mgr.HandleListSessions)
	return r, mock
}

// TestHandleListSessions_SummaryPayload 列表返回摘要：重字段（events/messages/state）不出现在响应体里。
func TestHandleListSessions_SummaryPayload(t *testing.T) {
	r, mock := newListTestRouter(t)
	sess, err := mock.CreateSession(context.Background(), agent.CreateRequest{Goal: "摘要线型"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// mock 建会话时自带 Messages；补一条事件，确保两条重字段路径都被投影挡掉。
	sess.Events = []agent.Event{{Type: "llm", Message: "hello"}}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, banned := range []string{`"events"`, `"messages"`, `"state"`} {
		if strings.Contains(body, banned) {
			t.Errorf("列表响应不应包含 %s，实际 %s", banned, body)
		}
	}
	if !strings.Contains(body, `"id":"`+sess.ID+`"`) || !strings.Contains(body, `"goal":"摘要线型"`) {
		t.Errorf("列表响应缺少摘要字段: %s", body)
	}
}

// TestHandleListSessions_Limit limit 透传：?limit=2 只回 2 条；非法值回落默认（不报错、不截断）。
func TestHandleListSessions_Limit(t *testing.T) {
	r, mock := newListTestRouter(t)
	for i := 0; i < 3; i++ {
		if _, err := mock.CreateSession(context.Background(), agent.CreateRequest{Goal: "会话"}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sessions?limit=2", nil))
	var got []sessionSummary
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if len(got) != 2 {
		t.Errorf("limit=2 返回 %d 条，want 2", len(got))
	}

	wBad := httptest.NewRecorder()
	r.ServeHTTP(wBad, httptest.NewRequest(http.MethodGet, "/api/sessions?limit=abc", nil))
	var fallback []sessionSummary
	if err := json.Unmarshal(wBad.Body.Bytes(), &fallback); err != nil {
		t.Fatalf("unmarshal fallback: %v (body=%s)", err, wBad.Body.String())
	}
	if len(fallback) != 3 {
		t.Errorf("非法 limit 应回落默认（3 条全回），实际 %d 条", len(fallback))
	}
}
