package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/cmdqueue"
	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/runtime"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// Service is the core Agent facade that owns session lifecycle.
//
// It implements the Agent interface so HTTP/TUI consumers can depend on a
// stable abstraction. The previous wrapper around server.SessionManager has been
// inverted: server.SessionManager is now a thin HTTP adapter that delegates to
// this Service.
type Service struct {
	store    *sessionStore
	registry *graph.RoleRegistry
}

// NewService creates an Agent Service that owns the in-memory session store.
// Runtime dependencies are injected through the graph builder; the rt argument
// is retained for backward compatibility with existing call sites.
func NewService(g *graph.ThreeLayerGraph, registry *graph.RoleRegistry, rt *runtime.Runtime, opts ...ServiceOption) *Service {
	s := &Service{
		store:    newSessionStore(g, registry),
		registry: registry,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ServiceOption configures Service after construction.
type ServiceOption func(*Service)

// WithPostgresStore injects a Postgres store for session persistence.
func WithPostgresStore(pg *store.PostgresStore) ServiceOption {
	return func(s *Service) {
		s.store.setPostgresStore(pg)
	}
}

// WithModelFactory injects a ModelFactory for lightweight history summarization
// when resuming sessions.
func WithModelFactory(mf *model.ModelFactory) ServiceOption {
	return func(s *Service) {
		s.store.setModelFactory(mf)
	}
}

// CreateSession starts a new session for the given goal.
func (s *Service) CreateSession(ctx context.Context, req CreateRequest) (*Session, error) {
	sess := s.store.createSession(ctx, req.Goal)
	return toAgentSession(sess), nil
}

// Get returns a session by ID.
func (s *Service) Get(ctx context.Context, sessionID string) (*Session, error) {
	// First try the in-memory store.
	sess := s.store.snapshotSessionByID(sessionID)
	if sess != nil {
		return toAgentSession(sess), nil
	}
	// Fall back to Postgres history when the session has been evicted from memory.
	if s.store.pgStore != nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		rec, err := s.store.pgStore.GetSessionHistoryByID(ctx, sessionID)
		if err == nil && rec != nil {
			endedAt := rec.CreatedAt
			return &Session{
				ID:        rec.SessionID,
				Goal:      rec.Goal,
				Status:    string(enums.SessionStatusCompleted),
				Result:    rec.Summary,
				StartedAt: rec.CreatedAt,
				EndedAt:   endedAt,
				Events:    make([]Event, 0),
				Messages: []Message{
					{Role: string(enums.ChatRoleUser), Content: rec.Goal, Timestamp: rec.CreatedAt},
					{Role: string(enums.ChatRoleAssistant), Content: rec.Summary, Timestamp: rec.CreatedAt},
				},
			}, nil
		}
	}
	return nil, ErrSessionNotFound
}

// List returns in-memory sessions matching the supplied filter.
// Historical sessions that have been evicted from memory are not included here;
// they remain reachable via Get (which falls back to Postgres).
func (s *Service) List(ctx context.Context, filter Filter) ([]*Session, error) {
	all := s.store.listSessions()

	out := make([]*Session, 0, len(all))
	for _, sess := range all {
		if filter.Status != "" && string(sess.Status) != filter.Status {
			continue
		}
		out = append(out, toAgentSession(sess))
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// Send delivers a message to a session.
// For completed sessions the message triggers a resume.
func (s *Service) Send(ctx context.Context, sessionID string, msg Message) error {
	return s.sendMessage(ctx, sessionID, msg.Content)
}

// ResumeSession continues a previously finished or paused session.
func (s *Service) ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error) {
	if req.CarryOver != "" || req.UserInput != "" {
		content := req.UserInput
		if content == "" {
			content = req.CarryOver
		}
		if err := s.sendMessage(ctx, sessionID, content); err != nil {
			return nil, err
		}
	}
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	return toAgentSession(sess), nil
}

// Stream returns a real-time event channel for the session.
func (s *Service) Stream(ctx context.Context, sessionID string) (<-chan Event, error) {
	if s.store.snapshotSessionByID(sessionID) == nil {
		return nil, ErrSessionNotFound
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		seen := 0

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sess := s.store.snapshotSessionByID(sessionID)
				if sess == nil {
					return
				}
				for i := seen; i < len(sess.Events); i++ {
					select {
					case out <- *toAgentEvent(&sess.Events[i]):
					case <-ctx.Done():
						return
					}
				}
				seen = len(sess.Events)
				if sess.Status != enums.SessionStatusRunning && sess.Status != enums.SessionStatusAwaitingClarify {
					select {
					case <-time.After(200 * time.Millisecond):
					case <-ctx.Done():
					}
					return
				}
			}
		}
	}()

	return out, nil
}

// Query answers a read-only question about a session.
func (s *Service) Query(ctx context.Context, sessionID string, q Query) (Result, error) {
	switch q.Kind {
	case QueryKindBoard:
		return s.queryBoard(ctx, sessionID)
	case QueryKindMailbox:
		return s.queryMailbox(ctx, sessionID)
	case QueryKindMetrics:
		return s.queryMetrics(ctx, sessionID)
	case QueryKindWatchdog:
		return s.queryWatchdog(ctx, sessionID)
	case QueryKindTokenMetrics:
		return s.queryTokenMetrics(ctx, sessionID)
	case QueryKindLogs:
		return s.queryLogs(ctx, sessionID, q.Args)
	case QueryKindSessionCount:
		return Result{Data: s.store.sessionCount()}, nil
	case QueryKindLLMStats:
		calls, timeouts, avg, max := s.store.llmStats()
		return Result{Data: map[string]any{
			"calls":         calls,
			"timeouts":      timeouts,
			"avg_duration":  avg.Round(time.Millisecond).String(),
			"max_duration":  max.Round(time.Millisecond).String(),
		}}, nil
	default:
		return Result{}, fmt.Errorf("unknown query kind: %s", q.Kind)
	}
}

// Control sends an operational command to a session.
func (s *Service) Control(ctx context.Context, sessionID string, cmd ControlCommand) error {
	switch cmd.Op {
	case ControlOpMessage:
		content, _ := cmd.Args["content"].(string)
		return s.sendMessage(ctx, sessionID, content)
	case ControlOpClarify:
		answer, _ := cmd.Args["answer"].(string)
		return s.answerClarify(ctx, sessionID, answer)
	case ControlOpInterrupt:
		content, _ := cmd.Args["content"].(string)
		return s.interrupt(ctx, sessionID, content)
	case ControlOpEnqueue:
		content, _ := cmd.Args["content"].(string)
		return s.enqueue(ctx, sessionID, content)
	case ControlOpCancel:
		return s.cancel(ctx, sessionID)
	case ControlOpTopic:
		name, _ := cmd.Args["name"].(string)
		goal, _ := cmd.Args["goal"].(string)
		return s.switchTopic(ctx, sessionID, name, goal)
	default:
		return fmt.Errorf("unknown control op: %s", cmd.Op)
	}
}

// ListAgents returns the runtime agent instances associated with a session.
func (s *Service) ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error) {
	insts := s.registry.GetInstancesBySession(sessionID)
	out := make([]AgentInstance, 0, len(insts))
	for _, inst := range insts {
		ai := toAgentInstance(inst)
		switch ai.RoleType {
		case enums.RoleTypeDomain, enums.RoleTypeSubDomain:
			if ai.Domain != "" {
				ai.Name = ai.Domain
			}
		default:
			if def := s.registry.GetRoleDef(inst.RoleDefID); def != nil && def.Name != "" {
				ai.Name = def.Name
			}
		}
		out = append(out, *ai)
	}
	return out, nil
}

// Shutdown cancels all running sessions.
func (s *Service) Shutdown(ctx context.Context) error {
	s.store.shutdown()
	return nil
}

// LaunchSession implements dag.SessionLauncher.
func (s *Service) LaunchSession(goal string) string {
	sess := s.store.createSession(context.Background(), goal)
	return sess.ID
}

// RestoreSessions loads historical sessions from Postgres into memory.
func (s *Service) RestoreSessions(ctx context.Context, limit int) int {
	return s.store.restoreSessions(ctx, limit)
}

// ClearSessionChat clears a session's chat history while retaining the initial
// system/user messages.
func (s *Service) ClearSessionChat(id string) bool {
	return s.store.clearSessionChat(id)
}

// SessionCount returns the number of sessions currently held in memory.
func (s *Service) SessionCount() int {
	return s.store.sessionCount()
}

// LLMStats returns aggregated LLM call statistics.
func (s *Service) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	return s.store.llmStats()
}

// SummarizeTaskTitle returns a brief display title for a long task title.
func (s *Service) SummarizeTaskTitle(ctx context.Context, title string) string {
	if s.store.modelFactory == nil {
		return title
	}
	prompt := fmt.Sprintf("将以下任务描述压缩成 40 字以内的简短任务名，保留核心动作与对象，不要解释：\n%s", title)
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	brief, err := s.store.modelFactory.CallLightweightWithRetry(callCtx, prompt)
	if err != nil || strings.TrimSpace(brief) == "" {
		return title
	}
	brief = strings.TrimSpace(brief)
	brief = strings.Trim(brief, "\"'"+"`「」【】()")
	if len([]rune(brief)) > 40 {
		brief = string([]rune(brief)[:40]) + "…"
	}
	return brief
}

// internal helpers

func (s *Service) sendMessage(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}

	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		revived := s.store.reviveFromHistory(sessionID)
		if revived == nil {
			return ErrSessionNotFound
		}
		s.store.mu.Lock()
		session = revived
	} else if session.Status == enums.SessionStatusCompleted && len(session.Events) == 0 && session.Result != "" {
		s.store.mu.Unlock()
		revived := s.store.reviveFromHistory(sessionID)
		s.store.mu.Lock()
		if revived != nil {
			session = revived
		}
	}

	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   content,
		Timestamp: time.Now(),
	})
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.UserMessage,
		Agent:     "User",
		Message:   content,
		Success:   true,
		Timestamp: time.Now(),
	})

	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	s.store.mu.Unlock()

	if !wasRunning {
		s.store.resetSessionRuntime(session.ID)
		go s.store.resumeSession(session)
	}
	return nil
}

func (s *Service) answerClarify(ctx context.Context, sessionID, answer string) error {
	if answer == "" {
		return fmt.Errorf("answer cannot be empty")
	}
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	if session.Status != enums.SessionStatusAwaitingClarify {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not awaiting clarification", ErrInvalidSessionState)
	}
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   "[澄清答复] " + answer,
		Timestamp: time.Now(),
	})
	if session.State != nil {
		session.State.PendingClarify = nil
	}
	session.Status = enums.SessionStatusRunning
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.Clarify,
		Agent:     "User",
		Message:   "用户答复: " + answer,
		Success:   true,
		Timestamp: time.Now(),
	})
	s.store.mu.Unlock()

	s.store.resetSessionRuntime(session.ID)
	go s.store.resumeSession(session)
	return nil
}

func (s *Service) interrupt(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	rt := s.store.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		return ErrQueueFull
	}
	if err := rt.CmdQueue.Enqueue(sessionID, cmdqueue.Item{Content: content, Intent: cmdqueue.IntentInterrupt}); err != nil {
		return ErrQueueFull
	}

	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.Interrupt,
		Agent:     "User",
		Message:   "抢占中断: " + content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	s.store.mu.Unlock()

	if !wasRunning {
		s.store.resetSessionRuntime(session.ID)
		go s.store.resumeSession(session)
	}
	return nil
}

func (s *Service) enqueue(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	rt := s.store.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		return ErrQueueFull
	}
	if err := rt.CmdQueue.Enqueue(sessionID, cmdqueue.Item{Content: content, Intent: cmdqueue.IntentEnqueue}); err != nil {
		return ErrQueueFull
	}

	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.Enqueue,
		Agent:     "User",
		Message:   "队列注入: " + content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	s.store.mu.Unlock()

	if !wasRunning {
		s.store.resetSessionRuntime(session.ID)
		go s.store.resumeSession(session)
	}
	return nil
}

func (s *Service) cancel(ctx context.Context, sessionID string) error {
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	if session.Status != enums.SessionStatusRunning {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not running", ErrInvalidSessionState)
	}
	cancelFn := session.cancelFn
	session.cancelFn = nil
	session.Status = enums.SessionStatusError
	session.Result = "cancelled by user"
	now := time.Now()
	session.EndedAt = &now
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.System,
		Agent:     "System",
		Message:   "会话已被用户取消",
		Success:   true,
		Timestamp: now,
	})
	s.store.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}
	return nil
}

// SwitchTopic switches the active topic of a session. For running sessions it
// enqueues an interrupt with the new goal; for non-running sessions it creates
// a new session and returns it.
func (s *Service) SwitchTopic(ctx context.Context, sessionID, name, goal string) (*Session, error) {
	if name == "" {
		return nil, fmt.Errorf("name cannot be empty")
	}
	if goal == "" {
		goal = name
	}

	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return nil, ErrSessionNotFound
	}

	wasRunning := session.Status == enums.SessionStatusRunning
	oldDomain := ""
	if session.State != nil {
		oldDomain = session.State.CurrentDomain
	}
	session.Events = append(session.Events, internalEvent{
		Type:      eventkind.Progress,
		Agent:     "User",
		Message:   fmt.Sprintf("切换话题: 从 [%s] 到 [%s]", oldDomain, name),
		Kind:      eventkind.TopicSwitch,
		Success:   true,
		Timestamp: time.Now(),
	})
	s.store.mu.Unlock()

	if wasRunning {
		rt := s.store.graph.Runtime()
		if rt != nil && rt.CmdQueue != nil {
			if err := rt.CmdQueue.Enqueue(sessionID, cmdqueue.Item{Content: goal, Intent: cmdqueue.IntentInterrupt}); err != nil {
				return nil, ErrQueueFull
			}
		}
		return toAgentSession(s.store.snapshotSessionByID(sessionID)), nil
	}

	return toAgentSession(s.store.createSession(ctx, goal)), nil
}

func (s *Service) switchTopic(ctx context.Context, sessionID, name, goal string) error {
	_, err := s.SwitchTopic(ctx, sessionID, name, goal)
	return err
}

// query implementations

func (s *Service) queryBoard(ctx context.Context, sessionID string) (Result, error) {
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return Result{}, ErrSessionNotFound
	}
	var snap any
	if rt := s.store.graph.Runtime(); rt != nil && rt.Boards != nil {
		if b := rt.Boards.Get(sessionID); b != nil {
			snap = b.Snapshot()
		}
	}
	return Result{Data: snap}, nil
}

func (s *Service) queryMailbox(ctx context.Context, sessionID string) (Result, error) {
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return Result{}, ErrSessionNotFound
	}
	var msgs []map[string]any
	rt := s.store.graph.Runtime()
	if rt != nil && rt.Mailbox != nil {
		instances := s.registry.GetInstancesBySession(sessionID)
		seen := make(map[string]bool)
		for _, inst := range instances {
			for _, msg := range rt.Mailbox.Peek(inst.ID) {
				if seen[msg.ID] {
					continue
				}
				seen[msg.ID] = true
				msgs = append(msgs, map[string]any{
					"id":         msg.ID,
					"from":       msg.From,
					"to":         msg.To,
					"type":       msg.Type,
					"subject":    msg.Subject,
					"body":       msg.Body,
					"priority":   msg.Priority,
					"status":     msg.Status,
					"created_at": msg.CreatedAt,
				})
			}
		}
		for _, msg := range rt.Mailbox.DrainBroadcast() {
			if seen[msg.ID] {
				continue
			}
			seen[msg.ID] = true
			msgs = append(msgs, map[string]any{
				"id":         msg.ID,
				"from":       msg.From,
				"to":         "*",
				"type":       msg.Type,
				"subject":    msg.Subject,
				"body":       msg.Body,
				"priority":   msg.Priority,
				"status":     msg.Status,
				"created_at": msg.CreatedAt,
				"broadcast":  true,
			})
		}
	}
	return Result{Data: msgs}, nil
}

func (s *Service) queryMetrics(ctx context.Context, sessionID string) (Result, error) {
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return Result{}, ErrSessionNotFound
	}
	var calls, timeouts, inputTokens, outputTokens int
	var maxDur time.Duration
	var totalDur time.Duration
	for _, ev := range sess.Events {
		if ev.Kind != eventkind.TokenUsage {
			continue
		}
		calls++
		inputTokens += ev.InputTokens
		outputTokens += ev.OutputTokens
		if strings.Contains(ev.Message, "timeout") || strings.Contains(ev.Message, "超时") {
			timeouts++
		}
		dur := textutil.ParseDurationFromTokenUsage(ev.Message)
		totalDur += dur
		if dur > maxDur {
			maxDur = dur
		}
	}
	avgDur := time.Duration(0)
	if calls > 0 {
		avgDur = totalDur / time.Duration(calls)
	}
	return Result{Data: map[string]any{
		"session_id":    sessionID,
		"calls":         calls,
		"timeouts":      timeouts,
		"avg_duration":  avgDur.Round(time.Millisecond).String(),
		"max_duration":  maxDur.Round(time.Millisecond).String(),
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
	}}, nil
}

func (s *Service) queryWatchdog(ctx context.Context, sessionID string) (Result, error) {
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return Result{}, ErrSessionNotFound
	}
	var decisions []map[string]any
	if rt := s.store.graph.Runtime(); rt != nil && rt.Watchdog != nil {
		for _, d := range rt.Watchdog.History() {
			if sess.State == nil {
				continue
			}
			if sess.State.ActiveBlocks[d.AgentID] == nil {
				found := false
				for _, bid := range sess.State.CompletedBlocks {
					if bid == d.AgentID {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			decisions = append(decisions, map[string]any{
				"agent_id":    d.AgentID,
				"tokens":      d.Tokens,
				"level":       d.Level.String(),
				"reason":      d.Reason,
				"suggested":   d.Suggested,
				"occurred_at": d.OccurredAt,
			})
		}
	}
	return Result{Data: decisions}, nil
}

func (s *Service) queryTokenMetrics(ctx context.Context, sessionID string) (Result, error) {
	if s.store.pgStore == nil {
		return Result{}, ErrPostgresUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	agg, err := s.store.pgStore.AggregateSessionTokens(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	stats := make([]map[string]any, 0, len(agg.ByAgentModel))
	for _, v := range agg.ByAgentModel {
		stats = append(stats, map[string]any{
			"agent":         v.Agent,
			"model":         v.Model,
			"input_tokens":  v.InputTokens,
			"output_tokens": v.OutputTokens,
			"calls":         v.Calls,
		})
	}
	layerMap := make(map[string]map[string]any)
	for _, v := range agg.ByAgentModel {
		layer := model.CallerToLayer(v.Agent)
		ls, ok := layerMap[layer]
		if !ok {
			ls = map[string]any{"layer": layer, "input_tokens": 0, "output_tokens": 0, "calls": 0}
			layerMap[layer] = ls
		}
		ls["input_tokens"] = ls["input_tokens"].(int) + v.InputTokens
		ls["output_tokens"] = ls["output_tokens"].(int) + v.OutputTokens
		ls["calls"] = ls["calls"].(int) + v.Calls
	}
	layerStats := make([]map[string]any, 0, len(layerMap))
	for _, ls := range layerMap {
		layerStats = append(layerStats, ls)
	}
	return Result{Data: map[string]any{
		"session_id":          sessionID,
		"total_input_tokens":  agg.TotalInputTokens,
		"total_output_tokens": agg.TotalOutputTokens,
		"total_calls":         agg.TotalCalls,
		"stats":               stats,
		"layer_stats":         layerStats,
	}}, nil
}

func (s *Service) queryLogs(ctx context.Context, sessionID string, args map[string]any) (Result, error) {
	if s.store.pgStore == nil {
		return Result{}, ErrPostgresUnavailable
	}
	agentFilter, _ := args["agent"].(string)
	level, _ := args["level"].(string)
	limit, _ := strconv.Atoi(fmt.Sprint(args["limit"]))
	offset, _ := strconv.Atoi(fmt.Sprint(args["offset"]))

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	recs, err := s.store.pgStore.QuerySessionLogs(ctx, sessionID, agentFilter, level, limit, offset)
	if err != nil {
		return Result{}, err
	}
	items := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		items = append(items, map[string]any{
			"id":            rec.ID,
			"session_id":    rec.SessionID,
			"agent":         rec.Agent,
			"level":         rec.Level,
			"phase":         rec.Phase,
			"message":       rec.Message,
			"prompt":        textutil.RedactSensitive(rec.Prompt),
			"response":      textutil.RedactSensitive(rec.Response),
			"input_tokens":  rec.InputTokens,
			"output_tokens": rec.OutputTokens,
			"model":         rec.Model,
			"latency_ms":    rec.LatencyMs,
			"created_at":    rec.CreatedAt,
			"meta":          rec.Meta,
		})
	}
	return Result{Data: map[string]any{
		"session_id": sessionID,
		"logs":       items,
		"count":      len(items),
	}}, nil
}
