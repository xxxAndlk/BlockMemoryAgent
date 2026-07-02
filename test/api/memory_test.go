//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/memory"
	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestMemorySearchEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	// Seed a block memory record directly through the store adapter.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rec := (&memory.BlockMemoryRecord{
		SessionID: "session-1",
		Domain:    "test_domain",
		Goal:      "integration test goal",
		Summary:   "A fact about testing.",
		Facts: []memory.Fact{
			{Key: "status", Value: "ok", Scope: memory.FactScopeDomain},
		},
		CreatedAt: time.Now(),
	}).ToKnowledgeRecord(768)
	if err := f.Server.Deps.Postgres.SaveKnowledge(ctx, rec); err != nil {
		t.Fatalf("seed knowledge: %v", err)
	}

	resp, err := http.Get(f.Server.URL() + "/api/memory/levels")
	if err != nil {
		t.Fatalf("GET /api/memory/levels: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("memory levels unexpected status: %d", resp.StatusCode)
	}

	resp, err = http.Get(f.Server.URL() + "/api/memory/search?query=testing&limit=5")
	if err != nil {
		t.Fatalf("GET /api/memory/search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("memory search unexpected status: %d", resp.StatusCode)
	}
}
