package agent

import (
	"time"

	"github.com/blockmemory/agent/backend/pkg/enums"
)

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
	Role      string
	Content   string
	Timestamp time.Time
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

// QueryKind constants identify read-only questions answered by Query.
const (
	QueryKindBoard        = "board"
	QueryKindMailbox      = "mailbox"
	QueryKindMetrics      = "metrics"
	QueryKindWatchdog     = "watchdog"
	QueryKindTokenMetrics = "token-metrics"
	QueryKindLogs         = "logs"
	QueryKindSessionCount = "session-count"
	QueryKindLLMStats     = "llm-stats"
)

// ControlCommand sends an operational command to a session.
type ControlCommand struct {
	Op   string
	Args map[string]any
}

// Control operations.
const (
	ControlOpMessage   = "message"
	ControlOpClarify   = "clarify"
	ControlOpInterrupt = "interrupt"
	ControlOpEnqueue   = "enqueue"
	ControlOpCancel    = "cancel"
	ControlOpTopic     = "topic"
)

// Filter selects sessions when listing.
type Filter struct {
	Status string
	Limit  int
}

// ActiveBlock is a lightweight view of a running session block.
type ActiveBlock struct {
	ID     string
	Domain string
	Goal   string
}

// ClarifyRequest mirrors types.ClarifyRequest using plain DTO fields.
type ClarifyRequest struct {
	ID         string
	Question   string
	Context    string
	AgentID    string
	CreatedAt  time.Time
	Answer     string
	AnsweredAt *time.Time
}

// Session mirrors server.Session field-by-field using plain DTO types so the
// agent package stays decoupled from internal/server and backend/pkg/types.
type Session struct {
	ID             string
	Goal           string
	Status         string
	Result         string
	State          string
	StartedAt      time.Time
	EndedAt        time.Time
	Events         []Event
	Messages       []Message
	TempDir        string
	ActiveBlocks   []ActiveBlock
	PendingClarify *ClarifyRequest
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
	Name      string
	Role      string
	ModuleID  string
	Status    string
	Domain    string
	RoleType  enums.RoleType
	Children  []string
	CreatedAt time.Time
	RoleDefID string
}
