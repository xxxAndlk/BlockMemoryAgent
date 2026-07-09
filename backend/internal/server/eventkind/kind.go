// Package eventkind defines the event type and kind constants used across the
// server package for SessionEvent.Type and SessionEvent.Kind.
package eventkind

const (
	Think        = "think"
	Prompt       = "prompt"
	TokenUsage   = "token_usage"
	GraphStep    = "graph_step"
	ToolExec     = "tool_exec"
	ToolCall     = "tool_call"
	MemoryRecall = "memory_recall"
	TopicSwitch  = "topic_switch"
	LLMResult    = "llm_result"
	AgentDone    = "agent_done"
	System       = "system"
	Error        = "error"
	Intend       = "intend"
	ToolResult   = "tool_result"
	Wait         = "wait"
	AgentCreated = "agent_created"

	Progress    = "progress"
	Clarify     = "clarify"
	Stats       = "stats"
	LLM         = "llm"
	UserMessage = "user_message"
	Message     = "message"
	Interrupt   = "interrupt"
	Enqueue     = "enqueue"
)
