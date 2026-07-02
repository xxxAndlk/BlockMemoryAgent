//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestHealthEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	resp, err := http.Get(f.Server.URL() + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}

	for _, key := range []string{"postgres", "redis", "llm"} {
		if _, ok := body[key]; !ok {
			t.Errorf("health response missing %s", key)
		}
	}

	pg, _ := body["postgres"].(map[string]any)
	if pg["online"] != true {
		t.Errorf("postgres not online: %+v", pg)
	}
	redis, _ := body["redis"].(map[string]any)
	if redis["online"] != true {
		t.Errorf("redis not online: %+v", redis)
	}
}
