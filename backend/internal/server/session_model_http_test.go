package server

// session_model_http_test.go 验证 POST /api/sessions/:id/model（2026-10-19 会话级模型覆盖）：
// 条目 ID 200 回显并经 Control 下发；空串合法（恢复跟随全局）；会话不存在 404。
// 条目存在性校验在 agent 层（SetSessionModel 查注册表），handler 只验请求体形态。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"

	"github.com/gin-gonic/gin"
)

// TestHandleSessionModel 验证会话模型切换端点三态。
func TestHandleSessionModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &captureControlAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions/:id/model", mgr.HandleSessionModel)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/model", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1) 合法条目 ID：200 回显 + Control 入参正确。
	rec := do(`{"model_id":"ark-glm-5.3-flash"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法 model_id status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
		ModelID   string `json:"model_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.SessionID != "s1" || resp.ModelID != "ark-glm-5.3-flash" {
		t.Fatalf("回显不符: %+v", resp)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 || cap.ops[0].Op != agent.ControlOpModel || cap.ops[0].Args["model_id"] != "ark-glm-5.3-flash" {
		t.Fatalf("Control 入参不符: %+v", cap.ops)
	}
	cap.mu.Unlock()

	// 2) 空串合法（恢复跟随全局）。
	if rec := do(`{"model_id":""}`); rec.Code != http.StatusOK {
		t.Fatalf("空 model_id 应 200, got %d", rec.Code)
	}

	// 3) 会话不存在：404（ErrSessionNotFound 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = agent.ErrSessionNotFound
	cap.mu.Unlock()
	if rec := do(`{"model_id":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleCreateSessionModel 验证建会话携带会话级模型覆盖（2026-10-19）：
// model_id 透传 CreateRequest.Model；缺省为空（跟随全局）。
func TestHandleCreateSessionModel(t *testing.T) {
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

	// 1) 携带 model_id：200 且透传。
	if rec := do(`{"goal":"x","model_id":"ark-glm-5.3-flash"}`); rec.Code != http.StatusOK {
		t.Fatalf("携带 model_id 建会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.Model != "ark-glm-5.3-flash" {
		t.Fatalf("model 应透传, got %q", cap.lastReq.Model)
	}

	// 2) 缺省：200 且为空（跟随全局）。
	if rec := do(`{"goal":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("缺省 model_id status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.Model != "" {
		t.Fatalf("缺省 model_id 应为空串, got %q", cap.lastReq.Model)
	}
}
