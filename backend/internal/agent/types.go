package agent

import "time"

// CreateRequest starts a new session.
type CreateRequest struct {
	Goal string
	Meta map[string]any
}

// ResumeRequest continues a previously paused or finished session.
type ResumeRequest struct {
	CarryOver string
	UserInput string
}

// Message is a single chat message exchanged within a session.
type Message struct {
	Role    string
	Content string
}

// Query asks a read-only question about a session.
type Query struct {
	Kind string
	Args map[string]any
}

// Result carries the answer to a Query.
type Result struct {
	Data any
}

// ControlCommand sends an operational command to a session.
type ControlCommand struct {
	Op   string
	Args map[string]any
}

// Filter selects sessions when listing.
type Filter struct {
	Status string
	Limit  int
}

// Session mirrors server.Session field-by-field using plain DTO types so the
// agent package stays decoupled from internal/server and backend/pkg/types.
type Session struct {
	ID        string
	Goal      string
	Status    string
	Result    string
	State     string
	StartedAt time.Time
	EndedAt   time.Time
	Events    []Event
	Messages  []Message
	TempDir   string
}

// Event mirrors server.SessionEvent field-by-field.
type Event struct {
	Type         string
	Agent        string
	Message      string
	Kind         string
	Tool         string
	ToolPath     string
	ToolOutput   string
	ToolError    string
	Success      bool
	Timestamp    time.Time
	Prompt       string
	InputTokens  int
	OutputTokens int
	DetailJSON   string
}

// AgentInstance is a lightweight runtime view of an instantiated agent.
type AgentInstance struct {
	Name     string
	Role     string
	ModuleID string
	Status   string
}
