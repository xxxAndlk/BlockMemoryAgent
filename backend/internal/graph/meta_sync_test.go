package graph

import (
	"testing"

	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/pkg/types"
)

func TestMergeSessionMetaMemory(t *testing.T) {
	state := &types.ThreeLayerState{
		MetaMemory: []types.MetaMemoryEntry{
			{Content: "decision A"},
			{Content: "decision B"},
		},
	}
	block := &types.SessionBlock{
		MetaMemory: []types.MetaMemoryEntry{
			{Content: "decision A"},
		},
	}
	mergeSessionMetaMemory(state, block)
	if len(block.MetaMemory) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(block.MetaMemory))
	}
}

func TestNotifyOtherActiveBlocks(t *testing.T) {
	mb := mailbox.New()
	rt := &runtime.Runtime{Mailbox: mb}
	state := &types.ThreeLayerState{
		ActiveBlocks: map[string]*types.SessionBlock{
			"b1": {ID: "b1", Domain: "db", Agents: []string{"agent-db"}},
			"b2": {ID: "b2", Domain: "ui", Agents: []string{"agent-ui"}},
		},
	}
	completed := &types.SessionBlock{
		ID:     "b1",
		Domain: "db",
		Agents: []string{"agent-db"},
		Result: &types.AgentResult{
			SummaryForUser: "db done",
			Facts:          []string{"user table added"},
		},
	}
	notifyOtherActiveBlocks(state, completed, rt)

	msgs := mb.Drain("agent-ui")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message to ui agent, got %d", len(msgs))
	}
	if msgs[0].Type != mailbox.MsgInfo {
		t.Fatalf("expected MsgInfo, got %v", msgs[0].Type)
	}
}

func TestApplyMailboxToBlock(t *testing.T) {
	mb := mailbox.New()
	mb.Send(&mailbox.Message{
		From:    "agent-db",
		To:      "agent-ui",
		Type:    mailbox.MsgInfo,
		Subject: "db done",
		Body:    "user table added",
	})
	rt := &runtime.Runtime{Mailbox: mb}
	state := &types.ThreeLayerState{}
	block := &types.SessionBlock{}
	applyMailboxToBlock(state, block, rt, "agent-ui")
	if len(block.MetaMemory) != 1 {
		t.Fatalf("expected 1 meta memory entry, got %d", len(block.MetaMemory))
	}
}
