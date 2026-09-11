package server

// session_delete_http_test.go 覆盖会话硬删除 HTTP 端点：
//   - DELETE /api/sessions/{id}：200 + 数据摘除；重复删除 404。
//   - POST /api/sessions/delete：批量逐条结果（partial success）；空 ids 400。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// newDeleteTestRouter 构造挂载删除端点的测试路由与 mock agent。
func newDeleteTestRouter(t *testing.T) (*gin.Engine, *mockAgentForServer) {
	t.Helper()
	mock := newMockAgentForServer()
	mgr := NewSessionManager(mock)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.DELETE("/api/sessions/:id", mgr.HandleDeleteSession)
	r.POST("/api/sessions/delete", mgr.HandleDeleteSessions)
	r.GET("/api/sessions/:id", mgr.HandleGetSession)
	return r, mock
}

// TestHandleDeleteSession_HardDelete 单会话删除：200 后会话不可再查，重复删除 404。
func TestHandleDeleteSession_HardDelete(t *testing.T) {
	r, mock := newDeleteTestRouter(t)
	sess, err := mock.CreateSession(context.Background(), agent.CreateRequest{Goal: "待删除"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"deleted"`) {
		t.Errorf("delete body = %s, want status deleted", w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID, nil))
	if w2.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w2.Code)
	}

	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sess.ID, nil))
	if w3.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", w3.Code)
	}
}

// TestHandleDeleteSessions_BatchPartial 批量删除：存在的删掉、不存在的进 errors，整体 200。
func TestHandleDeleteSessions_BatchPartial(t *testing.T) {
	r, mock := newDeleteTestRouter(t)
	a, _ := mock.CreateSession(context.Background(), agent.CreateRequest{Goal: "a"})
	b, _ := mock.CreateSession(context.Background(), agent.CreateRequest{Goal: "b"})

	body := `{"ids":["` + a.ID + `","` + b.ID + `","session-404"]}`
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sessions/delete", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if !strings.Contains(got, a.ID) || !strings.Contains(got, b.ID) {
		t.Errorf("deleted list missing ids: %s", got)
	}
	if !strings.Contains(got, "session-404") || !strings.Contains(got, `"errors"`) {
		t.Errorf("errors list should mention session-404: %s", got)
	}
}

// TestHandleDeleteSessions_EmptyIds 空 ids 400（防御直连 API 的调用方）。
func TestHandleDeleteSessions_EmptyIds(t *testing.T) {
	r, _ := newDeleteTestRouter(t)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sessions/delete", strings.NewReader(`{"ids":[]}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty ids status = %d, want 400", w.Code)
	}
}
