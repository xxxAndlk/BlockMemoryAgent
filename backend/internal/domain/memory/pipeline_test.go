package memory

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestPipeline_WriteAndAssemble(t *testing.T) {
	pipe := NewPipeline(nil)
	if err := pipe.Write("agent-1", agent.MemoryEvent{Type: "tool_call", AgentID: "agent-1", ToolName: "ReadFile", Output: "hello"}); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	history := []agent.ReactMessage{{Role: "user", Content: "read the file"}}
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)

	if len(out) != len(history)+1 {
		t.Fatalf("expected %d messages, got %d", len(history)+1, len(out))
	}
	if out[0].Role != "system" {
		t.Fatalf("expected injected system message, got %s", out[0].Role)
	}
	if out[0].Content == "" {
		t.Fatal("expected non-empty context injection")
	}
}

func TestPipeline_Assemble_NoEvents(t *testing.T) {
	pipe := NewPipeline(nil)
	history := []agent.ReactMessage{{Role: "user", Content: "hi"}}
	out := pipe.Assemble(types.RoleDefinition{}, "agent-1", history)
	if len(out) != len(history) {
		t.Fatalf("expected history unchanged, got %d messages", len(out))
	}
}

func TestInMemoryStore(t *testing.T) {
	store := NewInMemoryStore()
	ctx := t.Context()
	_ = store.SaveEvent(ctx, "a", agent.MemoryEvent{Type: "answer", Content: "one"})
	_ = store.SaveEvent(ctx, "a", agent.MemoryEvent{Type: "answer", Content: "two"})
	_ = store.SaveEvent(ctx, "b", agent.MemoryEvent{Type: "answer", Content: "other"})

	events, err := store.LoadEvents(ctx, "a", 10)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Content != "two" {
		t.Fatalf("expected latest event 'two', got %q", events[1].Content)
	}
}
