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

func TestSessionLifecycle(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.LLM.RegisterResponseBySubstring("long task", fixtures.MockResponse{
		Content: "acknowledged",
	})

	createBody, _ := json.Marshal(map[string]string{"goal": "run a long task"})
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

	// Clarify endpoint should exist and return a deterministic response when no
	// clarification is pending. This test only verifies the HTTP contract.
	resp, err = http.Post(f.Server.URL()+"/api/sessions/"+sessionID+"/clarify", "application/json", bytes.NewReader([]byte(`{"answer":"yes"}`)))
	if err != nil {
		t.Fatalf("POST clarify: %v", err)
	}
	resp.Body.Close()
	// Status may be 4xx if no clarification is pending; we just ensure it does not 5xx.
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("clarify returned server error: %d", resp.StatusCode)
	}

	// Interrupt endpoint.
	resp, err = http.Post(f.Server.URL()+"/api/sessions/"+sessionID+"/interrupt", "application/json", bytes.NewReader([]byte(`{"goal":"stop and do this instead"}`)))
	if err != nil {
		t.Fatalf("POST interrupt: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("interrupt returned server error: %d", resp.StatusCode)
	}

	// Enqueue endpoint.
	resp, err = http.Post(f.Server.URL()+"/api/sessions/"+sessionID+"/enqueue", "application/json", bytes.NewReader([]byte(`{"message":"queued instruction"}`)))
	if err != nil {
		t.Fatalf("POST enqueue: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("enqueue returned server error: %d", resp.StatusCode)
	}

	// Cancel endpoint.
	req, _ := http.NewRequest(http.MethodPost, f.Server.URL()+"/api/sessions/"+sessionID+"/cancel", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("cancel returned server error: %d", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)
}
