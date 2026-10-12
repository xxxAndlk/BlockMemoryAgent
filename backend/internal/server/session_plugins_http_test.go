package server

// session_plugins_http_test.go 验证 POST /api/sessions/:id/plugins（KIMI 式插件选择）：
// 合法列表 200 回显并经 Control 下发；空数组=清空（合法 200）；会话不存在 404；
// 无效插件 400（ErrInvalidSessionState 映射）；建会话透传 CreateRequest.PinnedPlugins。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"

	"github.com/gin-gonic/gin"
)

// TestHandleSessionPlugins 验证指定插件切换端点。
func TestHandleSessionPlugins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &captureControlAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions/:id/plugins", mgr.HandleSessionPlugins)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/plugins", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1) 合法列表：200 回显 + Control 入参正确。
	rec := do(`{"plugins":["plugA","plugB"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法列表 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SessionID string   `json:"session_id"`
		Plugins   []string `json:"plugins"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.SessionID != "s1" || len(resp.Plugins) != 2 || resp.Plugins[0] != "plugA" {
		t.Fatalf("回显不符: %+v", resp)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 || cap.ops[0].Op != agent.ControlOpPinnedPlugins {
		t.Fatalf("Control 入参不符: %+v", cap.ops)
	}
	gotPlugins, ok := cap.ops[0].Args["plugins"].([]string)
	if !ok || len(gotPlugins) != 2 || gotPlugins[1] != "plugB" {
		t.Fatalf("plugins 参数不符: %+v", cap.ops[0].Args)
	}
	cap.mu.Unlock()

	// 2) 空数组=清空：合法 200 并下发空列表。
	rec = do(`{"plugins":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 status=%d body=%s", rec.Code, rec.Body.String())
	}
	cap.mu.Lock()
	if len(cap.ops) != 2 {
		t.Fatalf("清空应触达 agent 层, ops=%+v", cap.ops)
	}
	if got, ok := cap.ops[1].Args["plugins"].([]string); !ok || len(got) != 0 {
		t.Fatalf("清空应下发空列表, got %+v", cap.ops[1].Args)
	}
	cap.mu.Unlock()

	// 3) 会话不存在：404（ErrSessionNotFound 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = agent.ErrSessionNotFound
	cap.mu.Unlock()
	if rec := do(`{"plugins":["plugA"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应 404, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 4) 无效插件：400（ErrInvalidSessionState 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = fmt.Errorf("%w: 无效插件（不存在/未启用/未运行）: ghost", agent.ErrInvalidSessionState)
	cap.mu.Unlock()
	if rec := do(`{"plugins":["ghost"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("无效插件应 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleCreateSessionPlugins 验证建会话携带初始指定插件（KIMI 式插件选择）。
func TestHandleCreateSessionPlugins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &captureCreateAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions", mgr.HandleCreateSession)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1) 携带 plugins：200 且透传 CreateRequest.PinnedPlugins。
	if rec := do(`{"goal":"x","plugins":["plugA"]}`); rec.Code != http.StatusOK {
		t.Fatalf("携带 plugins 建会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cap.lastReq.PinnedPlugins) != 1 || cap.lastReq.PinnedPlugins[0] != "plugA" {
		t.Fatalf("plugins 应透传, got %v", cap.lastReq.PinnedPlugins)
	}

	// 2) 缺省 plugins：200 且为 nil（不指定）。
	if rec := do(`{"goal":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("缺省 plugins status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.PinnedPlugins != nil {
		t.Fatalf("缺省 plugins 应为 nil, got %v", cap.lastReq.PinnedPlugins)
	}
}
