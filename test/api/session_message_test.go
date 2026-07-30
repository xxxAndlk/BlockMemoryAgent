//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSessionMessage(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// Register a deterministic response so the mock LLM can route/answer.
	f.LLM.RegisterResponseBySubstring("hello", fixtures.MockResponse{
		Content: "Hello from the mock LLM.",
	})

	// Create a session.
	createBody, _ := json.Marshal(map[string]string{"goal": "say hello"})
	resp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	defer resp.Body.Close()

	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sessionID := sess["id"].(string)

	// Send a follow-up message（handler 期望 content 字段）。
	msgBody, _ := json.Marshal(map[string]string{"content": "hello again"})
	resp, err = http.Post(f.Server.URL()+"/api/sessions/"+sessionID+"/message", "application/json", bytes.NewReader(msgBody))
	if err != nil {
		t.Fatalf("POST message: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("message unexpected status: %d", resp.StatusCode)
	}

	// Give the graph a moment to process.
	time.Sleep(200 * time.Millisecond)

	// Verify the mock LLM received at least one request.
	if len(f.LLM.Requests()) == 0 {
		t.Errorf("mock LLM did not receive any requests")
	}
}
