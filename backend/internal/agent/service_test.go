package agent

import (
	"context"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/graph"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/types"
)

type fakeMetaAgentForService struct{}

func (n *fakeMetaAgentForService) Name() string { return "MetaAgent" }
func (n *fakeMetaAgentForService) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.SessionSummary = "done"
	state.NextAction = enums.ActionFinish
	return state, nil
}

type fakeSinkerForService struct{}

func (n *fakeSinkerForService) Name() string { return "Sinker" }
func (n *fakeSinkerForService) Invoke(ctx context.Context, state *types.ThreeLayerState) (*types.ThreeLayerState, error) {
	state.NextAction = enums.ActionFinish
	return state, nil
}

func newTestService(t *testing.T) *Service {
	t.Helper()

	cfg := &pkgconfig.RoleConfigFile{}
	registry := graph.NewRoleRegistry(cfg)
	factory := graph.NewRoleFactory(registry, nil, cfg)
	builder := graph.NewThreeLayerGraphBuilder(registry, factory)
	builder.AddNode(&fakeMetaAgentForService{})
	builder.AddNode(graph.NewEscalationHandlerNode())
	builder.AddNode(&fakeSinkerForService{})
	g := builder.Build()

	return NewService(g, registry, nil)
}

func TestServiceCreateAndGetSession(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "test goal"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected session ID")
	}
	if created.Goal != "test goal" {
		t.Errorf("Goal = %q, want %q", created.Goal, "test goal")
	}
	if created.Status != string(enums.SessionStatusRunning) {
		t.Errorf("Status = %q, want %q", created.Status, string(enums.SessionStatusRunning))
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("Get ID = %q, want %q", got.ID, created.ID)
	}
	if got.Goal != "test goal" {
		t.Errorf("Get Goal = %q, want %q", got.Goal, "test goal")
	}
}

func TestServiceGetNotFound(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.Get(context.Background(), "session-does-not-exist")
	if err != ErrSessionNotFound {
		t.Errorf("Get error = %v, want ErrSessionNotFound", err)
	}
}

func TestServiceListSessions(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateSession(ctx, CreateRequest{Goal: "first"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	_, err = svc.CreateSession(ctx, CreateRequest{Goal: "second"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	all, err := svc.List(ctx, Filter{})
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("len(List) = %d, want 2", len(all))
	}

	limited, err := svc.List(ctx, Filter{Limit: 1})
	if err != nil {
		t.Fatalf("List with limit error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("len(List with limit) = %d, want 1", len(limited))
	}
}

func TestServiceListAgentsEmpty(t *testing.T) {
	svc := newTestService(t)
	agents, err := svc.ListAgents(context.Background(), "session-no-instances")
	if err != nil {
		t.Fatalf("ListAgents error: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("len(ListAgents) = %d, want 0", len(agents))
	}
}

func TestServiceShutdown(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateSession(ctx, CreateRequest{Goal: "shutdown test"})
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}

	if err := svc.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}

	// After shutdown the running session should eventually enter a terminal
	// state because its context was cancelled.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := svc.Get(ctx, created.ID)
		if snap != nil && snap.Status != string(enums.SessionStatusRunning) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("session did not leave running state after Shutdown")
}
