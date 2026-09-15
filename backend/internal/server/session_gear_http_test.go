package server

// session_gear_http_test.go 验证 POST /api/sessions/:id/gear（TODO #14 会话三档控制）：
// 合法枚举 200 回显并经 Control 下发；非法枚举 400（不到 agent 层）；会话不存在 404。

import (
	"context"
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

// captureCreateAgent 记录 CreateSession 入参（验证 gear 透传）。
type captureCreateAgent struct {
	mockAgentForServer
	lastReq agent.CreateRequest
}

func (c *captureCreateAgent) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) {
	c.lastReq = req
	return c.mockAgentForServer.CreateSession(ctx, req)
}

// TestHandleCreateSessionGear 验证建会话携带初始档位（TODO #14 新会话页选档）：
// 合法枚举透传 CreateRequest.Gear；非法枚举 400（不到 agent 层）；缺省为空回落默认。
func TestHandleCreateSessionGear(t *testing.T) {
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

	// 1) 合法 gear：200 且透传 CreateRequest。
	if rec := do(`{"goal":"x","gear":"fast"}`); rec.Code != http.StatusOK {
		t.Fatalf("合法 gear 建会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.Gear != "fast" {
		t.Fatalf("gear 应透传 fast, got %q", cap.lastReq.Gear)
	}

	// 2) 非法 gear：400。
	if rec := do(`{"goal":"x","gear":"warp"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 gear 应 400, got %d", rec.Code)
	}

	// 3) 缺省 gear：200 且为空（回落默认档）。
	if rec := do(`{"goal":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("缺省 gear status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.Gear != "" {
		t.Fatalf("缺省 gear 应为空串, got %q", cap.lastReq.Gear)
	}
}
