package server

import (
	"context"       // 请求上下文与超时
	"encoding/json" // JSON 编解码
	"fmt"           // 格式化输出
	"log"           // 日志输出
	"net/http"      // HTTP 处理器
	"sort"          // 会话列表按时间排序
	"strconv"       // 字符串与数字转换
	"strings"       // 字符串处理
	"sync"          // 读写锁保护 sessions
	"sync/atomic"   // 原子计数器（seq）
	"time"          // 时间戳与超时

	"github.com/blockmemory/agent/backend/internal/cmdqueue" // 用户指令队列（特性6）
	"github.com/blockmemory/agent/backend/internal/graph"    // Graph 引擎
	"github.com/blockmemory/agent/backend/internal/model"    // 模型工厂（轻量模型用于历史总结）
	"github.com/blockmemory/agent/backend/internal/store"    // Postgres 存储
	"github.com/blockmemory/agent/backend/pkg/enums"         // 枚举常量
	"github.com/blockmemory/agent/backend/pkg/types"         // 共享类型
)

// Session 表示一次会话的完整运行时状态。
// 由 SessionManager 创建并维护，序列化后通过 /api/sessions 暴露给前端。
//
// 字段说明：
//   - ID: 会话 ID（session-N）
//   - Goal: 用户目标 / 请求
//   - Status: running / completed / error
//   - Result: 会话结果摘要
//   - State: 最终 Graph 状态
//   - StartedAt: 起始时间
//   - EndedAt: 结束时间（nil 表示未结束）
//   - Events: 事件流（含工具调用、思考、统计）
//   - Messages: 对话消息（含 system / user / assistant）
//   - cancelFn: 取消函数，用于终止正在运行的 graph.Invoke（nil 表示未运行）
type Session struct {
	ID        string                 `json:"id"`                 // 会话 ID（session-N）
	Goal      string                 `json:"goal"`               // 用户目标
	Status    enums.SessionStatus    `json:"status"`             // running / completed / error / awaiting_clarify
	Result    string                 `json:"result,omitempty"`   // 会话结果摘要
	State     *types.ThreeLayerState `json:"state,omitempty"`    // 最终 Graph 状态
	StartedAt time.Time              `json:"started_at"`         // 起始时间
	EndedAt   *time.Time             `json:"ended_at,omitempty"` // 结束时间（nil 表示未结束）
	Events    []SessionEvent         `json:"events"`             // 事件流
	Messages  []types.ChatMessage    `json:"messages"`           // 对话消息
	cancelFn  context.CancelFunc     `json:"-"`                  // 取消函数（不序列化）
}

// SessionEvent 是会话事件流的单个事件。
// 字段较宽松，覆盖 progress / tool_exec / system / error / stats 等多种类型。
type SessionEvent struct {
	Type       string    `json:"type"`                  // 事件类型（progress / tool_exec / system / error / stats / ...）
	Agent      string    `json:"agent"`                 // 触发事件的 Agent 名称
	Message    string    `json:"message"`               // 人类可读简述
	Kind       string    `json:"kind,omitempty"`        // progress 子类型: think/intend/llm/tool_call/tool_result/wait/error/prompt/agent_created/token_usage/graph_step
	Tool       string    `json:"tool,omitempty"`        // 工具名（tool_exec 时有效）
	ToolPath   string    `json:"tool_path,omitempty"`   // 工具操作文件路径
	ToolOutput string    `json:"tool_output,omitempty"` // 工具标准输出（截断）
	ToolError  string    `json:"tool_error,omitempty"`  // 工具错误信息
	Success    bool      `json:"success,omitempty"`     // 是否成功
	Timestamp  time.Time `json:"timestamp"`             // 事件时间戳

	// ---- 调试扩展字段（v3 debug） ----
	Prompt       string `json:"prompt,omitempty"`        // 发送给 LLM 的 prompt（截断）
	InputTokens  int    `json:"input_tokens,omitempty"`  // 输入 token 估算
	OutputTokens int    `json:"output_tokens,omitempty"` // 输出 token 估算
	DetailJSON   string `json:"detail_json,omitempty"`   // 结构化详情（Agent 创建参数、图步骤状态等）
}

// maxInMemorySessions 内存中保留的最大已完成会话数。
// 超过此阈值的最早完成会话从内存淘汰，仅保留 Postgres 持久化记录。
// 运行中会话不计入此上限，永不淘汰。
const maxInMemorySessions = 20

// SessionManager 管理所有运行中 / 已完成的会话。
// 持有 Graph 与 RoleRegistry 引用，通过 ToolCallback / ProgressCallback 把
// Graph 内部事件回流到对应会话的事件流。并发安全（RWMutex 保护 sessions 映射）。
type SessionManager struct {
	mu           sync.RWMutex           // 保护 sessions 映射
	sessions     map[string]*Session    // session_id -> Session
	graph        *graph.ThreeLayerGraph // 注入的 Graph 引擎
	registry     *graph.RoleRegistry    // 注入的角色注册表
	seq          atomic.Int64           // 会话 ID 自增计数
	pgStore      *store.PostgresStore   // 可选：Postgres 持久化
	modelFactory *model.ModelFactory    // 可选：模型工厂，用于续话时调轻量模型总结历史
}

// NewSessionManager 创建会话管理器，并把工具 / 进度回调注入 Graph。
// 参数：g - 三层图；registry - 角色注册表。
// 返回值：*SessionManager。
// 副作用：g.SetToolCallback / g.SetProgressCallback 注入闭包，回调把事件写入对应 Session。
func NewSessionManager(g *graph.ThreeLayerGraph, registry *graph.RoleRegistry) *SessionManager {
	m := &SessionManager{
		sessions: make(map[string]*Session), // 初始化 sessions 映射
		graph:    g,                         // 注入 Graph
		registry: registry,                  // 注入角色注册表
	}

	// 将工具回调注入图，使工具执行结果自动成为会话事件
	g.SetToolCallback(graph.ToolCallback(func(result *graph.ToolResult) {
		m.handleToolResult(result) // 工具结果路由到对应会话
	}))

	// 将进度回调注入图，把 Agent 的思考 / 意图 / 工具调用实时推给会话事件流
	g.SetProgressCallback(graph.ProgressCallback(func(ctx context.Context, ev graph.ProgressEvent) {
		m.handleProgress(ctx, ev) // 进度事件路由到对应会话
	}))

	return m
}

// SetPostgresStore 注入 Postgres 存储，用于会话结束时落历史。
// 参数：pg - Postgres 存储句柄（可为 nil，表示禁用持久化）。
func (m *SessionManager) SetPostgresStore(pg *store.PostgresStore) {
	m.pgStore = pg
}

// SetModelFactory 注入模型工厂，用于续话时调轻量模型总结历史消息。
// 参数：mf - 模型工厂句柄（可为 nil，表示禁用 LLM 总结，回退到原始 history 文本）。
func (m *SessionManager) SetModelFactory(mf *model.ModelFactory) {
	m.modelFactory = mf
}

// LaunchSession 实现 dag.SessionLauncher 接口（特性1）。
// 把一个 task.goal 派发为新 session，返回 sessionID。
func (m *SessionManager) LaunchSession(goal string) string {
	s := m.CreateSession(context.Background(), goal)
	return s.ID
}

// RestoreSessions 从 session_history 表恢复历史会话到内存映射。
//
// 服务重启后内存中的 m.sessions 会被清空，前端列表也就空了。本方法把
// 数据库里最近 N 条 session_history 记录加载成最小 Session 对象
// （只含 ID / Goal / Status / Result / StartedAt / Events=[]），
// 让用户在 UI 上仍能看到上次的会话。
//
// 注意：完整的 events / messages 流无法从 session_history 还原
// （该表只存 goal / summary / tool_results）。需要完整事件回放请走
// 未来扩展的 events 表 + SSE 归档。
//
// 参数：ctx - 上下文；limit - 最多恢复条数（<=0 时取默认 50）。
// 返回值：int - 实际恢复的条数。
// 并发安全：mu.Lock 保护 sessions 写入与 seq 更新。
func (m *SessionManager) RestoreSessions(ctx context.Context, limit int) int {
	if m.pgStore == nil {
		return 0 // 未注入 Postgres，直接返回 0
	}
	if limit <= 0 {
		limit = 50 // 默认恢复 50 条
	}
	recs, err := m.pgStore.RecentSessionHistories(ctx, limit) // 查询最近 N 条历史
	if err != nil {
		log.Printf("恢复会话失败: %v", err) // 查询失败仅记录日志
		return 0
	}

	restored := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rec := range recs {
		if _, exists := m.sessions[rec.SessionID]; exists {
			continue // 不要覆盖正在运行的会话
		}
		endedAt := rec.CreatedAt // 用创建时间作为结束时间（历史会话已结束）
			restoredEvents := m.loadSessionEvents(ctx, rec.SessionID)
			simMsgs := []types.ChatMessage{
				{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
				{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
			}
			if len(restoredEvents) > 0 {
				simMsgs = extractMessagesFromEvents(restoredEvents, rec.Goal, rec.Summary)
			}
			m.sessions[rec.SessionID] = &Session{
				ID:        rec.SessionID,
				Goal:      rec.Goal,
				Status:    enums.SessionStatusCompleted,
				Result:    rec.Summary,
				StartedAt: rec.CreatedAt,
				EndedAt:   &endedAt,
				Events:    restoredEvents,
				Messages:  simMsgs,
			}
		// 让列表的 seq 不与未来创建冲突
		restored++
	}

	// 根据已恢复会话的 ID 后缀同步 seq，避免新建会话 ID 与历史记录冲突
	var maxSeq int64
	for _, rec := range recs {
		if id := rec.SessionID; strings.HasPrefix(id, "session-") {
			if n, err := strconv.ParseInt(strings.TrimPrefix(id, "session-"), 10, 64); err == nil && n > maxSeq {
				maxSeq = n // 记录最大数字后缀
			}
		}
	}
	if maxSeq > 0 {
		m.seq.Store(maxSeq) // 同步原子计数器
	}

	if restored > 0 {
		log.Printf("从历史恢复了 %d 个会话", restored) // 输出恢复条数
	}
	return restored
}

// evictCompletedSessions 淘汰最早的已完成会话，把内存占用控制在 maxInMemorySessions 以内。
// 设计意图: 长期运行时 sessions map 无上限增长，每个会话的 Events 切片含 LLM prompt
// 与工具输出，可达 MB 级，最终 OOM。已完成会话已持久化到 session_history 表，
// 淘汰后前端仍可通过 HandleListSessions / HandleGetSession 从 DB 取回最小记录。
// 并发安全: mu.Lock 保护 delete。
func (m *SessionManager) evictCompletedSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) <= maxInMemorySessions {
		return
	}
	type kv struct {
		id    string
		ended time.Time
	}
	var completed []kv
	for id, s := range m.sessions {
		if s.Status == enums.SessionStatusRunning || s.EndedAt == nil {
			continue // 运行中不淘汰
		}
		completed = append(completed, kv{id, *s.EndedAt})
	}
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].ended.Before(completed[j].ended) // 升序：越早越先淘汰
	})
	excess := len(m.sessions) - maxInMemorySessions
	dropped := 0
	for i := 0; i < len(completed) && dropped < excess; i++ {
		delete(m.sessions, completed[i].id)
		dropped++
	}
	if dropped > 0 {
		log.Printf("从内存淘汰了 %d 个已完成会话 (保留 %d)", dropped, len(m.sessions))
	}
}

// handleToolResult 处理工具执行结果，将其路由到归属会话。
// 设计要点：不能在持有 m.mu.RLock 的情况下调用 addEvent（addEvent 内部取 Lock，
// 同 goroutine RLock+Lock 会自死锁，导致会话卡死）。先在 RLock 下收集目标
// 会话指针，释放后再写事件。
// 参数：result - 工具执行结果（含 sessionID / tool / output 等）。
// 副作用：对每个匹配的运行中会话追加 tool_exec 事件。
func (m *SessionManager) handleToolResult(result *graph.ToolResult) {
	// 注意：不能在持有 m.mu.RLock 的情况下调用 addEvent（addEvent 内部取 Lock，
	// 同 goroutine RLock+Lock 会自死锁，导致会话卡死）。先在 RLock 下收集目标
	// 会话指针，释放后再写事件。
	m.mu.RLock()
	targets := make([]*Session, 0, 1) // 收集目标会话指针
	for _, session := range m.sessions {
		if session.Status != enums.SessionStatusRunning {
			continue // 跳过非运行中会话
		}
		// 优先按工具结果携带的 sessionID 精确匹配，未携带时保持原广播行为兜底
		if result.SessionID != "" && session.ID != result.SessionID {
			continue // sessionID 不匹配则跳过
		}
		targets = append(targets, session) // 加入待写入列表
	}
	m.mu.RUnlock()

	for _, session := range targets {
		// 释放锁后再写入事件，避免 RLock+Lock 自死锁
		// message 含工具名 + 入参摘要，便于日志定位工具调用上下文
		msg := fmt.Sprintf("执行工具: %s (path=%s)", result.Tool, result.Path)
		if result.ArgsJSON != "" {
			msg = fmt.Sprintf("执行工具: %s 入参=%s", result.Tool, result.ArgsJSON)
		}
		m.addEvent(session, "tool_exec", "ToolExecutor", msg,
			"", result.Tool, result.Path, result.Output, result.Error, result.Success)
	}
}

// handleProgress 把 graph 的进度事件转为会话事件，推到当前运行中的会话。
// 事件类型映射:
//
//	think / intend / llm / wait -> "progress"
//	tool_call / tool_result     -> "progress"（tool_exec 仍由 handleToolResult 单独发）
//	error                       -> "progress"（标记 success=false）
//	prompt / agent_created / token_usage / graph_step -> 同上，但扩展字段携带调试信息
//
// 参数：ctx - 上下文（保留以备未来扩展）；ev - Graph 进度事件。
// 副作用：对每个匹配的运行中会话追加 progress 事件（含调试字段）。
func (m *SessionManager) handleProgress(ctx context.Context, ev graph.ProgressEvent) {
	// message 字段仅保留人类可读简述，detail 仅走 detail_json / prompt 字段，
	// 前端按需展开。之前把 detail 拼到 message 里，前端用 markdown 渲染多行
	// prompt（中英混排 + JSON + 代码），会出现"乱码夹在中文中"的视觉错位。
	msg := ev.Message
	if len(msg) > 1000 {
		msg = msg[:1000] + "..." // 防止超长 message 撑爆事件流
	}
	success := ev.Kind != "error" // error 类标记为失败

	// 提取调试信息
	var prompt string
	var inputTokens, outputTokens int
	switch ev.Kind {
	case "prompt":
		prompt = ev.Detail // 原始 prompt 摘要
	case "token_usage":
		inputTokens, outputTokens = parseTokenUsage(ev.Message) // 解析 in / out token
	}

	m.mu.RLock()
	targets := make([]*Session, 0, 1) // 收集目标会话指针
	for _, session := range m.sessions {
		if session.Status != enums.SessionStatusRunning {
			continue // 跳过非运行中会话
		}
		// 优先按进度事件携带的 sessionID 精确匹配，未携带时保持原广播行为兜底
		if ev.SessionID != "" && session.ID != ev.SessionID {
			continue // sessionID 不匹配则跳过
		}
		targets = append(targets, session) // 加入待写入列表
	}
	m.mu.RUnlock()

	for _, session := range targets {
		// 写入带调试字段的事件；ev.Tool 仅 tool_call 携带工具名，其余为空
		m.addEventDebug(session, "progress", ev.Agent, msg, ev.Kind, ev.Tool, "", "", "", success, prompt, inputTokens, outputTokens, ev.Detail)
	}
}

// parseTokenUsage 从 token_usage 消息中解析 in / out token 数
// 格式: "[caller] Token 消耗: in=N out=M dur=X"
// 返回值：in, out - 输入 / 输出 token 数。
func parseTokenUsage(msg string) (in, out int) {
	fmt.Sscanf(msg, "%*s Token 消耗: in=%d out=%d", &in, &out) // %*s 跳过 caller 前缀
	return
}

// CreateSession 创建并启动新会话。
// 职责：分配 session ID，初始化 Session 对象（含 system 消息），异步调用 runSession。
// 参数：ctx - 上下文（仅用于传递超时，不阻塞创建）；goal - 用户目标。
// 返回值：*Session - 创建的会话对象（已加入 sessions 映射，运行中状态）。
// 并发安全：mu.Lock 保护 sessions 写入。
func (m *SessionManager) CreateSession(ctx context.Context, goal string) *Session {
	sessionID := fmt.Sprintf("session-%d", m.seq.Add(1)) // 原子自增生成 ID

	session := &Session{
		ID:        sessionID,
		Goal:      goal,
		Status:    enums.SessionStatusRunning, // 初始状态为运行中
		StartedAt: time.Now(),
		Events:    make([]SessionEvent, 0), // 空事件流
		Messages: []types.ChatMessage{
			{Role: enums.ChatRoleSystem, Content: "Goal: " + goal, Timestamp: time.Now()},         // 注入 system 消息
			{Role: enums.ChatRoleUser, Content: goal, Timestamp: time.Now()},                       // 注入用户原始输入，供 TUI/前端对话区展示
		},
	}

	m.mu.Lock()
	m.sessions[sessionID] = session // 加入 sessions 映射
	m.mu.Unlock()

	// 创建可取消的 context，允许 HandleSessionCancel 终止 graph 执行
	runCtx, cancelFn := context.WithCancel(context.Background())
	session.cancelFn = cancelFn
	go m.runSession(runCtx, session) // 不阻塞 HTTP 请求

	return session
}

// GetSession 获取会话
// 参数：id - 会话 ID。
// 返回值：*Session - 找不到返回 nil。
func (m *SessionManager) GetSession(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id] // 找不到返回 nil
}

// LLMStats 返回所有运行中会话聚合的 LLM 调用统计（用于 /api/health）
// 返回值：callCount - 调用次数；timeoutCount - 超时次数；avgDur / maxDur - 平均 / 最长耗时（当前实现未填充，由调用方按需扩展）。
func (m *SessionManager) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, s := range m.sessions {
		for _, ev := range s.Events {
			if ev.Kind != "token_usage" {
				continue // 只统计 token_usage 事件
			}
			callCount++
			if strings.Contains(ev.Message, "timeout") || strings.Contains(ev.Message, "超时") {
				timeoutCount++ // 消息含 timeout / 超时 关键字
			}
		}
	}
	return
}

// ListSessions 列出所有会话
// 返回值：[]*Session - 所有会话指针的切片（拷贝，可安全遍历）。
func (m *SessionManager) ListSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Session, 0, len(m.sessions)) // 预分配容量
	for _, s := range m.sessions {
		result = append(result, s) // 拷贝指针
	}
	return result
}

// runSession 是会话执行的主循环（在独立 goroutine 中运行）。
// 职责：构建初始 ThreeLayerState，调用 graph.Invoke，根据结果更新 Session 状态，
// 写入 agent_done / system / stats 事件，最后持久化历史。
// 参数：ctx - 请求上下文；session - 待运行的会话。
// 副作用：异步运行；写入多个事件；更新 session.Status / Result / EndedAt。
func (m *SessionManager) runSession(ctx context.Context, session *Session) {
	// 全局超时10分钟：复杂任务（写游戏+运行+DB检查）需多轮 ReAct，5分钟不够
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	// 会话结束时清空 cancelFn，防止 HandleSessionCancel 对已完成会话误操作
	defer func() {
		m.mu.Lock()
		session.cancelFn = nil
		m.mu.Unlock()
	}()

	state := types.NewThreeLayerState(session.ID) // 创建初始图状态
	state.DomainGoal = session.Goal               // 注入用户目标

	m.addEvent(session, "system", "MetaAgent", "会话启动，目标: "+session.Goal, "", "", "", "", "", false)

	result, err := m.graph.Invoke(ctx, state) // 调用三层图
	if err != nil {
		// 失败：仅当会话仍为 running 时更新状态（可能已被 HandleSessionCancel 抢先设置）
		m.mu.Lock()
		if session.Status == enums.SessionStatusRunning {
			session.Status = enums.SessionStatusError
			session.Result = err.Error()
			now := time.Now()
			session.EndedAt = &now
		}
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
		return
	}

	// 特性5：人机对话挂起 — Graph 返回 ActionWait 且有待处理澄清请求时，
	// 保留运行态，等待用户通过 /api/sessions/{id}/clarify 提交答复后恢复执行。
	if result.NextAction == types.ActionWait && result.PendingClarify != nil {
		m.mu.Lock()
		session.Status = enums.SessionStatusAwaitingClarify
		session.State = result
		m.mu.Unlock()
		m.addEvent(session, "clarify", "MetaAgent",
			"请求用户澄清: "+result.PendingClarify.Question,
			"", "", "", "", "", false)
		return
	}

	// 成功：先暂存结果到 State，再检查 graph 运行期间是否收到用户指令
	m.mu.Lock()
	session.State = result
	session.Result = result.SessionSummary
	m.mu.Unlock()

	// 若在 graph.Invoke 执行期间有 interrupt/enqueue 到达，不标记完成，
	// 改为 resumeSession 继续处理队列中的用户指令，避免竞态丢失。
	if rt := m.graph.Runtime(); rt != nil && rt.CmdQueue != nil && rt.CmdQueue.HasPending(session.ID) {
		m.mu.Lock()
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		m.mu.Unlock()
		m.addEvent(session, "system", "MetaAgent", "检测到待处理用户指令，继续执行", "", "", "", "", "", false)
		go m.resumeSession(session)
		return
	}

	// 成功：更新状态、结果、最终 State
	m.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	now := time.Now()
	session.EndedAt = &now
	// 落最终助手回复到 Messages，供 TUI/前端对话区展示完整一问一答
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleAssistant,
		Content:   result.SessionSummary,
		Timestamp: now,
	})
	m.mu.Unlock()

	// 添加角色实例事件
	for _, inst := range m.registry.GetInstancesBySession(session.ID) {
		roleDef := m.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name // 取角色定义名称
		}
		m.addEvent(session, "agent_done", name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	m.addEvent(session, "system", "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)

	// 持久化会话历史（跨会话记忆基础）
	m.persistHistory(session)
	m.persistEvents(session)

	// 报告 LLM 统计
	if metaNode, ok := m.graph.GetNode("MetaAgent"); ok {
		if ma, ok := metaNode.(*graph.MetaAgentNode); ok {
			calls, timeouts, avg, max := ma.TimeoutStats()     // 超时统计
			inTotal, outTotal := ma.LLMTracker().TokenTotals() // token 总量
			if calls > 0 {
				m.addEvent(session, "stats", "System",
					fmt.Sprintf("LLM统计: 调用%d次, 超时%d次, 平均%v, 最长%v, 输入Token=%d, 输出Token=%d",
						calls, timeouts, avg.Round(time.Millisecond), max.Round(time.Millisecond), inTotal, outTotal),
					"", "", "", "", "", false)
			}
		}
	}

	// 会话完成后淘汰最早的已完成会话，防止长期运行 OOM
	m.evictCompletedSessions()
}

// persistHistory 把会话目标 / 总结 / 工具调用结果写入 session_history 表
// 职责：从 session.Events 过滤 tool_exec 事件，组装 SessionHistoryRecord，调用 pgStore 落库。
// 副作用：写 Postgres；失败仅记录日志，不影响主流程。
func (m *SessionManager) persistHistory(session *Session) {
	if m.pgStore == nil {
		return // 未配置 Postgres，跳过
	}
	toolResults := make([]map[string]any, 0, len(session.Events))
	for _, ev := range session.Events {
		if ev.Type != "tool_exec" {
			continue // 仅持久化 tool_exec 事件
		}
		toolResults = append(toolResults, map[string]any{
			"tool":   ev.Tool,
			"path":   ev.ToolPath,
			"output": truncate(ev.ToolOutput, 500), // 截断 500 字
			"error":  ev.ToolError,
			"ok":     ev.Success,
		})
	}
	rec := &store.SessionHistoryRecord{
		SessionID:   session.ID,
		Goal:        session.Goal,
		Summary:     session.Result, // 摘要长度由 MetaAgent 生成时限制（见 meta_agent.go 各 prompt）
		ToolResults: toolResults,
		CreatedAt:   time.Now(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second) // 持久化超时 3 秒
	defer cancel()
	if err := m.pgStore.SaveSessionHistory(ctx, rec); err != nil {
		log.Printf("[%s] 持久化会话历史失败: %v", session.ID, err) // 持久化失败仅日志
	}
}

// persistEvents 批量持久化会话事件流到 session_events 表。
func (m *SessionManager) persistEvents(session *Session) {
	if m.pgStore == nil {
		return
	}
	records := make([]store.SessionEventRecord, 0, len(session.Events))
	for _, ev := range session.Events {
		records = append(records, store.SessionEventRecord{
			SessionID:    session.ID,
			Type:         ev.Type,
			Agent:        ev.Agent,
			Message:      ev.Message,
			Kind:         ev.Kind,
			Tool:         ev.Tool,
			ToolPath:     ev.ToolPath,
			ToolOutput:   truncate(ev.ToolOutput, 2048),
			ToolError:    ev.ToolError,
			Success:      ev.Success,
			Timestamp:    ev.Timestamp,
			Prompt:       truncate(ev.Prompt, 2048),
			InputTokens:  ev.InputTokens,
			OutputTokens: ev.OutputTokens,
			DetailJSON:   ev.DetailJSON,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.pgStore.SaveSessionEvents(ctx, session.ID, records); err != nil {
		log.Printf("[%s] 持久化会话事件失败: %v", session.ID, err)
	}
}

// trimDebugEvents 从事件切片头部移除最多 maxDrop 条调试类事件（think/prompt/token_usage/graph_step），
// 保留 tool_exec / error / user_message / system / clarify / interrupt / enqueue 等关键事件。
// 若无可剔除的调试事件则返回原切片。
// 参数：events - 事件切片；maxDrop - 最多剔除条数。
// 返回值：[]SessionEvent - 裁剪后的事件切片。
func trimDebugEvents(events []SessionEvent, maxDrop int) []SessionEvent {
	dropped := 0
	out := make([]SessionEvent, 0, len(events))
	for _, ev := range events {
		debugKind := ev.Kind == "think" || ev.Kind == "prompt" || ev.Kind == "token_usage" || ev.Kind == "graph_step"
		if debugKind && dropped < maxDrop {
			dropped++
			continue
		}
		out = append(out, ev)
	}
	return out
}

// truncate 把字符串按 rune 截断到指定字数，超出加省略后缀。
// 按 rune 切片避免 UTF-8 多字节字符（如中文）被从中段截断产生乱码。
// 参数：s - 原始字符串；n - 最大 rune 数。
// 返回值：string - 截断后的字符串。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s // 未超长
	}
	return string(r[:n]) + "...(truncated)"
}

// addEvent 是 addEventDebug 的简化封装，省略调试字段。
// 职责：把事件追加到 session.Events 并输出日志。
func (m *SessionManager) addEvent(session *Session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool) {
	m.addEventDebug(session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError, success, "", 0, 0, "")
}

// addEventDebug 写入一个完整的 SessionEvent（含调试字段），并广播日志。
// 职责：构造 SessionEvent，追加到 session.Events，输出日志（暂未接 SSE，靠前端轮询）。
// 参数：见 SessionEvent 字段对应关系。
// 副作用：修改 session.Events。
// 并发安全：mu.Lock 保护 append。
func (m *SessionManager) addEventDebug(session *Session, eventType, agent, message, kind, tool, toolPath, toolOutput, toolError string, success bool, prompt string, inputTokens, outputTokens int, detailJSON string) {
	// 截断超长工具输出，防止单个事件膨胀数 MB
	if len(toolOutput) > 4096 {
		toolOutput = toolOutput[:4096] + "...(truncated)"
	}
	ev := SessionEvent{
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
	m.mu.Lock()
	session.Events = append(session.Events, ev) // 追加事件
	// 事件流裁剪：超 500 条时移除最早的 200 条调试类事件（think/prompt/token_usage/graph_step），
	// 保留 tool_exec / error / user_message / system / clarify / interrupt / enqueue 等关键事件
	if len(session.Events) > 500 {
		session.Events = trimDebugEvents(session.Events, 200)
	}
	m.mu.Unlock()

	// 广播SSE
	log.Printf("[%s] %s: %s", session.ID, agent, message) // 当前仅日志，SSE 由前端轮询模拟
}

// ---- HTTP 处理器 ----

// loadSessionEvents 从 session_events 表加载会话的完整事件流。
func (m *SessionManager) loadSessionEvents(ctx context.Context, sessionID string) []SessionEvent {
	if m.pgStore == nil {
		return make([]SessionEvent, 0)
	}
	records, _ := m.pgStore.GetSessionEvents(ctx, sessionID)
	events := make([]SessionEvent, 0, len(records))
	for _, r := range records {
		events = append(events, SessionEvent{
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

// extractMessagesFromEvents 从事件流中恢复对话消息。
func extractMessagesFromEvents(events []SessionEvent, goal, summary string) []types.ChatMessage {
	var msgs []types.ChatMessage
	for _, ev := range events {
		if ev.Type == "user_message" || ev.Type == "message" {
			msgs = append(msgs, types.ChatMessage{
				Role:      enums.ChatRoleUser,
				Content:   ev.Message,
				Timestamp: ev.Timestamp,
			})
		} else if ev.Agent != "User" && ev.Message != "" && (ev.Type == "agent_done" || ev.Type == "system") {
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

// HandleCreateSession 处理 POST /api/sessions，创建并启动新会话。
// 解析 goal 并创建会话，返回 Session 对象。
// 参数：w / r - HTTP 标准参数。
// 副作用：创建会话并异步启动 runSession。
func (m *SessionManager) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Goal string `json:"goal"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Goal == "" {
		http.Error(w, "目标 (goal) 不能为空", http.StatusBadRequest)
		return
	}

	session := m.CreateSession(context.Background(), req.Goal) // 创建并异步启动

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session) // 返回 Session JSON
}

// HandleGetSession 处理 GET /api/sessions/{id}，返回单个会话详情。
// 先查内存，未命中且配置了 Postgres 时回退到 session_history 表，返回最小记录。
func (m *SessionManager) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sessions/"):] // 截取 id
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	session := m.GetSession(id)
	if session == nil && m.pgStore != nil {
		// 内存已淘汰，从 DB 恢复最小记录（无 events 流）
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if rec, err := m.pgStore.GetSessionHistoryByID(ctx, id); err == nil && rec != nil {
			endedAt := rec.CreatedAt
			session = &Session{
				ID:        rec.SessionID,
				Goal:      rec.Goal,
				Status:    enums.SessionStatusCompleted,
				Result:    rec.Summary,
				StartedAt: rec.CreatedAt,
				EndedAt:   &endedAt,
				Events:    make([]SessionEvent, 0),
				Messages: []types.ChatMessage{
					{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
					{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
				},
			}
		}
	}
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// HandleListSessions 处理 GET /api/sessions，返回会话列表。
// 返回会话列表：内存中的运行中 + 最近完成会话，叠加 Postgres 中更早的历史会话。
// 内存未命中但 DB 有记录的会话以最小形态返回（goal/summary/result/time，无 events）。
func (m *SessionManager) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	memSessions := m.ListSessions() // 拷贝切片，已释放锁
	seen := make(map[string]bool, len(memSessions))
	for _, s := range memSessions {
		seen[s.ID] = true
	}

	var all []*Session
	all = append(all, memSessions...)

	if m.pgStore != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		recs, err := m.pgStore.RecentSessionHistories(ctx, 200)
		if err == nil {
			for _, rec := range recs {
				if seen[rec.SessionID] {
					continue // 内存已有，跳过 DB 版本
				}
				endedAt := rec.CreatedAt
				all = append(all, &Session{
					ID:        rec.SessionID,
					Goal:      rec.Goal,
					Status:    enums.SessionStatusCompleted,
					Result:    rec.Summary,
					StartedAt: rec.CreatedAt,
					EndedAt:   &endedAt,
					Events:    make([]SessionEvent, 0),
					Messages: []types.ChatMessage{
						{Role: enums.ChatRoleUser, Content: rec.Goal, Timestamp: rec.CreatedAt},
						{Role: enums.ChatRoleAssistant, Content: rec.Summary, Timestamp: rec.CreatedAt},
					},
				})
			}
		} else {
			log.Printf("列出会话失败(数据库回退): %v", err)
		}
	}

	// 按 StartedAt 倒序，最新在前
	sort.Slice(all, func(i, j int) bool {
		return all[i].StartedAt.After(all[j].StartedAt)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(all)
}

// HandleSessionBoard 处理 GET /api/sessions/{id}/board，返回任务看板。
// 返回该会话的 TaskBoard 快照（领域子任务、约束、状态）。若无则返回 null。
func (m *SessionManager) HandleSessionBoard(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/board") // 剥离 /board 后缀

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	var snap any
	if rt := m.graph.Runtime(); rt != nil && rt.Boards != nil {
		if b := rt.Boards.Get(id); b != nil {
			snap = b.Snapshot() // 取 TaskBoard 快照
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"board":      snap, // 没有则 nil
	})
}

// agentNode 是给前端用的扁平+树结构节点，含 RoleDefinition 名称与父子关系
type agentNode struct {
	InstID    string `json:"inst_id"`            // 实例 ID
	RoleDefID string `json:"role_def_id"`        // 角色定义 ID
	Name      string `json:"name"`               // 角色名称
	Type      string `json:"type"`               // 实例类型（meta / domain / subdomain / assistant）
	Domain    string `json:"domain"`             // 所属领域
	Status    string `json:"status"`             // 实例状态
	ParentID  string `json:"parent_id"`          // 父实例 ID（树结构）
	Goal      string `json:"goal,omitempty"`     // Block 目标
	BlockID   string `json:"block_id,omitempty"` // 所属 SessionBlock ID
}

// HandleSessionAgents 处理 GET /api/sessions/{id}/agents，返回会话内角色实例。
// 返回该会话所有 RoleInstance（带 RoleDefinition 名称），并附上对应 SessionBlock 的目标。
func (m *SessionManager) HandleSessionAgents(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/agents") // 剥离 /agents 后缀

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	// 收集 block 中 domain->goal 映射，便于按 domain 回填 goal
	blockGoalByDomain := make(map[string]string)
	blockIDByDomain := make(map[string]string)
	if session.State != nil {
		for _, b := range session.State.ActiveBlocks {
			blockGoalByDomain[b.Domain] = b.Goal // domain -> goal
			blockIDByDomain[b.Domain] = b.ID     // domain -> blockID
		}
	}

	instances := m.registry.GetInstancesBySession(id) // 查询所有实例
	nodes := make([]agentNode, 0, len(instances))
	for _, inst := range instances {
		name := "unknown"
		if def := m.registry.GetRoleDef(inst.RoleDefID); def != nil {
			name = def.Name // 取角色定义名
		}
		goal := ""
		blockID := ""
		if inst.Domain != "" {
			goal = blockGoalByDomain[inst.Domain]  // 按 domain 回填 goal
			blockID = blockIDByDomain[inst.Domain] // 按 domain 回填 blockID
		}
		nodes = append(nodes, agentNode{
			InstID:    inst.ID,
			RoleDefID: inst.RoleDefID,
			Name:      name,
			Type:      string(inst.Type),
			Domain:    inst.Domain,
			Status:    string(inst.Status),
			ParentID:  inst.ParentID,
			Goal:      goal,
			BlockID:   blockID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"agents":     nodes,
	})
}

// HandleSessionStream 处理 GET /api/sessions/{id}/stream，SSE 实时事件流。
// SSE 长连接：先推送当前 session 全量快照，再轮询增量事件，直到会话结束或客户端断开。
// 副作用：阻塞当前 goroutine 直到 session 结束或客户端断开。
func (m *SessionManager) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/sessions/"):]
	id = id[:len(id)-len("/stream")] // 剥离 /stream 后缀

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream") // SSE 头
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*") // 跨域

	flusher, ok := w.(http.Flusher) // 断言 Flusher 接口
	if !ok {
		http.Error(w, "不支持流式输出", http.StatusInternalServerError)
		return
	}

	// 先发送当前状态
	data, _ := json.Marshal(session)     // 序列化当前 session
	fmt.Fprintf(w, "data: %s\n\n", data) // 写入 SSE 帧
	flusher.Flush()

	// 轮询更新
	ticker := time.NewTicker(500 * time.Millisecond) // 500ms 轮询一次
	defer ticker.Stop()

	lastEventCount := len(session.Events) // 记录上次推送的事件数

	for {
		select {
		case <-ticker.C:
			session = m.GetSession(id) // 重新查询（可能已被回收）
			if session == nil {
				return
			}

			if len(session.Events) > lastEventCount {
				// 推送增量事件
				for _, ev := range session.Events[lastEventCount:] {
					data, _ := json.Marshal(ev)
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				lastEventCount = len(session.Events) // 更新水位
				flusher.Flush()
			}

			if session.Status != enums.SessionStatusRunning {
				// 推送 done 事件并退出
				data, _ := json.Marshal(map[string]string{"type": "done", "status": string(session.Status)})
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
				return
			}

		case <-r.Context().Done(): // 客户端断开
			return
		}
	}
}

// HandleSessionClarify 处理 POST /api/sessions/{id}/clarify，提交人机对话答复。
// 特性5：人机对话 — 用户答复 Agent 提出的澄清问题。
// 职责：将答复追加到会话消息，清空 PendingClarify，异步恢复 graph 执行。
// 副作用：修改 session.Messages / Status / State；异步启动 resumeSession。
func (m *SessionManager) HandleSessionClarify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/clarify")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Answer     string `json:"answer"`
		QuestionID string `json:"question_id"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Answer == "" {
		http.Error(w, "答复内容不能为空", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	if session.Status != enums.SessionStatusAwaitingClarify {
		m.mu.Unlock()
		http.Error(w, "会话未处于等待澄清状态", http.StatusBadRequest)
		return
	}
	// 追加用户答复到对话历史：前缀 "[澄清答复]" 让 LLM 在后续上下文中识别这是对悬停问题的回答
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   "[澄清答复] " + req.Answer,
		Timestamp: time.Now(),
	})
	// 清空 PendingClarify，避免 resumeSession 时被再次判定为挂起状态
	if session.State != nil {
		session.State.PendingClarify = nil
	}
	// 切回 running 让其他端点（interrupt/enqueue）知道会话已恢复可被抢占
	session.Status = enums.SessionStatusRunning
	// 直接 append 事件，避免 addEvent 再次取锁自死锁（此处已持 m.mu）
	session.Events = append(session.Events, SessionEvent{
		Type:      "clarify",
		Agent:     "User",
		Message:   "用户答复: " + req.Answer,
		Success:   true,
		Timestamp: time.Now(),
	})
	m.mu.Unlock()

	// 异步恢复：避免阻塞 HTTP 响应；graph 从最新 state 继续，可能再次 ActionWait
	go m.resumeSession(session)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"status":     "running",
	})
}

// HandleSessionInterrupt 处理 POST /api/sessions/{id}/interrupt，抢占中断。
// 特性6：抢占中断 — 用户暂停当前任务并以新指令重启。
// 职责：把新指令作为 IntentInterrupt 推入会话队列；若会话已完成则直接以新指令恢复执行。
// 副作用：写入 CmdQueue；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/interrupt")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	rt := m.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		http.Error(w, "命令队列不可用", http.StatusServiceUnavailable)
		return
	}
	// 先入队再判断会话状态，避免运行中会话被漏掉：MetaAgent 下个 tick Drain 时会拿到这条指令
	rt.CmdQueue.Push(id, cmdqueue.Item{Content: req.Content, Intent: cmdqueue.IntentInterrupt})

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	// 直接 append 事件，避免 addEvent 再次取锁自死锁
	session.Events = append(session.Events, SessionEvent{
		Type:      "interrupt",
		Agent:     "User",
		Message:   "抢占中断: " + req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束的会话需重新置 running 并清空 EndedAt，否则 resumeSession 会因状态不对跳过
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	// 已结束会话：异步恢复执行，由 drainCommandQueue 在首 tick 应用中断；
	// 运行中会话：不主动 resume，等 MetaAgent 下一个 tick 自然拉取队列
	if !wasRunning {
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionEnqueue 处理 POST /api/sessions/{id}/enqueue，队列注入消息。
// 特性6：队列注入 — 用户在任务执行中追加指令，不中断当前流程。
// 职责：把新指令作为 IntentEnqueue 推入会话队列；若会话已结束则按 /message 行为恢复。
// 副作用：写入 CmdQueue；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/enqueue")
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	rt := m.graph.Runtime()
	if rt == nil || rt.CmdQueue == nil {
		http.Error(w, "命令队列不可用", http.StatusServiceUnavailable)
		return
	}
	// 与 interrupt 同序：先入队，再判断是否需要 resume；enqueue 不重置上下文，仅追加消息
	rt.CmdQueue.Push(id, cmdqueue.Item{Content: req.Content, Intent: cmdqueue.IntentEnqueue})

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}
	// 直接 append 事件，避免 addEvent 再次取锁自死锁
	session.Events = append(session.Events, SessionEvent{
		Type:      "enqueue",
		Agent:     "User",
		Message:   "队列注入: " + req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})
	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束会话：enqueue 退化为普通 message 恢复，重新置 running
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	// 运行中会话：等 MetaAgent 下个 tick Drain；已结束会话：异步恢复
	if !wasRunning {
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "running"})
}

// HandleSessionCancel 处理 POST /api/sessions/{id}/cancel，终止运行中的会话。
// 调用 cancelFn 取消 graph 执行的 context，graph.Invoke 收到 ctx.Done() 后
// 应尽快退出。会话状态被设为 error，Result 记录取消原因。
func (m *SessionManager) HandleSessionCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/cancel")
	if id == "" {
		http.Error(w, "session id required", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if session.Status != enums.SessionStatusRunning {
		m.mu.Unlock()
		http.Error(w, "session is not running", http.StatusBadRequest)
		return
	}
	cancelFn := session.cancelFn
	session.cancelFn = nil
	session.Status = enums.SessionStatusError
	session.Result = "cancelled by user"
	now := time.Now()
	session.EndedAt = &now
	session.Events = append(session.Events, SessionEvent{
		Type:      "system",
		Agent:     "System",
		Message:   "会话已被用户取消",
		Success:   true,
		Timestamp: now,
	})
	m.mu.Unlock()

	if cancelFn != nil {
		cancelFn()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"session_id": id, "status": "error"})
}

// HandleSessionMessage 处理 POST /api/sessions/{id}/message，向会话追加用户消息。
// 向会话追加用户消息；若会话已结束则恢复执行。
// 副作用：修改 session.Messages / Status / EndedAt；可能异步启动 resumeSession。
func (m *SessionManager) HandleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不被允许", http.StatusMethodNotAllowed)
		return
	}

	id := r.URL.Path[len("/api/sessions/"):]
	id = id[:len(id)-len("/message")] // 剥离 /message 后缀
	if id == "" {
		http.Error(w, "缺少会话 ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := DecodeJSONRequest(r.Body, &req); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		http.Error(w, "内容不能为空", http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		// 会话不在内存中 — 从数据库恢复完整会话（含工具调用过程）。
		revived := m.reviveFromHistory(id)
		if revived == nil {
			http.Error(w, "会话不存在", http.StatusNotFound)
			return
		}
		m.mu.Lock()
		session = revived
	} else if session.Status == enums.SessionStatusCompleted && len(session.Events) == 0 && session.Result != "" {
		// 启动时 RestoreSessions 加载的最小历史会话（无 Events），
		// 续话前需替换为完整版本（含工具调用过程），否则 LLM 看不到历史工具结果会重复调用。
		m.mu.Unlock()
		revived := m.reviveFromHistory(id)
		m.mu.Lock()
		if revived != nil {
			session = revived
		}
	}

	// 追加用户消息与事件（直接 append，避免调用 addEvent 再次取锁自死锁）
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   req.Content,
		Timestamp: time.Now(),
	})
	session.Events = append(session.Events, SessionEvent{
		Type:      "user_message",
		Agent:     "User",
		Message:   req.Content,
		Success:   true,
		Timestamp: time.Now(),
	})

	wasRunning := session.Status == "running"
	if !wasRunning {
		// 已结束会话：重新激活并异步恢复
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
	}
	m.mu.Unlock()

	if !wasRunning {
		go m.resumeSession(session)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// reviveFromHistory 从 session_history 表加载会话并插入 m.sessions，
// 恢复完整对话上下文（含工具调用过程），以便后续续话执行。
// 调用方不得持有 m.mu；若数据库中也不存在则返回 nil。
//
// 恢复内容：
//   - Events: 把 ToolResults 还原为 tool_exec 事件，前端可回放工具调用过程
//   - Messages: user(goal) → 每个工具调用拼成 assistant 消息 → assistant(summary)
//
// 注意：ToolResults 中的 output 在持久化时已截断到 500 字（见 persistHistory），
// 属于正常压缩，续话时 LLM 看到的是工具结果摘要而非完整原文。
func (m *SessionManager) reviveFromHistory(id string) *Session {
	if m.pgStore == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec, err := m.pgStore.GetSessionHistoryByID(ctx, id)
	if err != nil || rec == nil {
		return nil
	}
	endedAt := rec.CreatedAt

	// 还原 Events：每条 ToolResult → 一个 tool_exec 事件
	events := make([]SessionEvent, 0, len(rec.ToolResults)+2)
	for _, tr := range rec.ToolResults {
		tool, _ := tr["tool"].(string)
		path, _ := tr["path"].(string)
		output, _ := tr["output"].(string)
		toolErr, _ := tr["error"].(string)
		ok, _ := tr["ok"].(bool)
		events = append(events, SessionEvent{
			Type:       "tool_exec",
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

	// 还原 Messages：user(goal) → 工具调用序列（assistant）→ assistant(summary)
	msgs := make([]types.ChatMessage, 0, len(rec.ToolResults)+2)
	msgs = append(msgs, types.ChatMessage{
		Role:      enums.ChatRoleUser,
		Content:   rec.Goal,
		Timestamp: rec.CreatedAt,
	})
	for _, tr := range rec.ToolResults {
		tool, _ := tr["tool"].(string)
		path, _ := tr["path"].(string)
		output, _ := tr["output"].(string)
		toolErr, _ := tr["error"].(string)
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

	session := &Session{
		ID:        rec.SessionID,
		Goal:      rec.Goal,
		Status:    enums.SessionStatusCompleted,
		Result:    rec.Summary,
		StartedAt: rec.CreatedAt,
		EndedAt:   &endedAt,
		Events:    events,
		Messages:  msgs,
	}
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	return session
}

// summarizeHistoryForGoal 用轻量模型把历史对话总结为清晰的目标描述。
// 设计意图：避免把 raw "role: content" 文本直接作为 DomainGoal 让 MetaAgent 处理，
// 否则 MetaAgent 会把整段历史当成新目标，容易误判任务边界、重复拆分领域。
// 调轻量模型把历史压缩为"用户当前想做什么"的一句话目标，让续话路径与新建会话一致。
//
// 参数：
//   - ctx: 上下文（含超时）
//   - sessionID: 会话 ID（仅用于日志与错误定位）
//   - history: 历史消息拼接文本（role: content 形式）
//
// 返回：
//   - string: 总结后的目标
//   - error: 模型不可用 / 调用失败 / 超时 / 空结果时返回，视为系统级故障，调用方应中止续话
//
// 容错策略：模型调用是续话的前置依赖，失败即系统级问题，不回退 raw history。
// 空历史直接返回空串不算错误（无需总结）。
func (m *SessionManager) summarizeHistoryForGoal(ctx context.Context, sessionID, history string) (string, error) {
	if strings.TrimSpace(history) == "" {
		return "", nil // 空历史无需总结，非错误
	}
	if m.modelFactory == nil {
		return "", fmt.Errorf("modelFactory not injected: lightweight model unavailable for session %s", sessionID)
	}
	client, err := m.modelFactory.GetLightweightModel(ctx)
	if err != nil {
		return "", fmt.Errorf("get lightweight model for session %s: %w", sessionID, err)
	}
	prompt := fmt.Sprintf(`你是会话续接助手。请基于以下历史对话，提炼出用户当前想要完成的核心目标。
要求：
1. 用一句话（不超过 200 字）描述目标
2. 保留关键上下文（涉及的文件/领域/已尝试的方案）
3. 不要复述历史，只输出目标本身
4. 不要加任何前缀或解释

历史对话：
%s

用户当前目标：`, history)
	// 轻量模型总结独立超时 30 秒，避免阻塞续话主流程
	summaryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.Generate(summaryCtx, prompt)
	if err != nil {
		return "", fmt.Errorf("lightweight summary for session %s failed: %w", sessionID, err)
	}
	resp = strings.TrimSpace(resp)
	if resp == "" {
		return "", fmt.Errorf("lightweight summary for session %s returned empty response", sessionID)
	}
	return resp, nil
}

// resumeSession 基于历史消息恢复会话执行
// 职责：用轻量模型总结历史消息为目标，调用 graph.Invoke 续跑，更新状态并持久化。
// 参数：session - 待恢复的会话（Status 已被设为 running）。
// 副作用：异步运行；可能调用轻量模型；写入 system / agent_done / error 事件；更新 session 状态。
func (m *SessionManager) resumeSession(session *Session) {
	ctx, rootCancel := context.WithCancel(context.Background())
	session.cancelFn = rootCancel
	ctx, timeoutCancel := context.WithTimeout(ctx, 10*time.Minute) // 恢复也用 10 分钟超时
	defer timeoutCancel()
	// 会话结束时清空 cancelFn，防止 HandleSessionCancel 对已完成会话误操作
	defer func() {
		m.mu.Lock()
		session.cancelFn = nil
		m.mu.Unlock()
	}()

	// 构建对话上下文（最近20条消息）
	var history strings.Builder
	start := 0
	if len(session.Messages) > 20 {
		start = len(session.Messages) - 20 // 仅取最后 20 条
	}
	for _, msg := range session.Messages[start:] {
		fmt.Fprintf(&history, "%s: %s\n", msg.Role, msg.Content) // 拼成 role: content 文本
	}

	// 用轻量模型把历史对话总结为清晰的目标描述，避免直接塞 raw history 让 MetaAgent 误判
	// 模型调用失败视为系统级故障，中止续话并将会话置为 error
	m.addEvent(session, "llm", "LightweightModel",
		fmt.Sprintf("续话：调用轻量模型总结历史对话 (%d 字符)", history.Len()), "", "", "", "", "", false)
	goal, err := m.summarizeHistoryForGoal(ctx, session.ID, history.String())
	if err != nil {
		m.mu.Lock()
		session.Status = enums.SessionStatusError
		session.Result = err.Error()
		now := time.Now()
		session.EndedAt = &now
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "续话失败（轻量模型不可用）: "+err.Error(), "", "", "", "", "", false)
		log.Printf("[%s] 续话失败: %v", session.ID, err)
		return
	}
	m.addEvent(session, "think", "LightweightModel",
		"续话：历史对话已总结为目标: "+goal, "", "", "", "", "", false)

	state := types.NewThreeLayerState(session.ID)
	state.DomainGoal = goal               // 轻量模型总结后的目标
	state.SessionSummary = session.Result // 携带之前的摘要
	state.Messages = session.Messages     // 传递消息流

	m.addEvent(session, "system", "MetaAgent", "继续会话，新消息已纳入上下文", "", "", "", "", "", false)

	result, err := m.graph.Invoke(ctx, state)
	if err != nil {
		// 失败：仅当会话仍为 running 时更新状态（可能已被 HandleSessionCancel 抢先设置）
		m.mu.Lock()
		if session.Status == enums.SessionStatusRunning {
			session.Status = enums.SessionStatusError
			session.Result = err.Error()
			now := time.Now()
			session.EndedAt = &now
		}
		m.mu.Unlock()
		m.addEvent(session, "error", "System", "执行失败: "+err.Error(), "", "", "", "", "", false)
		return
	}

	// 特性5：人机对话挂起 — 恢复路径同样支持再次挂起
	if result.NextAction == types.ActionWait && result.PendingClarify != nil {
		m.mu.Lock()
		session.Status = enums.SessionStatusAwaitingClarify
		session.State = result
		m.mu.Unlock()
		m.addEvent(session, "clarify", "MetaAgent",
			"请求用户澄清: "+result.PendingClarify.Question,
			"", "", "", "", "", false)
		return
	}

	// 成功：暂存结果到 State，检查是否有新的用户指令到达
	m.mu.Lock()
	session.State = result
	session.Result = result.SessionSummary
	m.mu.Unlock()

	if rt := m.graph.Runtime(); rt != nil && rt.CmdQueue != nil && rt.CmdQueue.HasPending(session.ID) {
		m.mu.Lock()
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		m.mu.Unlock()
		m.addEvent(session, "system", "MetaAgent", "检测到待处理用户指令，继续执行", "", "", "", "", "", false)
		go m.resumeSession(session)
		return
	}

	// 成功：更新状态、结果、最终 State，并追加助手回复
	m.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	now := time.Now()
	session.EndedAt = &now
	session.Messages = append(session.Messages, types.ChatMessage{
		Role:      enums.ChatRoleAssistant,
		Content:   result.SessionSummary,
		Timestamp: now,
	})
	m.mu.Unlock()

	// 写入每个角色实例的完成事件
	for _, inst := range m.registry.GetInstancesBySession(session.ID) {
		roleDef := m.registry.GetRoleDef(inst.RoleDefID)
		name := "unknown"
		if roleDef != nil {
			name = roleDef.Name
		}
		m.addEvent(session, "agent_done", name, fmt.Sprintf("类型: %s, 领域: %s, 状态: %s", inst.Type, inst.Domain, inst.Status), "", "", "", "", "", false)
	}

	m.addEvent(session, "system", "MetaAgent", "会话完成: "+result.SessionSummary, "", "", "", "", "", false)
	m.persistHistory(session)
	m.persistEvents(session)  // 持久化历史+事件
	m.evictCompletedSessions() // 淘汰旧会话，防 OOM
}

// HandleSessionMetrics GET /api/sessions/{id}/metrics — 会话级 LLM 统计
// 职责：聚合该会话所有 token_usage 事件，输出调用次数、超时次数、平均 / 最长耗时、token 总量。
func (m *SessionManager) HandleSessionMetrics(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/metrics")

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	var calls, timeouts, inputTokens, outputTokens int
	var maxDur time.Duration
	var totalDur time.Duration
	for _, ev := range session.Events {
		if ev.Kind != "token_usage" {
			continue // 仅统计 token_usage
		}
		calls++
		inputTokens += ev.InputTokens
		outputTokens += ev.OutputTokens
		if strings.Contains(ev.Message, "timeout") || strings.Contains(ev.Message, "超时") {
			timeouts++
		}
		// token_usage 消息格式: "[caller] Token 消耗: in=N out=M dur=X"
		var dur time.Duration
		fmt.Sscanf(ev.Message, "%*s Token 消耗: in=%*d out=%*d dur=%v", &dur) // 解析 dur
		totalDur += dur
		if dur > maxDur {
			maxDur = dur // 记录最长
		}
	}
	avgDur := time.Duration(0)
	if calls > 0 {
		avgDur = totalDur / time.Duration(calls) // 计算平均
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id":    id,
		"calls":         calls,
		"timeouts":      timeouts,
		"avg_duration":  avgDur.Round(time.Millisecond).String(),
		"max_duration":  maxDur.Round(time.Millisecond).String(),
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
	})
}

// HandleSessionWatchdog GET /api/sessions/{id}/watchdog — 看门狗历史决策
// 职责：从全局 Watchdog 历史中过滤出本会话相关的决策（按 blockID 匹配 active / completed blocks），返回决策列表。
func (m *SessionManager) HandleSessionWatchdog(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/watchdog")

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	var decisions []map[string]any
	if m.graph.Runtime() != nil && m.graph.Runtime().Watchdog != nil {
		for _, d := range m.graph.Runtime().Watchdog.History() {
			// Watchdog.Check 使用 blockID 作为 agentID
			if session.State == nil || session.State.ActiveBlocks[d.AgentID] == nil {
				// 也匹配已完成 block
				found := false
				for _, bid := range session.State.CompletedBlocks {
					if bid == d.AgentID {
						found = true // 命中已完成 block
						break
					}
				}
				if !found {
					continue // 既不在 active 也不在 completed，跳过
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"decisions":  decisions,
	})
}

// HandleSessionMailbox GET /api/sessions/{id}/mailbox — 会话内 Agent 未读邮件
// 职责：枚举该会话所有 RoleInstance 的邮箱，去重后合并广播桶消息，返回邮件列表。
// 副作用：DrainBroadcast 会消费广播桶（Peek 不消费点对点邮件）。
func (m *SessionManager) HandleSessionMailbox(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	id = strings.TrimSuffix(id, "/mailbox")

	session := m.GetSession(id)
	if session == nil {
		http.Error(w, "会话不存在", http.StatusNotFound)
		return
	}

	var msgs []map[string]any
	if rt := m.graph.Runtime(); rt != nil && rt.Mailbox != nil {
		instances := m.registry.GetInstancesBySession(id)
		seen := make(map[string]bool) // 邮件去重集合
		for _, inst := range instances {
			for _, msg := range rt.Mailbox.Peek(inst.ID) { // Peek 不消费
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
		// 合并广播桶
		for _, msg := range rt.Mailbox.DrainBroadcast() { // DrainBroadcast 会消费广播桶
			if seen[msg.ID] {
				continue
			}
			seen[msg.ID] = true
			msgs = append(msgs, map[string]any{
				"id":         msg.ID,
				"from":       msg.From,
				"to":         "*", // 广播邮件 to 字段统一为 *
				"type":       msg.Type,
				"subject":    msg.Subject,
				"body":       msg.Body,
				"priority":   msg.Priority,
				"status":     msg.Status,
				"created_at": msg.CreatedAt,
				"broadcast":  true, // 标记为广播邮件
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session_id": id,
		"messages":   msgs,
	})
}
