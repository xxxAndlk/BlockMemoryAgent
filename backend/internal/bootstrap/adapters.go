package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/embed"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/memory"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// pgBlockMemoryAdapter bridges graph.BlockMemoryStore to the memory/store layers.
type pgBlockMemoryAdapter struct {
	pg       *store.PostgresStore
	embedder embed.Embedder
	dim      int
}

func (a *pgBlockMemoryAdapter) SaveBlockMemory(ctx context.Context, sessionID, domain, goal, summary string, facts []graph.BlockMemoryFact) error {
	memFacts := make([]memory.Fact, 0, len(facts))
	for _, f := range facts {
		memFacts = append(memFacts, memory.Fact{Key: f.Key, Value: f.Value, Scope: memory.FactScope(f.Scope)})
	}
	rec, err := (&memory.BlockMemoryRecord{
		SessionID: sessionID,
		Domain:    domain,
		Goal:      goal,
		Summary:   summary,
		Facts:     memFacts,
		CreatedAt: time.Now(),
	}).ToKnowledgeRecord(ctx, a.embedder, a.dim)
	if err != nil {
		return fmt.Errorf("convert block memory: %w", err)
	}
	return a.pg.SaveKnowledge(ctx, rec)
}

func (a *pgBlockMemoryAdapter) SearchBlockMemory(ctx context.Context, domain, query string, topK int) (string, error) {
	recs, err := memory.SearchBlockMemory(ctx, a.pg, domain, query, topK)
	if err != nil {
		return "", err
	}
	if len(recs) == 0 {
		return "", nil
	}
	var b strings.Builder
	for i, r := range recs {
		b.WriteString(fmt.Sprintf("[%d] %s\n", i+1, r.Summary))
		if facts := memory.FormatBlockMemoryFacts(r.Facts); facts != "" {
			b.WriteString("facts:\n")
			b.WriteString(facts)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// pgHistoryAdapter bridges graph.HistoryStore to the Postgres session_history table.
type pgHistoryAdapter struct {
	pg *store.PostgresStore
}

func (a *pgHistoryAdapter) RecentSessionHistories(ctx context.Context, limit int) ([]graph.HistoryEntry, error) {
	recs, err := a.pg.RecentSessionHistories(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]graph.HistoryEntry, 0, len(recs))
	for _, r := range recs {
		out = append(out, graph.HistoryEntry{
			SessionID:   r.SessionID,
			Goal:        r.Goal,
			Summary:     r.Summary,
			ToolResults: r.ToolResults,
			MetaMemory:  r.MetaMemory,
			CreatedAt:   r.CreatedAt,
		})
	}
	return out, nil
}

// globalKBAdapter retrieves global knowledge records for the context assembler.
type globalKBAdapter struct {
	pg       *store.PostgresStore
	embedder embed.Embedder
}

func (a *globalKBAdapter) Retrieve(ctx context.Context, query string, topK int) ([]*types.KnowledgeRecord, error) {
	emb, err := a.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return a.pg.SearchKnowledge(ctx, emb, topK)
}
