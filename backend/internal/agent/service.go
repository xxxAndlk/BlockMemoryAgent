package agent

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/internal/store"
)

// Service is a thin Agent facade that wraps server.SessionManager.
//
// It implements the Agent interface so HTTP/TUI consumers can depend on a
// stable abstraction while the heavy SessionManager logic is gradually moved
// here in later refactoring tasks.
type Service struct {
	sessions *server.SessionManager
	registry *graph.RoleRegistry
	runtime  *runtime.Runtime
	graph    *graph.ThreeLayerGraph
}

// NewService creates an Agent Service. The supplied graph and registry are
// forwarded to a private server.SessionManager.
func NewService(g *graph.ThreeLayerGraph, registry *graph.RoleRegistry, rt *runtime.Runtime, opts ...ServiceOption) *Service {
	s := &Service{
		sessions: server.NewSessionManager(g, registry),
		registry: registry,
		runtime:  rt,
		graph:    g,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ServiceOption configures Service after construction.
type ServiceOption func(*Service)

// WithPostgresStore injects a Postgres store into the wrapped SessionManager.
func WithPostgresStore(pg *store.PostgresStore) ServiceOption {
	return func(s *Service) {
		s.sessions.SetPostgresStore(pg)
	}
}

// WithModelFactory injects a ModelFactory into the wrapped SessionManager.
func WithModelFactory(mf *model.ModelFactory) ServiceOption {
	return func(s *Service) {
		s.sessions.SetModelFactory(mf)
	}
}

// CreateSession starts a new session for the given goal.
func (s *Service) CreateSession(ctx context.Context, req CreateRequest) (*Session, error) {
	sess := s.sessions.CreateSession(ctx, req.Goal)
	return toAgentSession(sess), nil
}

// Get returns a session by ID.
func (s *Service) Get(ctx context.Context, sessionID string) (*Session, error) {
	sess := s.sessions.SnapshotSession(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	return toAgentSession(sess), nil
}

// List returns sessions matching the supplied filter.
func (s *Service) List(ctx context.Context, filter Filter) ([]*Session, error) {
	all := s.sessions.ListSessions()

	if filter.Status != "" {
		filtered := make([]*server.Session, 0, len(all))
		for _, sess := range all {
			if string(sess.Status) == filter.Status {
				filtered = append(filtered, sess)
			}
		}
		all = filtered
	}

	if filter.Limit > 0 && filter.Limit < len(all) {
		all = all[:filter.Limit]
	}

	out := make([]*Session, 0, len(all))
	for _, sess := range all {
		out = append(out, toAgentSession(sess))
	}
	return out, nil
}

// Send delivers a message to a session. Complex routing through the wrapped
// SessionManager is left unimplemented in this incremental step.
func (s *Service) Send(ctx context.Context, sessionID string, msg Message) error {
	return fmt.Errorf("not implemented")
}

// Stream returns a real-time event channel for the session. A proper
// implementation requires significant refactoring, so it is left for a later
// task.
func (s *Service) Stream(ctx context.Context, sessionID string) (<-chan Event, error) {
	return nil, fmt.Errorf("not implemented")
}

// ResumeSession continues a previously finished or paused session.
func (s *Service) ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error) {
	return nil, fmt.Errorf("not implemented")
}

// Query answers a read-only question about a session.
func (s *Service) Query(ctx context.Context, sessionID string, q Query) (Result, error) {
	return Result{}, fmt.Errorf("not implemented")
}

// Control sends an operational command to a session.
func (s *Service) Control(ctx context.Context, sessionID string, cmd ControlCommand) error {
	return fmt.Errorf("not implemented")
}

// ListAgents returns the runtime agent instances associated with a session.
func (s *Service) ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error) {
	insts := s.registry.GetInstancesBySession(sessionID)
	out := make([]AgentInstance, 0, len(insts))
	for _, inst := range insts {
		ai := toAgentInstance(inst)
		if def := s.registry.GetRoleDef(inst.RoleDefID); def != nil && def.Name != "" {
			ai.Name = def.Name
		}
		out = append(out, *ai)
	}
	return out, nil
}

// Shutdown cancels all running sessions managed by the wrapped SessionManager.
func (s *Service) Shutdown(ctx context.Context) error {
	s.sessions.Shutdown()
	return nil
}
