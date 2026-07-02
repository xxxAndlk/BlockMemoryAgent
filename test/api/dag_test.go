//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestDAGEndpoints(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// The DAG scheduler is disabled by default. These tests verify that the
	// endpoints are wired and return a non-5xx status.
	resp, err := http.Get(f.Server.URL() + "/api/dag")
	if err != nil {
		t.Fatalf("GET /api/dag: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		t.Errorf("GET /api/dag returned server error: %d", resp.StatusCode)
	}
}
