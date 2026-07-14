package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/blockmemory/agent/backend/internal/agent"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// mockAgentForServer is a minimal agent.Agent implementation for testing the
// HTTP adapter layer without wiring a real ReAct engine or graph.
type mockAgentForServer struct {
	mu       sync.RWMutex
	sessions map[string]*agent.Session
	seq      int
}

func newMockAgentForServer() *mockAgentForServer {
	return &mockAgentForServer{sessions: make(map[string]*agent.Session)}
}

func (m *mockAgentForServer) CreateSession(ctx context.Context, req agent.CreateRequest) (*agent.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("session-%d", m.seq)
	s := &agent.Session{
		ID:        id,
		Goal:      req.Goal,
		Status:    string(enums.SessionStatusCompleted),
		Result:    "done",
		StartedAt: time.Now(),
		Messages: []agent.Message{
			{Role: string(enums.ChatRoleUser), Content: req.Goal, Timestamp: time.Now()},
		},
	}
	m.sessions[id] = s
	return s, nil
}

func (m *mockAgentForServer) ResumeSession(ctx context.Context, sessionID string, req agent.ResumeRequest) (*agent.Session, error) {
	return m.Get(ctx, sessionID)
}

func (m *mockAgentForServer) Send(ctx context.Context, sessionID string, msg agent.Message) error {
	return nil
}

func (m *mockAgentForServer) Stream(ctx context.Context, sessionID string) (<-chan agent.Event, error) {
	return nil, nil
}

func (m *mockAgentForServer) Query(ctx context.Context, sessionID string, q agent.Query) (agent.Result, error) {
	return agent.Result{}, nil
}

func (m *mockAgentForServer) Control(ctx context.Context, sessionID string, cmd agent.ControlCommand) error {
	return nil
}

func (m *mockAgentForServer) List(ctx context.Context, filter agent.Filter) ([]*agent.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*agent.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if filter.Status != "" && s.Status != filter.Status {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *mockAgentForServer) Get(ctx context.Context, sessionID string) (*agent.Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, agent.ErrSessionNotFound
	}
	return s, nil
}

func (m *mockAgentForServer) ListAgents(ctx context.Context, sessionID string) ([]agent.AgentInstance, error) {
	return nil, nil
}

func (m *mockAgentForServer) Shutdown(ctx context.Context) error { return nil }

func (m *mockAgentForServer) SummarizeTaskTitle(ctx context.Context, title string) string {
	return title
}

func newTestAgent(t *testing.T) agent.Agent {
	t.Helper()
	return newMockAgentForServer()
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
