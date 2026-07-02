//go:build integration

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestSessionCRUD(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// Create a session.
	createBody, _ := json.Marshal(map[string]string{"goal": "write a hello world program"})
	resp, err := http.Post(f.Server.URL()+"/api/sessions", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /api/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create session unexpected status: %d", resp.StatusCode)
	}

	var sess map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	sessionID, ok := sess["id"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("session id missing: %+v", sess)
	}

	// List sessions.
	resp, err = http.Get(f.Server.URL() + "/api/sessions")
	if err != nil {
		t.Fatalf("GET /api/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list sessions unexpected status: %d", resp.StatusCode)
	}

	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode sessions list: %v", err)
	}
	found := false
	for _, s := range list {
		if s["id"] == sessionID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("created session %s not in list", sessionID)
	}

	// Get session detail.
	resp, err = http.Get(f.Server.URL() + "/api/sessions/" + sessionID)
	if err != nil {
		t.Fatalf("GET /api/sessions/%s: %v", sessionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get session unexpected status: %d", resp.StatusCode)
	}

	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode session detail: %v", err)
	}
	if got["id"] != sessionID {
		t.Errorf("session id mismatch: %v", got["id"])
	}
}
