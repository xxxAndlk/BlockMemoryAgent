package server

// session_gear_http_test.go 验证 POST /api/sessions/:id/gear（TODO #14 会话三档控制）：
// 合法枚举 200 回显并经 Control 下发；非法枚举 400（不到 agent 层）；会话不存在 404。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"

	"github.com/gin-gonic/gin"
)

// TestHandleSessionGear 验证档位切换端点三态。
func TestHandleSessionGear(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &captureControlAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions/:id/gear", mgr.HandleSessionGear)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/gear", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1) 合法档位：200 回显 + Control 入参正确。
	rec := do(`{"gear":"fast"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法档位 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
		Gear      string `json:"gear"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.SessionID != "s1" || resp.Gear != "fast" {
		t.Fatalf("回显不符: %+v", resp)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 || cap.ops[0].Op != agent.ControlOpGear || cap.ops[0].Args["gear"] != "fast" {
		t.Fatalf("Control 入参不符: %+v", cap.ops)
	}
	cap.mu.Unlock()

	// 2) 非法枚举：400（不到 agent 层）。
	if rec := do(`{"gear":"warp"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法枚举应 400, got %d", rec.Code)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 {
		t.Fatalf("非法枚举不应触达 agent 层, ops=%+v", cap.ops)
	}
	cap.mu.Unlock()

	// 3) 会话不存在：404（ErrSessionNotFound 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = agent.ErrSessionNotFound
	cap.mu.Unlock()
	if rec := do(`{"gear":"cluster"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
