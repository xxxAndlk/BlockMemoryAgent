package agent

import "context"

// Agent is the facade for Agent orchestration.
// HTTP and TUI consumers should depend on this interface instead of reaching
// directly into internal/graph or internal/runtime.
type Agent interface {
	CreateSession(ctx context.Context, req CreateRequest) (*Session, error)
	ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error)
	Send(ctx context.Context, sessionID string, msg Message) error
	Stream(ctx context.Context, sessionID string) (<-chan Event, error)
	Query(ctx context.Context, sessionID string, q Query) (Result, error)
	Control(ctx context.Context, sessionID string, cmd ControlCommand) error
	List(ctx context.Context, filter Filter) ([]*Session, error)
	Get(ctx context.Context, sessionID string) (*Session, error)
	ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error)
	Shutdown(ctx context.Context) error
}
