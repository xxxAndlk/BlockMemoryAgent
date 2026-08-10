package server

import (
	"context"       // 用于传递请求上下文与超时控制
	"encoding/json" // 用于 HTTP 响应的 JSON 编码
	"errors"        // 用于错误判断（errors.Is）
	"fmt"           // 格式化日志消息
	"log"           // 未注入 logger 时的回退输出
	"net/http"      // HTTP 处理器与状态码
	"strconv"       // 字符串与数字转换
	"time"          // 时间类型与持续时间

	"github.com/blockmemory/agent/backend/internal/agent"  // Agent 门面接口
	"github.com/blockmemory/agent/backend/internal/logger" // 结构化日志器
	"github.com/blockmemory/agent/backend/internal/model"  // ModelFactory（兼容注入）
	"github.com/blockmemory/agent/backend/internal/store"  // PostgresStore（兼容注入）
	"github.com/blockmemory/agent/backend/pkg/enums"       // 会话状态、聊天角色等枚举
	"github.com/blockmemory/agent/backend/pkg/types"       // 共享类型（ThreeLayerState 等）
)

// Session 表示单个会话的运行时状态，同时作为 HTTP API 的传输对象（DTO）。
// 注意：会话的完整生命周期管理现在由 agent.Service 负责，SessionManager 仅作为薄适配层。
type Session struct {
	ID        string                 `json:"id"`                 // 会话唯一标识
	Goal      string                 `json:"goal"`               // 用户最初设定的目标
	Status    enums.SessionStatus    `json:"status"`             // 当前会话状态（running / completed / error 等）
	Result    string                 `json:"result,omitempty"`   // 最终结果摘要（可选）
	State     *types.ThreeLayerState `json:"state,omitempty"`    // 三层状态桥接对象（兼容旧前端，可选）
	StartedAt time.Time              `json:"started_at"`         // 会话开始时间
	EndedAt   *time.Time             `json:"ended_at,omitempty"` // 会话结束时间（可选）
	Events    []SessionEvent         `json:"events"`             // 会话事件流
	Messages  []types.ChatMessage    `json:"messages"`           // 用户与助手消息列表
	TempDir   string                 `json:"temp_dir,omitempty"` // 临时工作目录（可选）
	// StreamingText 是当前正在流式生成的助手文本（仅运行中有值），供前端实时渲染输出过程。
	StreamingText string `json:"streaming_text,omitempty"`
	// ThinkingText 是当前思考阶段的过程文本（瞬时，仅运行中有值），答复输出或结束时清空。
	ThinkingText string `json:"thinking_text,omitempty"`
	// ActiveTopicID 当前活跃话题 ID。话题切换时旧 Agent 树终结 + 新树起,
	// 旧话题摘要写入 sharedKV。空表示单话题(未切换过)。前端可据此展示话题列表。
	ActiveTopicID string `json:"active_topic_id,omitempty"`
	// DestroyAt 软停止销毁倒计时截止时间（TODO #37）：软停止后非 nil，续跑/到期后清空。
	// 前端可显示"MM:SS 后销毁"倒计时。
	DestroyAt *time.Time `json:"destroy_at,omitempty"`
}

// SessionEvent 是会话事件流中的单个事件，对应前端展示的一条日志/消息。
type SessionEvent struct {
	Type         string    `json:"type"`                    // 事件类型（如 think / tool_exec / token_usage 等）
	Agent        string    `json:"agent"`                   // 产生事件的 Agent 名称
	Message      string    `json:"message"`                 // 人类可读的事件描述
	Kind         string    `json:"kind,omitempty"`          // 事件细分种类（可选）
	Tool         string    `json:"tool,omitempty"`          // 工具名（可选）
	ToolPath     string    `json:"tool_path,omitempty"`     // 工具输出路径（可选）
	ToolOutput   string    `json:"tool_output,omitempty"`   // 工具标准输出（可选）
	ToolError    string    `json:"tool_error,omitempty"`    // 工具错误信息（可选）
	Success      bool      `json:"success,omitempty"`       // 工具/操作是否成功（可选）
	Timestamp    time.Time `json:"timestamp"`               // 事件发生时间
	Prompt       string    `json:"prompt,omitempty"`        // 关联的 LLM Prompt（可选）
	InputTokens  int       `json:"input_tokens,omitempty"`  // 输入 token 数（可选）
	OutputTokens int       `json:"output_tokens,omitempty"` // 输出 token 数（可选）
	DetailJSON   string    `json:"detail_json,omitempty"`   // 额外结构化详情（JSON 字符串，可选）
}

// SessionManager 是 agent.Agent 之上的薄 HTTP 适配层。
// 所有会话的变更与只读查询都委托给 Agent 门面，避免在 HTTP 层重复实现业务逻辑。
type SessionManager struct {
	agent agent.Agent    // Agent 门面接口
	log   *logger.Logger // 结构化日志器，由 SetLogger 注入；nil 时回退标准库 log
}

// NewSessionManager 创建一个委托给指定 Agent 门面的 HTTP 适配器。
// 参数 agentFacade：实现 agent.Agent 接口的对象。
// 返回值：*SessionManager，供 HTTP 路由注册使用。
func NewSessionManager(agentFacade agent.Agent) *SessionManager {
	return &SessionManager{agent: agentFacade}
}

// SetLogger 注入结构化日志器，使服务端错误以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log（级别固定 INFO）。
func (m *SessionManager) SetLogger(l *logger.Logger) {
	m.log = l
}

// logError 记录错误类日志；未注入 logger 时回退标准库 log，保持旧行为。
func (m *SessionManager) logError(ctx context.Context, msg string, err error) {
	if m.log != nil {
		m.log.Error(ctx, msg, err)
		return
	}
	log.Printf("%s: %v", msg, err)
}

// SetPostgresStore 保留该方法以保持接口兼容，但实际为无操作（no-op）。
// 原因：持久化配置已迁移到 agent.Service，HTTP 层不再直接持有存储。
// 参数 pg：PostgresStore 实例（被忽略）。
func (m *SessionManager) SetPostgresStore(pg *store.PostgresStore) {}

// SetModelFactory 保留该方法以保持接口兼容，但实际为无操作（no-op）。
// 原因：模型工厂已配置在 agent.Service 上，HTTP 层不再直接使用。
// 参数 mf：ModelFactory 实例（被忽略）。
func (m *SessionManager) SetModelFactory(mf *model.ModelFactory) {}

// LaunchSession 实现 dag.SessionLauncher 接口，用于通过目标文本启动会话。
// 参数 goal：用户目标描述。
// 返回值：新创建会话的 ID；创建失败时返回空字符串。
func (m *SessionManager) LaunchSession(goal string) string {
	// 使用后台上下文创建会话，因为 DAG 触发器不依赖具体请求上下文。
	s, err := m.agent.CreateSession(context.Background(), agent.CreateRequest{Goal: goal})
	if err != nil || s == nil {
		return ""
	}
	return s.ID
}

// RestoreSessions 将会话恢复工作委托给 Agent 门面。
// 参数 ctx：请求上下文；limit：最大恢复数量。
// 返回值：实际恢复的会话数。
func (m *SessionManager) RestoreSessions(ctx context.Context, limit int) int {
	// 通过局部接口做鸭子类型判断，兼容不同 Agent 实现。
	type restorer interface {
		RestoreSessions(ctx context.Context, limit int) int
	}
	if r, ok := m.agent.(restorer); ok {
		return r.RestoreSessions(ctx, limit)
	}
	return 0
}

// GetSession 根据 ID 获取 server.Session，内部把 agent.Session DTO 转换为 HTTP 线型。
// 参数 id：会话 ID。
// 返回值：转换后的 *Session；会话不存在或出错时返回 nil。
func (m *SessionManager) GetSession(id string) *Session {
	s, err := m.agent.Get(context.Background(), id)
	if err != nil || s == nil {
		return nil
	}
	return ToServerSession(s)
}

// LLMStats 通过 Agent 门面聚合 LLM 调用统计。
// 返回值：调用次数、超时次数、平均耗时、最大耗时。
func (m *SessionManager) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	// 优先使用直接实现 LLMStats() 的 Agent。
	type statsProvider interface {
		LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration)
	}
	if sp, ok := m.agent.(statsProvider); ok {
		return sp.LLMStats()
	}

	// 退而使用通用 Query 接口查询 LLM 统计。
	res, err := m.agent.Query(context.Background(), "", agent.Query{Kind: agent.QueryKindLLMStats})
	if err != nil {
		return 0, 0, 0, 0
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		return 0, 0, 0, 0
	}
	callCount, _ = data["calls"].(int)
	timeoutCount, _ = data["timeouts"].(int)
	if avgStr, ok := data["avg_duration"].(string); ok {
		avgDur, _ = time.ParseDuration(avgStr)
	}
	if maxStr, ok := data["max_duration"].(string); ok {
		maxDur, _ = time.ParseDuration(maxStr)
	}
	return
}

// SessionCount 返回当前内存中持有的会话数量。
// 返回值：会话数量；Agent 不支持时返回 0。
func (m *SessionManager) SessionCount() int {
	// 优先使用直接实现 SessionCount() 的 Agent。
	type counter interface {
		SessionCount() int
	}
	if c, ok := m.agent.(counter); ok {
		return c.SessionCount()
	}
	// 退而使用通用 Query 接口查询。
	res, err := m.agent.Query(context.Background(), "", agent.Query{Kind: agent.QueryKindSessionCount})
	if err != nil {
		return 0
	}
	if n, ok := res.Data.(int); ok {
		return n
	}
	return 0
}

// ListSessions 返回所有会话，均由 agent.Session 转换为 server.Session。
// 返回值：会话指针切片；出错时记录日志并返回 nil。
func (m *SessionManager) ListSessions() []*Session {
	sessions, err := m.agent.List(context.Background(), agent.Filter{})
	if err != nil {
		m.logError(context.Background(), "ListSessions error", err)
		return nil
	}
	out := make([]*Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, ToServerSession(s))
	}
	return out
}

// Shutdown 取消所有运行中的会话，通常用于服务优雅停机。
func (m *SessionManager) Shutdown() {
	_ = m.agent.Shutdown(context.Background())
}

// SnapshotSession 返回指定会话在 server.Session 形式下的深拷贝快照。
// 参数 id：会话 ID。
// 返回值：当前会话状态的快照。
func (m *SessionManager) SnapshotSession(id string) *Session {
	return m.GetSession(id)
}

// ClearSessionChat 委托给 Agent 门面清空会话聊天历史。
// 参数 id：会话 ID。
// 返回值：是否成功。
func (m *SessionManager) ClearSessionChat(id string) bool {
	type clearer interface {
		ClearSessionChat(id string) bool
	}
	if c, ok := m.agent.(clearer); ok {
		return c.ClearSessionChat(id)
	}
	return false
}

// ToServerSession 将 agent.Session DTO 转换为 server.Session HTTP 线型。
//
// 遗留桥接：ThreeLayerState 结构仅用于前端/API 兼容；
// 当前保留 current_domain / active_blocks 字段，等前端不再依赖后即可移除。
func ToServerSession(a *agent.Session) *Session {
	if a == nil {
		return nil
	}

	// 遗留桥接：把新的 ReAct Session DTO 映射回旧的 ThreeLayerState 线型，
	// 使现有 HTTP 客户端在不改动的情况下继续工作。
	state := &types.ThreeLayerState{
		CurrentDomain: a.State,
	}
	if len(a.ActiveBlocks) > 0 {
		state.ActiveBlocks = make(map[string]*types.SessionBlock, len(a.ActiveBlocks))
		for _, b := range a.ActiveBlocks {
			state.ActiveBlocks[b.ID] = &types.SessionBlock{
				ID:     b.ID,
				Domain: b.Domain,
				Goal:   b.Goal,
			}
		}
	}
	if a.PendingClarify != nil {
		req := a.PendingClarify
		state.PendingClarify = &types.ClarifyRequest{
			ID:         req.ID,
			Question:   req.Question,
			Context:    req.Context,
			AgentID:    req.AgentID,
			CreatedAt:  req.CreatedAt,
			Answer:     req.Answer,
			AnsweredAt: req.AnsweredAt,
		}
	}
	// 如果没有任何三层状态内容，则把 state 置为 nil，避免返回空对象。
	if a.State == "" && len(a.ActiveBlocks) == 0 && a.PendingClarify == nil {
		state = nil
	}

	// 处理结束时间：agent 使用 time.Time 零值表示未结束，HTTP 线型使用指针表示可选。
	var endedAt *time.Time
	if !a.EndedAt.IsZero() {
		t := a.EndedAt
		endedAt = &t
	}

	// 复制事件切片，保持顺序与内容一致。
	events := make([]SessionEvent, len(a.Events))
	for i, e := range a.Events {
		events[i] = SessionEvent{
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

	// 复制消息切片，角色字段使用枚举转换。
	messages := make([]types.ChatMessage, len(a.Messages))
	for i, msg := range a.Messages {
		messages[i] = types.ChatMessage{
			Role:      enums.ChatRole(msg.Role),
			Content:   msg.Content,
			Timestamp: msg.Timestamp,
		}
	}

	return &Session{
		ID:            a.ID,
		Goal:          a.Goal,
		Status:        enums.SessionStatus(a.Status),
		Result:        a.Result,
		State:         state,
		StartedAt:     a.StartedAt,
		EndedAt:       endedAt,
		Events:        events,
		Messages:      messages,
		TempDir:       a.TempDir,
		StreamingText: a.StreamingText,
		ThinkingText:  a.ThinkingText,
		ActiveTopicID: a.ActiveTopicID,
		DestroyAt:     a.DestroyAt,
	}
}

// HandleSessionMetrics 处理 GET /api/sessions/{id}/metrics。
// 返回指定会话的指标数据。
func (m *SessionManager) HandleSessionMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindMetrics})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// HandleSessionWatchdog 处理 GET /api/sessions/{id}/watchdog。
// 返回看门狗对会话的决策记录。
func (m *SessionManager) HandleSessionWatchdog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindWatchdog})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"decisions":  res.Data,
	})
}

// HandleSessionMailbox 处理 GET /api/sessions/{id}/mailbox。
// 返回会话邮箱中的消息列表。
func (m *SessionManager) HandleSessionMailbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindMailbox})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"messages":   res.Data,
	})
}

// HandleSessionLogs 处理 GET /api/sessions/{id}/logs。
// 支持 query 参数：agent、level、limit、offset。
func (m *SessionManager) HandleSessionLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	// 解析分页参数，转换失败时默认为 0（Atoi 返回 0）。
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	res, err := m.agent.Query(r.Context(), id, agent.Query{
		Kind: agent.QueryKindLogs,
		Args: map[string]any{
			"agent":  r.URL.Query().Get("agent"),
			"level":  r.URL.Query().Get("level"),
			"limit":  limit,
			"offset": offset,
		},
	})
	if err != nil {
		m.logError(r.Context(), fmt.Sprintf("[SessionManager] 查询 session_logs 失败: session=%s", id), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// HandleSessionTokenMetrics 处理 GET /api/sessions/{id}/token-metrics。
// 返回会话级别的 Token 消耗聚合。
func (m *SessionManager) HandleSessionTokenMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	if _, err := m.agent.Get(r.Context(), id); err != nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	res, err := m.agent.Query(r.Context(), id, agent.Query{Kind: agent.QueryKindTokenMetrics})
	if err != nil {
		m.logError(r.Context(), fmt.Sprintf("[SessionManager] 聚合 token 消耗失败: session=%s", id), err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.Data)
}

// agentErrorStatus 将 agent 的哨兵错误映射为 HTTP 状态码与可读消息。
// 未知错误返回 500 并附带原始错误信息。
// 参数 err：原始错误。
// 返回值：响应文本、HTTP 状态码。
func agentErrorStatus(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	switch {
	case errors.Is(err, agent.ErrSessionNotFound):
		return "会话不存在", http.StatusNotFound
	case errors.Is(err, agent.ErrQueueFull):
		return "命令队列已满", http.StatusServiceUnavailable
	case errors.Is(err, agent.ErrSessionFinished), errors.Is(err, agent.ErrInvalidSessionState):
		return err.Error(), http.StatusBadRequest
	case errors.Is(err, agent.ErrPostgresUnavailable):
		return "存储后端不可用", http.StatusServiceUnavailable
	case errors.Is(err, agent.ErrAgentNotFound):
		return "Agent 不存在或已结束", http.StatusNotFound
	default:
		return err.Error(), http.StatusInternalServerError
	}
}
