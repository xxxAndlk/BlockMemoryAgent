package server

// external_agents_test.go 覆盖管理 API 契约：GET agents 数组含 installed 字段、
// reload 返回 {status,agents}、错误 400 纯文本、未接线 503。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
)

// TestExternalAgentsAPI_List 契约：{agents:[...]}，每项含 installed（exec.LookPath 结果）。
func TestExternalAgentsAPI_List(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(nil)
	h.SetExternalAgentProvider(
		func() []tool.ExternalAgentDef {
			return []tool.ExternalAgentDef{{
				ID: "ghost", Command: "bma-no-such-cli-xyz", Args: []string{"{{task}}"},
				TimeoutSec: 1800, Enabled: true, Description: "d",
			}}
		},
		nil,
	)
	r := gin.New()
	r.GET("/api/external-agents", h.ListExternalAgentsHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/external-agents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var body struct {
		Agents []struct {
			ID          string   `json:"id"`
			Command     string   `json:"command"`
			Args        []string `json:"args"`
			TimeoutSec  int      `json:"timeout_sec"`
			Enabled     bool     `json:"enabled"`
			Description string   `json:"description"`
			Installed   bool     `json:"installed"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Agents) != 1 || body.Agents[0].ID != "ghost" || !body.Agents[0].Enabled {
		t.Fatalf("unexpected agents: %s", w.Body.String())
	}
	if body.Agents[0].Installed {
		t.Fatal("ghost cli should not be installed")
	}
	if body.Agents[0].TimeoutSec != 1800 {
		t.Fatalf("timeout_sec should round-trip, got %d", body.Agents[0].TimeoutSec)
	}
}

// TestExternalAgentsAPI_Reload reload 返回 {status,agents}；错误闭包 400 纯文本；未接线 503。
func TestExternalAgentsAPI_Reload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAPIHandler(nil)
	calls := 0
	h.SetExternalAgentProvider(nil, func() ([]tool.ExternalAgentDef, error) {
		calls++
		return []tool.ExternalAgentDef{{ID: "fresh", Command: "bma-no-such-cli-xyz", TimeoutSec: 1800, Enabled: true}}, nil
	})
	r := gin.New()
	r.POST("/api/external-agents/reload", h.ReloadExternalAgentsHandler)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/external-agents/reload", nil))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status %d calls %d body %s", w.Code, calls, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) || !strings.Contains(w.Body.String(), "fresh") {
		t.Fatalf("unexpected body %s", w.Body.String())
	}

	// 错误路径：400 纯文本。
	h2 := NewAPIHandler(nil)
	h2.SetExternalAgentProvider(nil, func() ([]tool.ExternalAgentDef, error) { return nil, errors.New("yaml broken") })
	r2 := gin.New()
	r2.POST("/api/external-agents/reload", h2.ReloadExternalAgentsHandler)
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, httptest.NewRequest(http.MethodPost, "/api/external-agents/reload", nil))
	if w2.Code != http.StatusBadRequest || !strings.Contains(w2.Body.String(), "yaml broken") {
		t.Fatalf("expected 400 with error text, got %d %q", w2.Code, w2.Body.String())
	}

	// 未接线：503。
	h3 := NewAPIHandler(nil)
	r3 := gin.New()
	r3.GET("/api/external-agents", h3.ListExternalAgentsHandler)
	r3.POST("/api/external-agents/reload", h3.ReloadExternalAgentsHandler)
	for _, path := range []string{"/api/external-agents", "/api/external-agents/reload"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/reload") {
			method = http.MethodPost
		}
		w3 := httptest.NewRecorder()
		r3.ServeHTTP(w3, httptest.NewRequest(method, path, nil))
		if w3.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s expected 503, got %d", path, w3.Code)
		}
	}
}
