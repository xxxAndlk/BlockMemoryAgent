package agent

import (
	"time"

	"github.com/blockmemory/agent/backend/internal/server"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// toAgentSession converts a server.Session DTO to an agent.Session DTO.
func toAgentSession(s *server.Session) *Session {
	if s == nil {
		return nil
	}

	state := ""
	var activeBlocks []ActiveBlock
	var pendingClarify *ClarifyRequest
	if s.State != nil {
		if s.State.CurrentDomain != "" {
			state = s.State.CurrentDomain
		} else {
			state = "active"
		}
		if len(s.State.ActiveBlocks) > 0 {
			activeBlocks = make([]ActiveBlock, 0, len(s.State.ActiveBlocks))
			for _, b := range s.State.ActiveBlocks {
				if b == nil {
					continue
				}
				activeBlocks = append(activeBlocks, ActiveBlock{
					ID:     b.ID,
					Domain: b.Domain,
					Goal:   b.Goal,
				})
			}
		}
		if s.State.PendingClarify != nil {
			req := s.State.PendingClarify
			pendingClarify = &ClarifyRequest{
				ID:         req.ID,
				Question:   req.Question,
				Context:    req.Context,
				AgentID:    req.AgentID,
				CreatedAt:  req.CreatedAt,
				Answer:     req.Answer,
				AnsweredAt: req.AnsweredAt,
			}
		}
	}

	var endedAt time.Time
	if s.EndedAt != nil {
		endedAt = *s.EndedAt
	}

	events := make([]Event, 0, len(s.Events))
	for i := range s.Events {
		events = append(events, *toAgentEvent(&s.Events[i]))
	}

	messages := make([]Message, 0, len(s.Messages))
	for _, m := range s.Messages {
		messages = append(messages, Message{
			Role:      string(m.Role),
			Content:   m.Content,
			Timestamp: m.Timestamp,
		})
	}

	return &Session{
		ID:             s.ID,
		Goal:           s.Goal,
		Status:         string(s.Status),
		Result:         s.Result,
		State:          state,
		StartedAt:      s.StartedAt,
		EndedAt:        endedAt,
		Events:         events,
		Messages:       messages,
		TempDir:        s.TempDir,
		ActiveBlocks:   activeBlocks,
		PendingClarify: pendingClarify,
	}
}

// toAgentEvent converts a server.SessionEvent DTO to an agent.Event DTO.
func toAgentEvent(e *server.SessionEvent) *Event {
	if e == nil {
		return nil
	}
	return &Event{
		Type:         e.Type,
		Agent:        e.Agent,
		Message:      e.Message,
		Kind:         e.Kind,
		Tool:         e.Tool,
		ToolPath:     e.ToolPath,
		ToolOutput:   e.ToolOutput,
		ToolError:    e.ToolError,
		Success:      e.Success,
		Timestamp:    e.Timestamp,
		Prompt:       e.Prompt,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
		DetailJSON:   e.DetailJSON,
	}
}

// toAgentInstance converts a runtime RoleInstance to an agent.AgentInstance DTO.
func toAgentInstance(inst *types.RoleInstance) *AgentInstance {
	if inst == nil {
		return nil
	}
	return &AgentInstance{
		Name:      inst.RoleDefID,
		Role:      string(inst.Type),
		ModuleID:  inst.ID,
		Status:    string(inst.Status),
		Domain:    inst.Domain,
		RoleType:  inst.Type,
		Children:  inst.Children,
		CreatedAt: inst.CreatedAt,
		RoleDefID: inst.RoleDefID,
	}
}
