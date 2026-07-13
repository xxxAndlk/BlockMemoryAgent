package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/graph"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
)

// internalSession is the canonical in-memory runtime representation of a
// session. It intentionally mirrors the former server.Session shape so the
// lifecycle code can move here with minimal behavioural change.
type internalSession struct {
	ID        string                 `json:"id"`
	Goal      string                 `json:"goal"`
	Status    enums.SessionStatus    `json:"status"`
	Result    string                 `json:"result,omitempty"`
	State     *types.ThreeLayerState `json:"state,omitempty"`
	StartedAt time.Time              `json:"started_at"`
	EndedAt   *time.Time             `json:"ended_at,omitempty"`
	Events    []internalEvent        `json:"events"`
	Messages  []types.ChatMessage    `json:"messages"`
	TempDir   string                 `json:"temp_dir,omitempty"`
	cancelFn  context.CancelFunc     `json:"-"`
}

// internalEvent is the runtime representation of a session event. It mirrors
// the former server.SessionEvent shape.
type internalEvent struct {
	Type         string    `json:"type"`
	Agent        string    `json:"agent"`
	Message      string    `json:"message"`
	Kind         string    `json:"kind,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	ToolPath     string    `json:"tool_path,omitempty"`
	ToolOutput   string    `json:"tool_output,omitempty"`
	ToolError    string    `json:"tool_error,omitempty"`
	Success      bool      `json:"success,omitempty"`
	Timestamp    time.Time `json:"timestamp"`
	Prompt       string    `json:"prompt,omitempty"`
	InputTokens  int       `json:"input_tokens,omitempty"`
	OutputTokens int       `json:"output_tokens,omitempty"`
	DetailJSON   string    `json:"detail_json,omitempty"`
}

const maxInMemorySessions = 20

// sessionStore owns the in-memory session map and lifecycle helpers.
type sessionStore struct {
	mu           sync.RWMutex
	sessions     map[string]*internalSession
	graph        *graph.ThreeLayerGraph
	registry     *graph.RoleRegistry
	seq          atomic.Int64
	pgStore      *store.PostgresStore
	modelFactory *model.ModelFactory
	workDir      string
	metrics      *metricsCollector
}

func newSessionStore(g *graph.ThreeLayerGraph, registry *graph.RoleRegistry) *sessionStore {
	workDir, _ := os.Getwd()
	store := &sessionStore{
		sessions: make(map[string]*internalSession),
		graph:    g,
		registry: registry,
		workDir:  workDir,
		metrics:  newMetricsCollector(),
	}

	g.SetToolCallback(graph.ToolCallback(func(result *graph.ToolResult) {
		store.handleToolResult(result)
	}))
	g.SetProgressCallback(graph.ProgressCallback(func(ctx context.Context, ev graph.ProgressEvent) {
		store.handleProgress(ctx, ev)
	}))

	return store
}

func (st *sessionStore) setPostgresStore(pg *store.PostgresStore) {
	st.pgStore = pg
}

func (st *sessionStore) setModelFactory(mf *model.ModelFactory) {
	st.modelFactory = mf
}

func (st *sessionStore) resetSessionRuntime(sessionID string) {
	if st.graph != nil {
		if rt := st.graph.Runtime(); rt != nil && rt.Boards != nil {
			rt.Boards.Remove(sessionID)
		}
	}
	if st.registry != nil {
		st.registry.RemoveSessionInstances(sessionID)
	}
}

func (st *sessionStore) launchSession(goal string) string {
	s := st.createSession(context.Background(), goal)
	return s.ID
}

func (st *sessionStore) restoreSessions(ctx context.Context, limit int) int {
	if st.pgStore == nil {
		return 0
	}
	if limit <= 0 {
		limit = 50
	}
	recs, err := st.pgStore.RecentSessionHistories(ctx, limit)
	if err != nil {
		log.Printf("恢复会话失败: %v", err)
		return 0
	}

	restored := 0
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, rec := range recs {
		if _, exists := st.sessions[rec.SessionID]; exists {
			continue
		}
		endedAt := rec.CreatedAt
		restoredEvents := st.loadSessionEvents(ctx, rec.SessionID)
		simMsgs := []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
			{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
		}
		if len(restoredEvents) > 0 {
			simMsgs = extractMessagesFromEvents(restoredEvents, rec.Goal, rec.Summary)
		}
		st.sessions[rec.SessionID] = &internalSession{
			ID:        rec.SessionID,
			Goal:      rec.Goal,
			Status:    enums.SessionStatusCompleted,
			Result:    rec.Summary,
			StartedAt: rec.CreatedAt,
			EndedAt:   &endedAt,
			Events:    restoredEvents,
			Messages:  simMsgs,
		}
		restored++
	}

	var maxSeq int64
	for _, rec := range recs {
		if id := rec.SessionID; strings.HasPrefix(id, "session-") {
			if n, err := strconv.ParseInt(strings.TrimPrefix(id, "session-"), 10, 64); err == nil && n > maxSeq {
				maxSeq = n
			}
		}
	}
	if maxSeq > 0 {
		st.seq.Store(maxSeq)
	}

	if restored > 0 {
		log.Printf("从历史恢复了 %d 个会话", restored)
	}
	return restored
}

func (st *sessionStore) cleanupSessionTempDir(sessionID, tempDir string) {
	if tempDir == "" {
		return
	}
	if _, err := os.Stat(tempDir); os.IsNotExist(err) {
		return
	}
	if err := os.RemoveAll(tempDir); err != nil {
		log.Printf("[%s] 清理临时目录失败: %v", sessionID, err)
	} else {
		log.Printf("[%s] 已清理临时目录: %s", sessionID, tempDir)
	}
}

func (st *sessionStore) evictCompletedSessions() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.sessions) <= maxInMemorySessions {
		return
	}
	type kv struct {
		id    string
		ended time.Time
	}
	var completed []kv
	for id, s := range st.sessions {
		if s.Status == enums.SessionStatusRunning || s.EndedAt == nil {
			continue
		}
		completed = append(completed, kv{id, *s.EndedAt})
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].ended.Before(completed[j].ended)
	})
	excess := len(st.sessions) - maxInMemorySessions
	dropped := 0
	for i := 0; i < len(completed) && dropped < excess; i++ {
		delete(st.sessions, completed[i].id)
		dropped++
	}
	if dropped > 0 {
		log.Printf("从内存淘汰了 %d 个已完成会话 (保留 %d)", dropped, len(st.sessions))
	}
}

func (st *sessionStore) handleToolResult(result *graph.ToolResult) {
	st.mu.RLock()
	targets := make([]*internalSession, 0, 1)
	for _, session := range st.sessions {
		if session.Status != enums.SessionStatusRunning {
			continue
		}
		if result.SessionID != "" && session.ID != result.SessionID {
			continue
		}
		targets = append(targets, session)
	}
	st.mu.RUnlock()

	for _, session := range targets {
		msg := fmt.Sprintf("执行工具: %s (path=%s)", result.Tool, result.Path)
		if result.ArgsJSON != "" {
			msg = fmt.Sprintf("执行工具: %s 入参=%s", result.Tool, result.ArgsJSON)
		}
		st.addEvent(session, eventkind.ToolExec, "ToolExecutor", msg,
			"", result.Tool, result.Path, result.Output, result.Error, result.Success)
	}
}

func (st *sessionStore) handleProgress(ctx context.Context, ev graph.ProgressEvent) {
	msg := textutil.TruncateRunes(ev.Message, 1000, "...")
	success := ev.Kind != eventkind.Error

	var prompt string
	var inputTokens, outputTokens int
	switch ev.Kind {
	case eventkind.Prompt:
		prompt = ev.Detail
	case eventkind.TokenUsage:
		inputTokens, outputTokens = textutil.ParseTokenUsage(ev.Message)
		dur := textutil.ParseDurationFromTokenUsage(ev.Message)
		latencyMs := int(dur.Milliseconds())
		if strings.Contains(ev.Message, "timeout") || strings.Contains(ev.Message, "超时") {
			latencyMs = -latencyMs
		}
		st.metrics.record(ev.Agent, "", inputTokens, outputTokens, latencyMs)
	}

	st.mu.RLock()
	targets := make([]*internalSession, 0, 1)
	for _, session := range st.sessions {
		if session.Status != enums.SessionStatusRunning {
			continue
		}
		if ev.SessionID != "" && session.ID != ev.SessionID {
			continue
		}
		targets = append(targets, session)
	}
	st.mu.RUnlock()

	for _, session := range targets {
		st.addEventDebug(session, eventkind.Progress, ev.Agent, msg, ev.Kind, ev.Tool, "", "", "", success, prompt, inputTokens, outputTokens, ev.Detail)
	}
}

func (st *sessionStore) createSession(ctx context.Context, goal string) *internalSession {
	sessionID := fmt.Sprintf("session-%d", st.seq.Add(1))

	session := &internalSession{
		ID:        sessionID,
		Goal:      goal,
		Status:    enums.SessionStatusRunning,
		StartedAt: time.Now(),
		Events:    make([]internalEvent, 0),
		TempDir:   filepath.Join(st.workDir, ".bma", "tmp", sessionID),
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleSystem, Content: "Goal: " + goal, Timestamp: time.Now()},
			{Role: enums.ChatRoleUser, Content: goal, Timestamp: time.Now()},
		},
	}

	runCtx, cancelFn := context.WithCancel(context.Background())
	session.cancelFn = cancelFn

	st.mu.Lock()
	st.sessions[sessionID] = session
	st.mu.Unlock()

	go st.runSession(runCtx, session)

	return session
}

func (st *sessionStore) getSession(id string) *internalSession {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.sessions[id]
}

func (st *sessionStore) llmStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	snap := st.metrics.snapshot()
	return snap.CallCount,
		snap.TimeoutCount,
		time.Duration(snap.AvgDurationMs) * time.Millisecond,
		time.Duration(snap.MaxDurationMs) * time.Millisecond
}

func (st *sessionStore) sessionCount() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.sessions)
}

func (st *sessionStore) listSessions() []*internalSession {
	st.mu.RLock()
	defer st.mu.RUnlock()
	result := make([]*internalSession, 0, len(st.sessions))
	for _, s := range st.sessions {
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartedAt.After(result[j].StartedAt)
	})
	return result
}

func (st *sessionStore) shutdown() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.sessions {
		if s.cancelFn != nil {
			s.cancelFn()
			s.cancelFn = nil
		}
	}
}

func (st *sessionStore) snapshotSession(session *internalSession) *internalSession {
	if session == nil {
		return nil
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	cp := *session
	if session.Events != nil {
		cp.Events = make([]internalEvent, len(session.Events))
		copy(cp.Events, session.Events)
	}
	if session.Messages != nil {
		cp.Messages = make([]types.ChatMessage, len(session.Messages))
		copy(cp.Messages, session.Messages)
	}
	return &cp
}

func (st *sessionStore) snapshotSessionByID(id string) *internalSession {
	st.mu.RLock()
	session, ok := st.sessions[id]
	st.mu.RUnlock()
	if !ok {
		return nil
	}
	return st.snapshotSession(session)
}

func (st *sessionStore) addEvent(session *internalSession, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool) {
	st.addEventDebug(session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError, success, "", 0, 0, "")
}

func (st *sessionStore) addEventDebug(session *internalSession, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool, prompt string, inputTokens, outputTokens int, detailJSON string) {
	toolOutput = textutil.TruncateRunes(toolOutput, 4096, "...(truncated)")
	ev := internalEvent{
		Type:         eventType,
		Agent:        agent,
		Message:      message,
		Kind:         kind,
		Tool:         tool,
		ToolPath:     toolPath,
		ToolOutput:   toolOutput,
		ToolError:    toolError,
		Success:      success,
		Timestamp:    time.Now(),
		Prompt:       prompt,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		DetailJSON:   detailJSON,
	}
	st.mu.Lock()
	session.Events = append(session.Events, ev)
	if len(session.Events) > 500 {
		session.Events = trimDebugEvents(session.Events, 200)
	}
	st.mu.Unlock()

	log.Printf("[%s] %s: %s", session.ID, agent, message)
}

func (st *sessionStore) loadSessionEvents(ctx context.Context, sessionID string) []internalEvent {
	if st.pgStore == nil {
		return make([]internalEvent, 0)
	}
	records, _ := st.pgStore.GetSessionEvents(ctx, sessionID)
	events := make([]internalEvent, 0, len(records))
	for _, r := range records {
		events = append(events, internalEvent{
			Type:         r.Type,
			Agent:        r.Agent,
			Message:      r.Message,
			Kind:         r.Kind,
			Tool:         r.Tool,
			ToolPath:     r.ToolPath,
			ToolOutput:   r.ToolOutput,
			ToolError:    r.ToolError,
			Success:      r.Success,
			Timestamp:    r.Timestamp,
			Prompt:       r.Prompt,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			DetailJSON:   r.DetailJSON,
		})
	}
	return events
}

func (st *sessionStore) persistHistory(session *internalSession) {
	if st.pgStore == nil {
		return
	}
	toolResults := make([]map[string]any, 0, len(session.Events))
	for _, ev := range session.Events {
		if ev.Type != eventkind.ToolExec {
			continue
		}
		toolResults = append(toolResults, map[string]any{
			"tool":   ev.Tool,
			"path":   ev.ToolPath,
			"output": textutil.TruncateRunes(ev.ToolOutput, 500, "...(truncated)"),
			"error":  ev.ToolError,
			"ok":     ev.Success,
		})
	}
	metaMemory := make([]map[string]any, 0)
	if session.State != nil {
		for _, entry := range session.State.MetaMemory {
			metaMemory = append(metaMemory, map[string]any{
				"timestamp": entry.Timestamp,
				"source":    entry.Source,
				"content":   entry.Content,
				"tags":      entry.Tags,
			})
		}
	}
	rec := &store.SessionHistoryRecord{
		SessionID:   session.ID,
		Goal:        session.Goal,
		Summary:     session.Result,
		ToolResults: toolResults,
		MetaMemory:  metaMemory,
		CreatedAt:   time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := st.pgStore.SaveSessionHistory(ctx, rec); err != nil {
		log.Printf("[%s] 持久化会话历史失败: %v", session.ID, err)
	}
}

func (st *sessionStore) persistEvents(session *internalSession) {
	if st.pgStore == nil {
		return
	}
	records := make([]store.SessionEventRecord, 0, len(session.Events))
	for _, ev := range session.Events {
		records = append(records, store.SessionEventRecord{
			SessionID:    session.ID,
			Type:         ev.Type,
			Agent:        ev.Agent,
			Message:      sanitizeUTF8(ev.Message),
			Kind:         ev.Kind,
			Tool:         ev.Tool,
			ToolPath:     sanitizeUTF8(ev.ToolPath),
			ToolOutput:   sanitizeUTF8(textutil.TruncateRunes(ev.ToolOutput, 2048, "...(truncated)")),
			ToolError:    sanitizeUTF8(ev.ToolError),
			Success:      ev.Success,
			Timestamp:    ev.Timestamp,
			Prompt:       sanitizeUTF8(textutil.TruncateRunes(ev.Prompt, 2048, "...(truncated)")),
			InputTokens:  ev.InputTokens,
			OutputTokens: ev.OutputTokens,
			DetailJSON:   sanitizeUTF8(ev.DetailJSON),
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.pgStore.SaveSessionEvents(ctx, session.ID, records); err != nil {
		log.Printf("[%s] 持久化会话事件失败: %v", session.ID, err)
	}
}

func (st *sessionStore) clearSessionChat(id string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	session, ok := st.sessions[id]
	if !ok {
		return false
	}
	keep := make([]types.ChatMessage, 0, 2)
	for _, msg := range session.Messages {
		if msg.Role == enums.ChatRoleSystem || msg.Role == enums.ChatRoleUser {
			keep = append(keep, msg)
			if len(keep) >= 2 {
				break
			}
		}
	}
	session.Messages = keep
	session.Events = make([]internalEvent, 0)
	return true
}

func (st *sessionStore) reviveFromHistory(id string) *internalSession {
	if st.pgStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec, err := st.pgStore.GetSessionHistoryByID(ctx, id)
	if err != nil || rec == nil {
		return nil
	}
	endedAt := rec.CreatedAt

	events := make([]internalEvent, 0, len(rec.ToolResults)+2)
	for _, tr := range rec.ToolResults {
		tool, _ := tr["tool"].(string)
		path, _ := tr["path"].(string)
		output, _ := tr["output"].(string)
		toolErr, _ := tr["error"].(string)
		ok, _ := tr["ok"].(bool)
		events = append(events, internalEvent{
			Type:       eventkind.ToolExec,
			Agent:      "Assistant",
			Message:    fmt.Sprintf("调用工具 %s", tool),
			Tool:       tool,
			ToolPath:   path,
			ToolOutput: output,
			ToolError:  toolErr,
			Success:    ok,
			Timestamp:  rec.CreatedAt,
		})
	}

	msgs := make([]types.ChatMessage, 0, len(rec.ToolResults)+2)
	msgs = append(msgs, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   rec.Goal,
		Timestamp: rec.CreatedAt,
	})
	for _, tr := range rec.ToolResults {
		tool, _ := tr["tool"].(string)
		path, _ := tr["path"].(string)
		toolErr, _ := tr["error"].(string)
		output, _ := tr["output"].(string)
		content := fmt.Sprintf("调用工具 %s (path=%s)", tool, path)
		if toolErr != "" {
			content += "\n错误: " + toolErr
		}
		if output != "" {
			content += "\n结果: " + output
		}
		msgs = append(msgs, types.ChatMessage{
			Role:      enums.ChatRoleAssistant,
			Content:   content,
			Timestamp: rec.CreatedAt,
		})
	}
	msgs = append(msgs, types.ChatMessage{
		Role:      enums.ChatRoleAssistant,
		Content:   rec.Summary,
		Timestamp: rec.CreatedAt,
	})

	session := &internalSession{
		ID:        rec.SessionID,
		Goal:      rec.Goal,
		Status:    enums.SessionStatusCompleted,
		Result:    rec.Summary,
		StartedAt: rec.CreatedAt,
		EndedAt:   &endedAt,
		Events:    events,
		Messages:  msgs,
	}
	st.mu.Lock()
	st.sessions[id] = session
	st.mu.Unlock()
	return session
}

func (st *sessionStore) summarizeHistoryForGoal(ctx context.Context, sessionID, history, priorSummary string) (string, error) {
	if strings.TrimSpace(history) == "" {
		return "", nil
	}
	if st.modelFactory == nil {
		return "", fmt.Errorf("modelFactory not injected: lightweight model unavailable for session %s", sessionID)
	}
	prompt := fmt.Sprintf(`你是会话续接助手。请基于以下历史对话与上次会话摘要，提炼出用户当前想要完成的核心目标。
要求：
1. 用一句话（不超过 200 字）描述目标
2. 保留关键上下文，特别是以下信息必须原样保留，禁止改写或省略：
   - 文件路径（如 workspace/tower_defense/index.html、backend/internal/...）
   - 目录路径与工作区位置
   - 领域名 / 模块名 / 配置项名
   - 已尝试的方案与结论
3. 区分"用户的新目标"与"用户对上次任务的追问"：
   - 若用户在追问/确认上次产物（如"index.html 在哪""如何打开""路径是什么"），
     目标应表述为"基于上次任务（工作区: <路径>）回答用户追问: <追问内容>"，
     禁止把追问本身转成新目标（如"打开 index.html"是错误目标）
   - 若用户提出新需求，直接描述新需求并保留相关历史路径
4. 不要复述历史，只输出目标本身
5. 不要加任何前缀或解释

上次会话摘要:
%s

历史对话:
%s

用户当前目标：`, priorSummary, history)
	summaryCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, err := st.modelFactory.CallLightweightWithRetry(summaryCtx, prompt)
	if err != nil {
		return "", fmt.Errorf("lightweight summary for session %s failed: %w", sessionID, err)
	}
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return "", fmt.Errorf("lightweight summary for session %s returned empty response", sessionID)
	}
	return resp, nil
}

// helpers

func trimDebugEvents(events []internalEvent, maxDrop int) []internalEvent {
	dropped := 0
	out := make([]internalEvent, 0, len(events))
	for _, ev := range events {
		debugKind := ev.Kind == eventkind.Think || ev.Kind == eventkind.Prompt || ev.Kind == eventkind.TokenUsage || ev.Kind == eventkind.GraphStep
		if debugKind && dropped < maxDrop {
			dropped++
			continue
		}
		out = append(out, ev)
	}
	return out
}

func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func extractMessagesFromEvents(events []internalEvent, goal, summary string) []types.ChatMessage {
	var msgs []types.ChatMessage
	for _, ev := range events {
		if ev.Type == eventkind.UserMessage || ev.Type == eventkind.Message {
			msgs = append(msgs, types.ChatMessage{
				Role:      enums.ChatRoleUser,
				Content:   ev.Message,
				Timestamp: ev.Timestamp,
			})
		} else if ev.Agent != "User" && ev.Message != "" && (ev.Type == eventkind.AgentDone || ev.Type == eventkind.System) {
			if len(msgs) > 0 {
				msgs = append(msgs, types.ChatMessage{
					Role:      enums.ChatRoleAssistant,
					Content:   ev.Message,
					Timestamp: ev.Timestamp,
				})
			}
		}
	}
	if len(msgs) == 0 {
		msgs = []types.ChatMessage{
			{Role: enums.ChatRoleUser, Content: goal, Timestamp: time.Now()},
			{Role: enums.ChatRoleAssistant, Content: summary, Timestamp: time.Now()},
		}
	}
	return msgs
}


