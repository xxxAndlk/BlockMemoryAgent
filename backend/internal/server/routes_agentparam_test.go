package server

// routes_agentparam_test.go 钉死"子 Agent 实例 ID 含 /"的路由可达性：
// inst_id 形如 "session-1/domain-2"，前端必须经 encodeURIComponent 转义（%2F）才能塞进
// 单段路径参数。若 gin 按解码后的 URL.Path 匹配，%2F 会被当作路径分隔符 → 404，
// 编排页对任何子 Agent 都不可用（只剩 meta 能通）。本用例是该回归的护栏。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/agent"
)

// newOrchTestRouter 构造仅含会话路由的引擎；rawPath 控制是否开启 RawPath 匹配
// （生产 NewDefaultRouter 必须为 true，否则 :aid 段承载不了含 "/" 的实例 ID）。
func newOrchTestRouter(t *testing.T, rawPath bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.UseRawPath = rawPath
	r.UnescapePathValues = true
	if rawPath {
		r.RemoveExtraSlash = true
	}
	RegisterSessionRoutes(r.Group("/api"), NewSessionManager(newMockAgentForServer()))
	return r
}

// TestAgentScopedRoutesAcceptEncodedSlash 验证 aid 含 %2F 时子资源路由可命中。
func TestAgentScopedRoutesAcceptEncodedSlash(t *testing.T) {
	r := newOrchTestRouter(t, true)
	enc := "session-1%2Fdomain-2"

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"messages", http.MethodGet, "/api/sessions/s1/agents/" + enc + "/messages", ""},
		{"message", http.MethodPost, "/api/sessions/s1/agents/" + enc + "/message", `{"content":"hi"}`},
		{"events", http.MethodGet, "/api/sessions/s1/agents/" + enc + "/events", ""},
		{"pause", http.MethodPost, "/api/sessions/s1/agents/" + enc + "/pause", ""},
		{"cancel", http.MethodPost, "/api/sessions/s1/agents/" + enc + "/cancel", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != "" {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s → 404：含 %%2F 的实例 ID 未命中路由", tc.method, tc.path)
			}
		})
	}
}

// TestAgentScopedRoutesHandlerSeesDecodedID 验证 handler 拿到的是解码后的完整实例 ID
// （gin 在 RawPath 模式下对路径参数再解码；拿不到原 ID 就等于打到了别的 Agent）。
func TestAgentScopedRoutesHandlerSeesDecodedID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.UseRawPath = true
	r.UnescapePathValues = true
	var got string
	r.GET("/x/:aid/events", func(c *gin.Context) { got = c.Param("aid") })
	req := httptest.NewRequest(http.MethodGet, "/x/session-1%2Fdomain-2/events", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if got != "session-1/domain-2" {
		t.Fatalf("aid 解出 %q，want %q", got, "session-1/domain-2")
	}
}

// TestAgentScopedRoutesNeedRawPath 钉死"为什么生产引擎必须开 RawPath"：
// 不开时 gin 按解码后的 URL.Path 匹配，%2F 变成路径分隔符 → 含 "/" 的实例 ID 全部 404。
func TestAgentScopedRoutesNeedRawPath(t *testing.T) {
	r := newOrchTestRouter(t, false)
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/s1/agents/session-1%2Fdomain-2/messages", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未开 RawPath 时应 404（本用例即该回归的说明），got %d", rec.Code)
	}
}

var _ = agent.QueryKindAgentMessages // 引用 agent 包，保持与生产路由同口径的 import 面
