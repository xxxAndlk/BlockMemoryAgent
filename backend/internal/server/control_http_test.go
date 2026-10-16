package server

// control_http_test.go 停止/暂停端点的"可选 body"契约（2026-10-16 统一停止按钮）：
//   - /sessions/:id/stop：无 body 或 `{}` → stop_children 缺省 true（全停——兼容存量
//     无 body 调用与 TUI 发 `{}` 的调用）；{"stop_children":false} → 只停主 Agent；
//   - /sessions/:id/agents/:aid/pause：空 body → cascade 缺省 false（只停本支路）；
//     {"cascade":true} → 连带子树。
// mock 对 Control / PauseAgent 参数原样留痕，逐案断言。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/gin-gonic/gin"
)

// newStopTestRouter 构造挂载 stop/pause 两个 handler 的测试路由与留痕 mock。
func newStopTestRouter(t *testing.T) (*gin.Engine, *mockAgentForServer) {
	t.Helper()
	mock := newMockAgentForServer()
	sm := NewSessionManager(mock)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/sessions/:id/stop", sm.HandleSessionStop)
	r.POST("/api/sessions/:id/agents/:aid/pause", sm.HandleSessionAgentPause)
	return r, mock
}

// postedStop 发送一次 /stop 请求（body 为空串表示无 body）。
func postedStop(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/api/sessions/s1/stop", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/sessions/s1/stop", strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// stopChildrenOf 取 mock 记录的 stop 控制命令的 stop_children 参数。
func stopChildrenOf(t *testing.T, mock *mockAgentForServer) bool {
	t.Helper()
	mock.mu.RLock()
	defer mock.mu.RUnlock()
	if len(mock.controlCmds) == 0 {
		t.Fatal("未记录到 Control 调用")
	}
	cmd := mock.controlCmds[len(mock.controlCmds)-1]
	if cmd.Op != agent.ControlOpStop {
		t.Fatalf("Op=%v want ControlOpStop", cmd.Op)
	}
	v, _ := cmd.Args["stop_children"].(bool)
	return v
}

// TestHandleSessionStopBodyContract 逐案断言 /stop 的 stop_children 解析。
func TestHandleSessionStopBodyContract(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		stopChildren bool
	}{
		{"no_body_defaults_full_stop", "", true},
		{"empty_object_defaults_full_stop", "{}", true},
		{"explicit_true_full_stop", `{"stop_children":true}`, true},
		{"explicit_false_meta_only", `{"stop_children":false}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newStopTestRouter(t)
			rec := postedStop(t, r, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if got := stopChildrenOf(t, mock); got != tc.stopChildren {
				t.Fatalf("stop_children=%v want %v", got, tc.stopChildren)
			}
		})
	}
}

// TestHandleSessionStopRejectsBadBody 截断 JSON 应 400（可选 body 不等于不校验）。
func TestHandleSessionStopRejectsBadBody(t *testing.T) {
	r, _ := newStopTestRouter(t)
	rec := postedStop(t, r, `{"stop_children":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

// TestHandleSessionAgentPauseBodyContract 逐案断言 /pause 的 cascade 解析。
func TestHandleSessionAgentPauseBodyContract(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		cascade bool
	}{
		{"no_body_defaults_branch_only", "", false},
		{"empty_object_defaults_branch_only", "{}", false},
		{"cascade_true", `{"cascade":true}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newStopTestRouter(t)
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(http.MethodPost, "/api/sessions/s1/agents/a1/pause", nil)
			} else {
				req = httptest.NewRequest(http.MethodPost, "/api/sessions/s1/agents/a1/pause", strings.NewReader(tc.body))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			mock.mu.RLock()
			calls := append([]mockPauseCall(nil), mock.pauseCalls...)
			mock.mu.RUnlock()
			if len(calls) != 1 || calls[0].instID != "a1" || calls[0].cascade != tc.cascade {
				t.Fatalf("pauseCalls=%+v want inst=a1 cascade=%v", calls, tc.cascade)
			}
		})
	}
}
