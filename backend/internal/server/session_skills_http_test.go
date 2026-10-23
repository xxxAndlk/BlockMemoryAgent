package server

// session_skills_http_test.go 验证 POST /api/sessions/:id/skills（技能选择器）：
// 合法列表 200 回显并经 Control 下发；空数组=清空（合法 200）；会话不存在 404；
// 无效技能 400（ErrInvalidSessionState 映射）；建会话透传 CreateRequest.PinnedSkills。

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

// TestHandleSessionSkills 验证指定技能切换端点。
func TestHandleSessionSkills(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cap := &captureControlAgent{mockAgentForServer: *newMockAgentForServer()}
	mgr := NewSessionManager(cap)
	r := gin.New()
	r.POST("/api/sessions/:id/skills", mgr.HandleSessionSkills)

	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/skills", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1) 合法列表：200 回显 + Control 入参正确。
	rec := do(`{"skills":["pdf_extract","读文件"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法列表 status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		SessionID string   `json:"session_id"`
		Skills    []string `json:"skills"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析: %v", err)
	}
	if resp.SessionID != "s1" || len(resp.Skills) != 2 || resp.Skills[0] != "pdf_extract" {
		t.Fatalf("回显不符: %+v", resp)
	}
	cap.mu.Lock()
	if len(cap.ops) != 1 || cap.ops[0].Op != agent.ControlOpPinnedSkills {
		t.Fatalf("Control 入参不符: %+v", cap.ops)
	}
	gotSkills, ok := cap.ops[0].Args["skills"].([]string)
	if !ok || len(gotSkills) != 2 || gotSkills[1] != "读文件" {
		t.Fatalf("skills 参数不符: %+v", cap.ops[0].Args)
	}
	cap.mu.Unlock()

	// 2) 空数组=清空：合法 200 并下发空列表。
	rec = do(`{"skills":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空 status=%d body=%s", rec.Code, rec.Body.String())
	}
	cap.mu.Lock()
	if len(cap.ops) != 2 {
		t.Fatalf("清空应触达 agent 层, ops=%+v", cap.ops)
	}
	if got, ok := cap.ops[1].Args["skills"].([]string); !ok || len(got) != 0 {
		t.Fatalf("清空应下发空列表, got %+v", cap.ops[1].Args)
	}
	cap.mu.Unlock()

	// 3) 会话不存在：404（ErrSessionNotFound 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = agent.ErrSessionNotFound
	cap.mu.Unlock()
	if rec := do(`{"skills":["pdf_extract"]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("会话不存在应 404, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 4) 无效技能：400（ErrInvalidSessionState 映射）。
	cap.mu.Lock()
	cap.errAt = len(cap.ops) + 1
	cap.err = fmt.Errorf("%w: 无效技能（不在技能池）: ghost", agent.ErrInvalidSessionState)
	cap.mu.Unlock()
	if rec := do(`{"skills":["ghost"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("无效技能应 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleCreateSessionSkills 验证建会话携带初始指定技能（技能选择器）。
func TestHandleCreateSessionSkills(t *testing.T) {
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

	// 1) 携带 skills：200 且透传 CreateRequest.PinnedSkills。
	if rec := do(`{"goal":"x","skills":["pdf_extract"]}`); rec.Code != http.StatusOK {
		t.Fatalf("携带 skills 建会话 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(cap.lastReq.PinnedSkills) != 1 || cap.lastReq.PinnedSkills[0] != "pdf_extract" {
		t.Fatalf("skills 应透传, got %v", cap.lastReq.PinnedSkills)
	}

	// 2) 缺省 skills：200 且为 nil（不指定）。
	if rec := do(`{"goal":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("缺省 skills status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.lastReq.PinnedSkills != nil {
		t.Fatalf("缺省 skills 应为 nil, got %v", cap.lastReq.PinnedSkills)
	}
}
