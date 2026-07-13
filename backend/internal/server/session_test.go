package server

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/internal/graph"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

type fakeMetaAgentForAdapter struct{}

func (n *fakeMetaAgentForAdapter) Name() string { return "MetaAgent" }
func (n *fakeMetaAgentForAdapter) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.SessionSummary = "done"
	state.NextAction = enums.ActionFinish
	return state, nil
}

type fakeSinkerForAdapter struct{}

func (n *fakeSinkerForAdapter) Name() string { return "Sinker" }
func (n *fakeSinkerForAdapter) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}

func newTestAgent(t *testing.T) agent.Agent {
	t.Helper()
	cfg := &pkgconfig.RoleConfigFile{}
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(&fakeMetaAgentForAdapter{})
	builder.AddNode(graph.NewEscalationHandlerNode())
	builder.AddNode(&fakeSinkerForAdapter{})
	g := builder.Build()
	return agent.NewService(g, registry, nil)
}

func TestSessionManagerDelegatesCreateAndGet(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	session, err := mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "adapter test"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	got := mgr.GetSession(session.ID)
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.ID != session.ID {
		t.Errorf("GetSession ID = %q, want %q", got.ID, session.ID)
	}
	if got.Goal != "adapter test" {
		t.Errorf("GetSession Goal = %q, want %q", got.Goal, "adapter test")
	}
}

func TestSessionManagerListSessions(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	_, _ = mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "first"})
	_, _ = mgr.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: "second"})

	sessions := mgr.ListSessions()
	if len(sessions) != 2 {
		t.Errorf("ListSessions len = %d, want 2", len(sessions))
	}
}

func TestSessionManagerLaunchSession(t *testing.T) {
	agentFacade := newTestAgent(t)
	mgr := NewSessionManager(agentFacade)

	id := mgr.LaunchSession("launch test")
	if id == "" {
		t.Fatal("LaunchSession returned empty id")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := mgr.GetSession(id)
		if s != nil && s.Status != enums.SessionStatusRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("LaunchSession did not complete")
}
