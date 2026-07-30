//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/blockmemory/agent/backend/test/fixtures"
)

func TestMemorySearchEndpoint(t *testing.T) {
	f := fixtures.NewIntegrationFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	emb, err := f.Server.Deps.Embedder.Embed(ctx, "integration test goal")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	rec := &types.KnowledgeRecord{
		KnowledgeType: enums.KnowledgeTypeBlockMemory,
		Content:       "A fact about testing.",
		Embedding:     emb,
		Meta: map[string]any{
			"goal":       "integration test goal",
			"domain":     "test_domain",
			"session_id": "session-1",
			"source":     "integration_test",
		},
		CreatedAt: time.Now(),
	}
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

	resp2, err := http.Post(f.Server.URL()+"/api/memory/search", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/memory/search: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("memory search unexpected status: %d", resp2.StatusCode)
	}
}
