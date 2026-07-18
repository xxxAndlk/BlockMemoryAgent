// Package agent 提供基于 ReAct 引擎的 Agent 服务实现，
// 负责会话生命周期管理与外部接口适配。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
)

// ReactService 是基于 ReAct（Reasoning + Acting）引擎的 Agent 接口实现。
// 它持有内存中的会话存储，并将实际执行委托给 ReActAgent。
type ReactService struct {
	store        *reactSessionStore  // 内存会话存储，管理会话生命周期与事件
	roleRegistry *role.Registry      // 角色注册表，用于获取 meta 角色等配置
	modelFactory *model.ModelFactory // 模型工厂，负责构造大模型调用 provider
	toolRegistry *tool.Registry      // 工具注册表，提供 ReAct 可调用的工具
	mailbox      *mailbox.Mailbox    // 邮箱，用于跨组件消息通知
	memory       MemoryPipeline      // 记忆管道，负责会话记忆的写入与查询
	runtimeCfg   ReactRuntimeConfig  // ReAct 主循环运行时参数（轮数/超时/重试/历史滑窗）

	// testProvider 是包内部测试使用的钩子，
	// 允许单元测试注入 mock 的 ModelProvider，从而无需真实 API 密钥即可运行 ReAct 循环。
	testProvider ModelProvider
}

// ReactRuntimeConfig 是 ReAct 引擎的运行时参数快照。
// 由 bootstrap 从 cfg.Agent 派生注入，避免 agent 包反向依赖 config 包；
// 未注入时全部取零值，由 loopConfig 回退到合理默认值。
type ReactRuntimeConfig struct {
	MaxIterations           int // ReAct 最大 LLM 轮数；<0 表示不限制
	LLMTimeoutSec           int // 单次 LLM 调用超时（秒）；<0 表示仅受会话取消控制
	RetryCount              int // LLM 失败重试次数（不含首次）
	RetryBackoffMs          int // 重试初始退避（毫秒）
	HistoryMaxMessages      int // 单次请求最大历史消息数；<0 表示不裁剪
	ToolOutputHistoryMaxRunes int // 写入历史的工具输出最大字符数；<0 表示不截断
}

// SetRuntimeConfig 注入 ReAct 主循环运行时参数（见 ReactRuntimeConfig）。
func (s *ReactService) SetRuntimeConfig(c ReactRuntimeConfig) {
	s.runtimeCfg = c
}

// LoopConfig 把服务级配置映射为 ReActAgent 的 LoopConfig：
// 负数（配置语义"不限制"）归一为 0（agent 语义"不启用该限制"），
// 零值（未注入配置）回退到与旧行为一致的默认值。
func (c ReactRuntimeConfig) LoopConfig() LoopConfig {
	lc := LoopConfig{
		MaxIterations:      50,
		LLMTimeout:         300 * time.Second,
		RetryCount:         3,
		RetryBackoff:       100 * time.Millisecond,
		HistoryMaxMessages: 40,
		ToolOutputMaxRunes: 2000,
	}
	if c.MaxIterations != 0 {
		lc.MaxIterations = max(c.MaxIterations, 0)
	}
	if c.LLMTimeoutSec != 0 {
		lc.LLMTimeout = time.Duration(max(c.LLMTimeoutSec, 0)) * time.Second
	}
	if c.RetryCount != 0 {
		lc.RetryCount = max(c.RetryCount, 0)
	}
	if c.RetryBackoffMs != 0 {
		lc.RetryBackoff = time.Duration(max(c.RetryBackoffMs, 0)) * time.Millisecond
	}
	if c.HistoryMaxMessages != 0 {
		lc.HistoryMaxMessages = max(c.HistoryMaxMessages, 0)
	}
	if c.ToolOutputHistoryMaxRunes != 0 {
		lc.ToolOutputMaxRunes = max(c.ToolOutputHistoryMaxRunes, 0)
	}
	return lc
}

// SetModelProvider 注入一个 mock 或替代的模型 provider。
// 主要用于需要在无真实 API 密钥情况下运行 ReAct 循环的测试场景。
func (s *ReactService) SetModelProvider(p ModelProvider) {
	s.testProvider = p
}

// SetLogger 注入结构化日志器，使会话存储的错误类日志以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log。
func (s *ReactService) SetLogger(l *logger.Logger) {
	s.store.setLogger(l)
}

// NewReactService 创建 ReactService，并注入 ReAct 引擎运行时所需的所有依赖。
//
// 参数说明：
//   - roleRegistry: 角色注册表；
//   - modelFactory: 模型工厂；
//   - toolRegistry: 工具注册表；
//   - mailbox: 消息邮箱；
//   - memory: 记忆管道；
//   - pgStore: PostgreSQL 持久化存储，用于历史会话读写。
func NewReactService(
	roleRegistry *role.Registry,
	modelFactory *model.ModelFactory,
	toolRegistry *tool.Registry,
	mailbox *mailbox.Mailbox,
	memory MemoryPipeline,
	pgStore *store.PostgresStore,
) *ReactService {
	// 初始化 ReactService 实例，并构建新的内存会话存储。
	s := &ReactService{
		store:        newReactSessionStore(),
		roleRegistry: roleRegistry,
		modelFactory: modelFactory,
		toolRegistry: toolRegistry,
		mailbox:      mailbox,
		memory:       memory,
	}
	// 将 PostgreSQL 存储与模型工厂注入会话存储，用于持久化与恢复。
	s.store.setPostgresStore(pgStore)
	s.store.setModelFactory(modelFactory)
	// 如果工具注册表存在，则注册进度回调，
	// 这样工具执行过程中产生的事件可以回流到对应会话。
	if toolRegistry != nil {
		toolRegistry.SetProgressCallback(s.handleToolEvent)
	}
	return s
}

// CreateSession 为指定目标创建一个新的 ReAct 会话，并异步启动 ReAct 主循环。
func (s *ReactService) CreateSession(ctx context.Context, req CreateRequest) (*Session, error) {
	// 在内存中创建会话对象。
	sess := s.store.createSession(req.Goal)
	// 在独立 goroutine 中运行 ReAct 循环，避免阻塞调用方。
	go s.runSession(sess)
	// 返回转换后的公共 Session DTO。
	return toReactAgentSession(sess), nil
}

// Get 根据会话 ID 获取会话。
// 如果会话已从内存中淘汰，则回退到 PostgreSQL 历史记录中查找。
func (s *ReactService) Get(ctx context.Context, sessionID string) (*Session, error) {
	// 优先从内存快照中查找会话。
	sess := s.store.snapshotSessionByID(sessionID)
	if sess != nil {
		return toReactAgentSession(sess), nil
	}
	// 内存未命中且存在 Postgres 存储时，查询历史记录。
	if s.store.pgStore != nil {
		// 设置 3 秒超时，避免外部存储故障导致长时间阻塞。
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		rec, err := s.store.pgStore.GetSessionHistoryByID(ctx, sessionID)
		// 查询成功且记录存在时，将历史记录转换为 Session DTO。
		if err == nil && rec != nil {
			return &Session{
				ID:     rec.SessionID,
				Goal:   rec.Goal,
				Status: string(enums.SessionStatusCompleted),
				Result: rec.Summary,
				// 历史会话没有准确的结束时间，使用创建时间占位。
				StartedAt: rec.CreatedAt,
				EndedAt:   rec.CreatedAt,
				Events:    make([]Event, 0),
				Messages: []Message{
					{Role: string(enums.ChatRoleUser), Content: rec.Goal, Timestamp: rec.CreatedAt},
					{Role: string(enums.ChatRoleAssistant), Content: rec.Summary, Timestamp: rec.CreatedAt},
				},
			}, nil
		}
	}
	// 既不在内存也不在历史记录中，返回未找到错误。
	return nil, ErrSessionNotFound
}

// List 返回内存中符合过滤条件的会话列表。
func (s *ReactService) List(ctx context.Context, filter Filter) ([]*Session, error) {
	// 获取所有内存会话。
	all := s.store.listSessions()
	// 预分配输出切片，容量与总数一致。
	out := make([]*Session, 0, len(all))
	// 遍历会话并应用过滤条件。
	for _, sess := range all {
		// 如果指定了状态过滤且状态不匹配，则跳过。
		if filter.Status != "" && string(sess.Status) != filter.Status {
			continue
		}
		// 将内部会话转换为公共 DTO 并追加到结果。
		out = append(out, toReactAgentSession(sess))
		// 如果达到数量上限，提前结束遍历。
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// Send 向指定会话投递一条用户消息。
func (s *ReactService) Send(ctx context.Context, sessionID string, msg Message) error {
	return s.sendMessage(ctx, sessionID, msg.Content)
}

// ResumeSession 继续一个之前已结束或暂停的会话。
// 如果请求中携带了 CarryOver 或 UserInput，会将其作为用户输入发送给会话。
func (s *ReactService) ResumeSession(ctx context.Context, sessionID string, req ResumeRequest) (*Session, error) {
	// 只要存在延续内容或用户输入，就尝试发送消息。
	if req.CarryOver != "" || req.UserInput != "" {
		content := req.UserInput
		// 优先使用 UserInput；若为空则退回到 CarryOver。
		if content == "" {
			content = req.CarryOver
		}
		if err := s.sendMessage(ctx, sessionID, content); err != nil {
			return nil, err
		}
	}
	// 再次获取会话快照并返回。
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	return toReactAgentSession(sess), nil
}

// Stream 返回指定会话的实时事件流通道。
func (s *ReactService) Stream(ctx context.Context, sessionID string) (<-chan Event, error) {
	// 如果会话不存在，直接返回错误。
	if s.store.snapshotSessionByID(sessionID) == nil {
		return nil, ErrSessionNotFound
	}

	// 创建带缓冲的输出通道，降低发送阻塞。
	out := make(chan Event, 16)
	// 在独立 goroutine 中持续轮询会话事件并推送到通道。
	go func() {
		defer close(out)
		// 创建 100 毫秒的轮询 ticker。
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		// seen 记录已经推送过的事件数量，避免重复发送。
		seen := 0

		for {
			select {
			case <-ctx.Done():
				// 调用方上下文取消时，结束事件流。
				return
			case <-ticker.C:
				// 每次 ticker 触发时拉取会话快照。
				sess := s.store.snapshotSessionByID(sessionID)
				if sess == nil {
					// 会话已销毁，结束事件流。
					return
				}
				// 推送自 seen 以来的所有新事件。
				for i := seen; i < len(sess.Events); i++ {
					select {
					case out <- *toAgentEvent(&sess.Events[i]):
					case <-ctx.Done():
						return
					}
				}
				// 更新已推送位置。
				seen = len(sess.Events)
				// 如果会话已不在运行或等待澄清状态，等待短暂时间后结束，
				// 确保客户端收到最终的收尾事件。
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

// Query 回答关于会话的只读查询。
func (s *ReactService) Query(ctx context.Context, sessionID string, q Query) (Result, error) {
	// 根据查询类型分发处理。
	switch q.Kind {
	case QueryKindSessionCount:
		// 返回内存中的会话数量。
		return Result{Data: s.store.sessionCount()}, nil
	case QueryKindLLMStats:
		// 返回 LLM 调用统计：总调用数、超时数、平均耗时、最大耗时。
		calls, timeouts, avg, max := s.store.llmStats()
		return Result{Data: map[string]any{
			"calls":        calls,
			"timeouts":     timeouts,
			"avg_duration": avg.Round(time.Millisecond).String(),
			"max_duration": max.Round(time.Millisecond).String(),
		}}, nil
	default:
		// 未知查询类型返回空结果。
		return Result{}, nil
	}
}

// Control 向会话发送操作指令。
func (s *ReactService) Control(ctx context.Context, sessionID string, cmd ControlCommand) error {
	// 根据操作类型分发到对应处理函数。
	switch cmd.Op {
	case ControlOpMessage:
		// 普通消息：提取 content 并发送。
		content, _ := cmd.Args["content"].(string)
		return s.sendMessage(ctx, sessionID, content)
	case ControlOpClarify:
		// 澄清答复：提取 answer 并答复。
		answer, _ := cmd.Args["answer"].(string)
		return s.answerClarify(ctx, sessionID, answer)
	case ControlOpInterrupt:
		// 中断：提取 content 并触发中断处理。
		content, _ := cmd.Args["content"].(string)
		return s.interrupt(ctx, sessionID, content)
	case ControlOpEnqueue:
		// 队列注入：提取 content 并注入会话。
		content, _ := cmd.Args["content"].(string)
		return s.enqueue(ctx, sessionID, content)
	case ControlOpCancel:
		// 取消会话。
		return s.cancel(ctx, sessionID)
	case ControlOpTopic:
		// 话题切换：提取 name 与 goal 并切换话题。
		name, _ := cmd.Args["name"].(string)
		goal, _ := cmd.Args["goal"].(string)
		_, err := s.SwitchTopic(ctx, sessionID, name, goal)
		return err
	default:
		// 未知操作返回错误。
		return fmt.Errorf("unknown control op: %s", cmd.Op)
	}
}

// ListAgents 返回与会话关联的运行时 Agent 实例列表。
// 在 ReAct 重构期间，这里返回单个 MetaAgent 节点，以保持 TUI 树形面板继续渲染；
// 后续阶段将根据子 Agent 事件流构建完整树。
func (s *ReactService) ListAgents(ctx context.Context, sessionID string) ([]AgentInstance, error) {
	// 获取会话快照，确认会话存在。
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return nil, ErrSessionNotFound
	}
	// 默认状态为活跃；若会话已完成或出错，则标记为结束。
	status := string(enums.RoleStatusActive)
	if sess.Status == enums.SessionStatusCompleted || sess.Status == enums.SessionStatusError {
		status = string(enums.RoleStatusDone)
	}
	return []AgentInstance{
		{
			Name:     "MetaAgent",
			Role:     "meta",
			RoleType: enums.RoleTypeMeta,
			Status:   status,
		},
	}, nil
}

// Shutdown 取消所有正在运行的会话。
func (s *ReactService) Shutdown(ctx context.Context) error {
	s.store.shutdown()
	return nil
}

// SummarizeTaskTitle 为长任务标题生成一个简短的展示标题。
func (s *ReactService) SummarizeTaskTitle(ctx context.Context, title string) string {
	// 如果模型工厂不可用，直接返回原标题。
	if s.store.modelFactory == nil {
		return title
	}
	// 构造压缩提示词，要求模型输出 40 字以内、保留核心动作与对象的任务名。
	prompt := fmt.Sprintf("将以下任务描述压缩成 40 字以内的简短任务名，保留核心动作与对象，不要解释：\n%s", title)
	// 设置 3 秒超时，避免摘要生成拖垮接口。
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// 调用轻量模型生成摘要。
	brief, err := s.store.modelFactory.CallLightweightWithRetry(callCtx, prompt)
	if err != nil || strings.TrimSpace(brief) == "" {
		return title
	}
	// 去除首尾空白与常见引号、括号等装饰字符。
	brief = strings.TrimSpace(brief)
	brief = strings.Trim(brief, "\"'"+"`「」【】()")
	// 按 rune 截断到 40 字，避免多字节字符被截断。
	if len([]rune(brief)) > 40 {
		brief = string([]rune(brief)[:40]) + "…"
	}
	return brief
}

// LaunchSession 实现 dag.SessionLauncher 接口。
func (s *ReactService) LaunchSession(goal string) string {
	// 创建会话并异步启动 ReAct 循环。
	sess := s.store.createSession(goal)
	go s.runSession(sess)
	return sess.ID
}

// RestoreSessions 从 PostgreSQL 加载历史会话到内存。
func (s *ReactService) RestoreSessions(ctx context.Context, limit int) int {
	return s.store.restoreSessions(ctx, limit)
}

// ClearSessionChat 清除会话的聊天记录，但保留初始系统/用户消息。
func (s *ReactService) ClearSessionChat(id string) bool {
	return s.store.clearSessionChat(id)
}

// SessionCount 返回当前内存中持有的会话数量。
func (s *ReactService) SessionCount() int {
	return s.store.sessionCount()
}

// LLMStats 返回聚合的 LLM 调用统计信息。
func (s *ReactService) LLMStats() (callCount, timeoutCount int, avgDur, maxDur time.Duration) {
	return s.store.llmStats()
}

// SwitchTopic 切换会话的当前话题。
// 对于运行中的会话，记录话题切换事件；
// 对于非运行中的会话，创建新会话并返回。
func (s *ReactService) SwitchTopic(ctx context.Context, sessionID, name, goal string) (*Session, error) {
	// 话题名称不能为空。
	if name == "" {
		return nil, fmt.Errorf("name cannot be empty")
	}
	// 若未提供目标，则默认使用话题名称作为目标。
	if goal == "" {
		goal = name
	}

	// 加锁访问会话映射，查找目标会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return nil, ErrSessionNotFound
	}

	// 记录切换前是否处于运行状态，随后解锁。
	wasRunning := session.Status == enums.SessionStatusRunning
	s.store.mu.Unlock()

	// 添加话题切换事件，供前端展示。
	s.store.addEvent(session, eventkind.Progress, "User", fmt.Sprintf("切换话题: 到 [%s]", name), eventkind.TopicSwitch, "", "", "", "", true)

	// 若会话仍在运行，返回当前会话快照；否则创建新会话承载新话题。
	if wasRunning {
		return toReactAgentSession(s.store.snapshotSessionByID(sessionID)), nil
	}

	return toReactAgentSession(s.store.createSession(goal)), nil
}

// handleToolEvent 接收工具注册表产生的进度事件，并将其注入到对应运行中会话的事件流。
func (s *ReactService) handleToolEvent(ctx context.Context, ev tool.ProgressEvent) {
	// 加读锁判断会话是否存在且处于运行状态。
	s.store.mu.RLock()
	session, ok := s.store.sessions[ev.SessionID]
	isRunning := ok && session != nil && session.Status == enums.SessionStatusRunning
	s.store.mu.RUnlock()
	// 会话不存在或未运行则直接丢弃事件。
	if !isRunning {
		return
	}

	// 只要不是 Error 类型事件，就视为成功。
	success := ev.Kind != eventkind.Error
	// 工具调用事件：直接记录工具执行事件。
	if ev.Kind == "tool_call" || ev.Kind == eventkind.ToolCall {
		s.store.addEvent(session, eventkind.ToolExec, ev.Agent, ev.Message, ev.Kind, ev.Tool, toolArgsLabel(ev.Detail), "", "", success)
		return
	}
	// 工具结果事件：尝试解析 Detail 中的 output、error 与 path 字段。
	if ev.Kind == "tool_result" || ev.Kind == eventkind.ToolResult {
		var output, toolErr, toolPath string
		if ev.Detail != "" {
			var detail map[string]any
			// 解析 JSON 详情，忽略解析失败的情况。
			if err := json.Unmarshal([]byte(ev.Detail), &detail); err == nil {
				if v, ok := detail["output"].(string); ok {
					output = v
				}
				if v, ok := detail["error"].(string); ok {
					toolErr = v
				}
				// path 由工具执行器填充（如 ReadFile 的文件路径、HTTPGet 的 URL），
				// 供 TUI 在工具行显示操作对象。
				if v, ok := detail["path"].(string); ok {
					toolPath = v
				}
			}
		}
		s.store.addEvent(session, eventkind.ToolExec, ev.Agent, ev.Message, ev.Kind, ev.Tool, toolPath, output, toolErr, success)
		return
	}
	// 其他类型事件作为进度事件记录。
	s.store.addEvent(session, eventkind.Progress, ev.Agent, ev.Message, ev.Kind, ev.Tool, "", "", "", success)
}

// toolArgsLabel 从工具调用参数 JSON 中提取一个简短的展示标签（路径/命令/URL 等），
// 供 TUI 在工具调用行中显示操作对象；无法解析时返回空串。
func toolArgsLabel(argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}
	for _, k := range []string{"path", "command", "url", "pattern", "dir", "query", "file"} {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// runSession 为新创建的会话执行 ReAct 主循环。
func (s *ReactService) runSession(session *reactInternalSession) {	// 获取会话上下文；若不存在则使用 Background。
	ctx := sessionContext(session)
	// 会话结束后清理临时目录并淘汰已完成会话。
	defer s.finalizeSession(session)

	// 记录会话启动事件。
	s.store.addEvent(session, eventkind.System, "System", "会话启动", "", "", "", "", "", true)

	// 获取 meta 角色配置；若缺失则标记会话错误并退出。
	metaRole := s.roleRegistry.Get("meta")
	if metaRole == nil {
		s.setSessionError(session, "meta role not found")
		return
	}

	// 确定模型 provider：优先使用测试注入的 provider，否则从模型工厂获取。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		p, err := s.modelFactory.GetBladesProvider(ctx, "meta")
		if err != nil {
			s.setSessionError(session, fmt.Sprintf("failed to get blades provider: %v", err))
			return
		}
		provider = p
	}

	// 构造 ReActAgent，并注入邮箱、记忆管道与主循环运行时配置。
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapter(s.toolRegistry)).
		WithMailbox(s.mailbox).
		WithMemory(s.memory).
		WithLoopConfig(s.runtimeCfg.LoopConfig())

	// 将会话 ID 注入工具上下文，便于工具内部识别当前会话。
	runCtx := tool.WithSessionID(ctx, session.ID)

	// 运行 ReAct 主循环，传入会话目标。
	result, err := agent.Run(runCtx, session.Goal)
	if err != nil {
		// 运行出错时标记会话错误并退出。
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		s.pauseSession(session, result.History)
		return
	}

	// 运行成功：更新会话状态为已完成，并记录结果与历史。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = result.Text
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

	// 添加 Agent 完成事件。
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", result.Text, "", "", "", "", "", true)

	// 持久化历史与事件到 Postgres。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// resumeSession 使用新的用户输入继续会话。
func (s *ReactService) resumeSession(session *reactInternalSession) {
	// 获取会话上下文。
	ctx := sessionContext(session)
	// 结束后清理资源。
	defer s.finalizeSession(session)

	// 获取 meta 角色；缺失则报错。
	metaRole := s.roleRegistry.Get("meta")
	if metaRole == nil {
		s.setSessionError(session, "meta role not found")
		return
	}

	// 确定模型 provider，逻辑同 runSession。
	var provider ModelProvider
	if s.testProvider != nil {
		provider = s.testProvider
	} else {
		p, err := s.modelFactory.GetBladesProvider(ctx, "meta")
		if err != nil {
			s.setSessionError(session, fmt.Sprintf("failed to get blades provider: %v", err))
			return
		}
		provider = p
	}

	// 构造并配置 ReActAgent。
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapter(s.toolRegistry)).
		WithMailbox(s.mailbox).
		WithMemory(s.memory).
		WithLoopConfig(s.runtimeCfg.LoopConfig())

	// 注入会话 ID 到工具上下文。
	runCtx := tool.WithSessionID(ctx, session.ID)

	// 使用最新用户消息作为本轮输入，并以之前的历史作为种子。
	var input string
	s.store.mu.RLock()
	if len(session.Messages) > 0 {
		// 取 Messages 中最后一条作为当前轮输入。
		input = session.Messages[len(session.Messages)-1].Content
	}
	// 拷贝历史记录，避免在加锁期间被外部修改。
	history := make([]ReactMessage, len(session.History))
	copy(history, session.History)
	s.store.mu.RUnlock()

	// 调用带历史的 ReAct 运行接口。
	result, err := agent.RunWithHistory(runCtx, input, history)
	if err != nil {
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		s.pauseSession(session, result.History)
		return
	}

	// 更新会话状态为已完成。
	now := time.Now()
	s.store.mu.Lock()
	session.Status = enums.SessionStatusCompleted
	session.Result = result.Text
	session.EndedAt = &now
	session.History = result.History
	s.store.mu.Unlock()

	// 添加完成事件并持久化。
	s.store.addEvent(session, eventkind.AgentDone, "MetaAgent", result.Text, "", "", "", "", "", true)

	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// finalizeSession 在会话结束时执行清理工作。
func (s *ReactService) finalizeSession(session *reactInternalSession) {
	// 暂停待续（awaiting_clarify）的会话保留临时目录，用户续跑时仍需其中的中间产物。
	if session.Status != enums.SessionStatusAwaitingClarify {
		// 清理会话临时目录。
		s.store.cleanupSessionTempDir(session.ID, session.TempDir)
	}
	// 淘汰已完成的会话，避免内存无限增长。
	s.store.evictCompletedSessions()
}

// pauseSession 在达到最大轮数上限时将会话置为"暂停待续"而非错误：
// 状态置为 awaiting_clarify（TUI/Web 显示等待态），History 完整保留，
// 并提示用户发送消息即可从当前进度续跑（sendMessage → resumeSession → RunWithHistory）。
func (s *ReactService) pauseSession(session *reactInternalSession, history []ReactMessage) {
	s.store.mu.Lock()
	session.Status = enums.SessionStatusAwaitingClarify
	session.History = history
	session.Result = "已达最大轮数上限，会话暂停，等待用户消息续跑"
	s.store.mu.Unlock()

	// 记录暂停事件并给出明确的续跑指引。
	maxIter := s.runtimeCfg.LoopConfig().MaxIterations
	msg := fmt.Sprintf("已达最大轮数上限（%d 轮），会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。", maxIter)
	s.store.addEvent(session, eventkind.System, "System", msg, "", "", "", "", "", true)

	// 持久化历史与事件，保证重启后仍可续跑。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// setSessionError 将会话标记为错误状态，并记录相关事件与持久化。
func (s *ReactService) setSessionError(session *reactInternalSession, msg string) {
	now := time.Now()
	// 更新会话状态、结果与结束时间。
	s.store.mu.Lock()
	session.Status = enums.SessionStatusError
	session.Result = msg
	session.EndedAt = &now
	s.store.mu.Unlock()

	// 添加错误事件。
	s.store.addEvent(session, eventkind.Error, "System", msg, "", "", "", "", "", false)

	// 持久化历史与事件。
	s.store.persistHistory(session)
	s.store.persistEvents(session)
}

// sessionContext 返回会话的上下文；若未设置则返回 Background。
func sessionContext(session *reactInternalSession) context.Context {
	if session.ctx != nil {
		return session.ctx
	}
	return context.Background()
}

// restartSessionContext 为被恢复的会话创建一个全新的可取消上下文，
// 确保之前被取消的会话能够再次运行。
func restartSessionContext(session *reactInternalSession) {
	ctx, cancel := context.WithCancel(context.Background())
	session.ctx = ctx
	session.cancelFn = cancel
}

// sendMessage 向会话发送一条消息，并在必要时恢复会话运行。
func (s *ReactService) sendMessage(ctx context.Context, sessionID, content string) error {
	// 空内容直接拒绝。
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}

	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}

	// 将用户消息追加到会话消息列表。
	session.Messages = append(session.Messages, Message{
		Role:      string(enums.ChatRoleUser),
		Content:   content,
		Timestamp: time.Now(),
	})

	// 记录会话原先是否处于运行状态。
	wasRunning := session.Status == enums.SessionStatusRunning
	// 如果不在运行，则重新置为运行状态，清除结束时间，并重建上下文。
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 添加用户消息事件。
	s.store.addEvent(session, eventkind.UserMessage, "User", content, "", "", "", "", "", true)

	// 如果会话原先未运行，则在 goroutine 中恢复执行。
	if !wasRunning {
		go s.resumeSession(session)
	}
	return nil
}

// answerClarify 处理用户对澄清问题的答复。
func (s *ReactService) answerClarify(ctx context.Context, sessionID, answer string) error {
	// 空答复拒绝处理。
	if answer == "" {
		return fmt.Errorf("answer cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 只有处于等待澄清状态的会话才能接收澄清答复。
	if session.Status != enums.SessionStatusAwaitingClarify {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not awaiting clarification", ErrInvalidSessionState)
	}
	// 将澄清答复作为用户消息追加。
	session.Messages = append(session.Messages, Message{
		Role:      string(enums.ChatRoleUser),
		Content:   "[澄清答复] " + answer,
		Timestamp: time.Now(),
	})
	// 恢复为运行状态并重建上下文。
	session.Status = enums.SessionStatusRunning
	restartSessionContext(session)
	s.store.mu.Unlock()

	// 记录澄清答复事件。
	s.store.addEvent(session, eventkind.Clarify, "User", "用户答复: "+answer, "", "", "", "", "", true)

	// 异步恢复会话执行。
	go s.resumeSession(session)
	return nil
}

// interrupt 向运行中或已暂停的会话发送抢占中断消息。
func (s *ReactService) interrupt(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 若会话未运行，则重新激活。
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 记录中断事件。
	s.store.addEvent(session, eventkind.Interrupt, "User", "抢占中断: "+content, "", "", "", "", "", true)

	// 若原先未运行，则异步恢复执行以处理中断。
	if !wasRunning {
		go s.resumeSession(session)
	}
	return nil
}

// enqueue 向会话注入一条队列消息，通常用于后台任务继续推进。
func (s *ReactService) enqueue(ctx context.Context, sessionID, content string) error {
	if content == "" {
		return fmt.Errorf("content cannot be empty")
	}
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 若会话未运行，则重新激活。
	wasRunning := session.Status == enums.SessionStatusRunning
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.EndedAt = nil
		restartSessionContext(session)
	}
	s.store.mu.Unlock()

	// 记录队列注入事件。
	s.store.addEvent(session, eventkind.Enqueue, "User", "队列注入: "+content, "", "", "", "", "", true)

	// 若原先未运行，则异步恢复执行。
	if !wasRunning {
		go s.resumeSession(session)
	}
	return nil
}

// cancel 取消指定会话的执行。
func (s *ReactService) cancel(ctx context.Context, sessionID string) error {
	// 加锁查找会话。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return ErrSessionNotFound
	}
	// 只有运行中的会话才能被取消。
	if session.Status != enums.SessionStatusRunning {
		s.store.mu.Unlock()
		return fmt.Errorf("%w: session is not running", ErrInvalidSessionState)
	}
	// 取出取消函数并在解锁后调用，避免在持有锁时执行取消回调。
	cancelFn := session.cancelFn
	session.cancelFn = nil
	// 将会话标记为错误状态并记录结束时间。
	session.Status = enums.SessionStatusError
	session.Result = "cancelled by user"
	now := time.Now()
	session.EndedAt = &now
	s.store.mu.Unlock()

	// 记录取消事件。
	s.store.addEvent(session, eventkind.System, "System", "会话已被用户取消", "", "", "", "", "", true)

	// 调用取消函数通知 ReAct 循环退出。
	if cancelFn != nil {
		cancelFn()
	}
	return nil
}

// toReactAgentSession 将内部 reactInternalSession 转换为公共 Session DTO。
func toReactAgentSession(s *reactInternalSession) *Session {
	// 空指针安全处理。
	if s == nil {
		return nil
	}

	// 处理可能为空的结束时间，DTO 使用值类型。
	var endedAt time.Time
	if s.EndedAt != nil {
		endedAt = *s.EndedAt
	}

	// 转换内部事件列表为公共事件列表。
	events := make([]Event, 0, len(s.Events))
	for i := range s.Events {
		events = append(events, *toAgentEvent(&s.Events[i]))
	}

	// 拷贝消息列表，避免外部修改内部状态。
	messages := make([]Message, len(s.Messages))
	copy(messages, s.Messages)

	// 组装并返回公共 Session。
	return &Session{
		ID:             s.ID,
		Goal:           s.Goal,
		Status:         string(s.Status),
		Result:         s.Result,
		State:          "active",
		StartedAt:      s.StartedAt,
		EndedAt:        endedAt,
		Events:         events,
		Messages:       messages,
		TempDir:        s.TempDir,
		ActiveBlocks:   []ActiveBlock{},
		PendingClarify: nil,
	}
}

// newReactServiceForTest 构造一个用于包级测试的 ReactService，使用 mock provider。
func newReactServiceForTest(provider ModelProvider, workDir string) *ReactService {
	// 使用空的角色配置创建角色注册表。
	cfg := &pkgconfig.RoleConfigFile{}
	roleRegistry := role.NewRegistry(cfg)
	// 创建仅含内建工具的注册表，不带外部回调。
	toolRegistry := tool.NewBuiltinRegistry(workDir, nil, nil)
	// 创建 ReactService，模型工厂与邮箱等依赖为空。
	s := NewReactService(roleRegistry, nil, toolRegistry, nil, NopMemoryPipeline{}, nil)
	// 注入 mock provider。
	s.testProvider = provider
	// 如果提供了工作目录，则覆盖默认存储工作目录。
	if workDir != "" {
		s.store.workDir = workDir
	}
	return s
}
