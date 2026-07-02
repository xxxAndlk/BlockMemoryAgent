//go:build integration

package api_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

// TestSessionErrorPaths 覆盖 API 错误路径（P2-2）：
//   - GET 不存在的会话 → 404
//   - POST /api/sessions 空 body → 4xx（不应 5xx）
//   - 向不存在会话发消息 → 4xx（不应 5xx）
func TestSessionErrorPaths(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// 1. 不存在的会话 ID → 404
	resp, err := http.Get(f.Server.URL() + "/api/sessions/nonexistent-session-id")
	if err != nil {
		t.Fatalf("GET nonexistent session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("nonexistent session: want 404, got %d", resp.StatusCode)
	}

	// 2. 空 body 创建会话 → 4xx（不应 5xx）
	resp, err = http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("POST empty body: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("empty body create: got server error %d (want 4xx)", resp.StatusCode)
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("empty body create: want 4xx, got %d", resp.StatusCode)
	}

	// 3. 向不存在会话发消息 → 4xx（不应 5xx）
	resp, err = http.Post(f.Server.URL()+"/api/sessions/nonexistent-session-id/message",
		"application/json", bytes.NewReader([]byte(`{"content":"hi"}`)))
	if err != nil {
		t.Fatalf("POST message to nonexistent: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("message to nonexistent: got server error %d (want 4xx)", resp.StatusCode)
	}
}
