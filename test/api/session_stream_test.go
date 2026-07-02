//go:build integration

package api_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSessionStream(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	f.LLM.RegisterResponseBySubstring("ping", fixtures.MockResponse{
		Content: "pong",
	})

	createBody, _ := json.Marshal(map[string]string{"goal": "ping"})
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

	// Connect to the SSE stream with a short timeout.
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err = client.Get(f.Server.URL() + "/api/sessions/" + sessionID + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream unexpected status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("unexpected content type: %s", ct)
	}

	// Read at least one SSE line. The graph may emit token_usage or status events.
	scanner := bufio.NewScanner(resp.Body)
	seen := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			seen = true
			break
		}
	}
	if !seen {
		t.Errorf("no SSE data event received")
	}
}
