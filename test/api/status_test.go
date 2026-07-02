//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestStatusEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	resp, err := http.Get(f.Server.URL() + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	if body["program"] != "BlockMemoryAgent" {
		t.Errorf("unexpected program: %v", body["program"])
	}
	if body["mode"] != "multi-agent" {
		t.Errorf("unexpected mode: %v", body["mode"])
	}
}
