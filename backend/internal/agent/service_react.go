// Package agent 提供基于 ReAct 引擎的 Agent 服务实现，
// 负责会话生命周期管理与外部接口适配。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/orchestrator"
	"github.com/blockmemory/agent/backend/internal/domain/role"
	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/server/eventkind"
	"github.com/blockmemory/agent/backend/internal/store"
	pkgconfig "github.com/blockmemory/agent/backend/pkg/config"
	"github.com/blockmemory/agent/backend/pkg/enums"
	"github.com/blockmemory/agent/backend/pkg/textutil"
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

	// pendingChecker 注入到每个 ReActAgent，用于父会话终结保护
	// （有未决子 Agent 时阻止终结，防止迟到 mailbox 消息丢失）。
	// 为 nil 时关闭保护；由 bootstrap 注入 subagent.Dispatcher 实现。
	pendingChecker PendingChildrenChecker

	// pausedChecker 注入到 MetaAgent ReActAgent，用于父终结保护 wait loop 检测
	// Paused 子 DomainAgent（触达 token 上限）。MetaAgent 无限 budget 不会自行暂停，
	// 靠此检查跳出 wait loop 返回 PausedOnChild，由上层 pauseSession 置会话暂停态。
	// 为 nil 时关闭检查；由 bootstrap 注入 subagent.Dispatcher 实现。
	pausedChecker PausedChildChecker

	// resumeDispatcher 注入 Paused DomainAgent 恢复器，sendMessage 在会话处于
	// PausedOnChild 态时调用其 ResumePaused 从 agent_messages 加载历史续跑。
	// 为 nil 时 PausedOnChild 会话回退到普通 resumeSession（不恢复暂停的 domain）。
	resumeDispatcher PausedDomainResumer

	// testProvider 是包内部测试使用的钩子，
	// 允许单元测试注入 mock 的 ModelProvider，从而无需真实 API 密钥即可运行 ReAct 循环。
	testProvider ModelProvider

	// trees 按 sessionID 维护权威 Agent 树（lazy init）。
	// Dispatcher 通过 TreeFor(sid) 取得 *orchestrator.Tree 后 Register/Finish/SetCancel。
	// Snapshot/Cancel 经由 Tree()/CancelAgent() 暴露给 HTTP API。
	// treeStore 非 nil 时 TreeFor lazy init 会调 LoadFromStore 恢复历史节点(重启不丢)。
	trees sync.Map
	// treeStore 可选的 Agent 树持久化层。为 nil 时纯内存。
	// 由 bootstrap 注入 store.PostgresTreeStore;测试场景保持 nil。
	treeStore orchestrator.TreeStore
	// sharedMemoryStore 可选的共享记忆 KV,用于话题切换时写入旧话题摘要。
	// key 格式 `topic:{topicID}:summary`。为 nil 时跳过摘要写入(测试场景)。
	// MetaAgent 在新话题召回该摘要依赖步骤 4 part C(MetaAgent 根 recall 注入,未做)。
	sharedMemoryStore tool.SharedMemoryStore
	// persona 可选的人格注入器（soul.Loader 实现该接口）；为 nil 时不注入人格前缀。
	// bootstrap 注入 runtime.Soul；runSession/resumeSession 构造 MetaAgent 时调用 WithPersonaInjector。
	persona PersonaInjector
}

// SetTreeStore 注入 Agent 树持久化层。bootstrap 在创建 ReactService 后调用。
// 传 nil 关闭持久化(纯内存,测试场景)。
func (s *ReactService) SetTreeStore(ts orchestrator.TreeStore) {
	s.treeStore = ts
}

// SetSharedMemoryStore 注入共享记忆 KV,用于话题切换时写入旧话题摘要。
// bootstrap 在创建 sharedKV 后调用。传 nil 关闭摘要写入(测试场景)。
func (s *ReactService) SetSharedMemoryStore(store tool.SharedMemoryStore) {
	s.sharedMemoryStore = store
}

// SetPersonaInjector 注入人格注入器(soul.Loader),使 MetaAgent 系统提示词头部带人格前缀。
// bootstrap 注入 runtime.Soul;传 nil 关闭人格注入(测试场景)。
func (s *ReactService) SetPersonaInjector(p PersonaInjector) {
	s.persona = p
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
	// TokenBudgetPerGoal 单次 RunWithHistory 累计 token 上限（input+output 之和）。
	// <=0 不限制；>0 超限后主循环 break 返回部分完成（LimitReached）。
	// 被 TokenBudgetPerRole 覆盖:按角色设预算时此项对该角色无效。
	TokenBudgetPerGoal int
	// TokenBudgetPerRole 按角色 ID 设单 Agent token 上限。未列出角色按默认:
	// domain=120000, meta=200000(安全网,不为 0 因 maxIter=-1 已无界), 其他(叶子助手)=40000。
	// nil 时全部走默认。显式值覆盖默认,resume 时重置(各 Agent 独立预算)。
	TokenBudgetPerRole map[string]int
}

// SetRuntimeConfig 注入 ReAct 主循环运行时参数（见 ReactRuntimeConfig）。
func (s *ReactService) SetRuntimeConfig(c ReactRuntimeConfig) {
	s.runtimeCfg = c
}

// SetPendingChildrenChecker 注入未决子 Agent 检查器，使后续创建的每个 ReActAgent
// 都开启父会话终结保护。由 bootstrap 在装配 subagent.Dispatcher 后调用。
func (s *ReactService) SetPendingChildrenChecker(p PendingChildrenChecker) {
	s.pendingChecker = p
}

// SetPausedChildChecker 注入 Paused 子 Agent 检查器，使 MetaAgent 在父终结保护
// wait loop 中能检测 Paused 子 DomainAgent 并主动暂停会话。由 bootstrap 注入。
func (s *ReactService) SetPausedChildChecker(p PausedChildChecker) {
	s.pausedChecker = p
}

// SetPausedDomainResumer 注入 Paused DomainAgent 恢复器，使 sendMessage 在
// PausedOnChild 态能优先恢复暂停的 domain。由 bootstrap 注入 subagent.Dispatcher。
func (s *ReactService) SetPausedDomainResumer(r PausedDomainResumer) {
	s.resumeDispatcher = r
}

// ForwardLiveEvent 是子 Agent 实时事件转发入口：Dispatcher 派发子 Agent 时注入的
// WithLiveEvents 回调按 sessionID 路由到这里，使子 Agent token 用量/流式增量/工具事件
// 也走会话级 handleLiveEvent，让 TUI/Web 看到所有 Agent 的累计 token。
// sessionID 来自子 Agent ctx（call_sub_agent 异步路径已 WithSessionID）。
func (s *ReactService) ForwardLiveEvent(sessionID string, ev LiveEvent) {
	if sessionID == "" {
		return
	}
	sess := s.store.snapshotSessionByID(sessionID)
	if sess == nil {
		return
	}
	// 子 Agent 的流式文本与主会话共用单一瞬时展示字段（StreamingText/ThinkingText），
	// 且多路并发会互相覆盖；加来源前缀让 UI 能分辨当前是谁在输出。
	// 主 Agent 的流式事件走 WithLiveEvents 直连 handleLiveEvent，不经过此处，不会被加前缀。
	if (ev.Kind == LiveEventLLMDelta || ev.Kind == LiveEventThinkDelta) && ev.Agent != "" {
		ev.Text = "【" + ev.Agent + "】\n" + ev.Text
	}
	s.handleLiveEvent(sess, ev)
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
	if c.TokenBudgetPerGoal > 0 {
		lc.TokenBudget = c.TokenBudgetPerGoal
	}
	return lc
}

// LoopConfigByRole 返回按角色定制的 LoopConfig:在 LoopConfig() 基础上按 roleID 覆盖 TokenBudget。
// 预算分级:DomainAgent 120000(到限暂停可恢复),叶子助手 40000(到限返回部分产出),
// meta 200000(安全网,不收敛时暂停等续跑;不为 0 因 maxIter=-1 已无界,双无界会死循环)。
// codegen 单次可吐 10K+ token(如塔防 config.js),旧值 50K/20K 扛不住多文件生成,抬至 120K/40K。
// TokenBudgetPerRole 显式配置覆盖默认;未列出角色按上述默认。
// resume 时 usedTokens 局部变量自动重置,即每个 Agent 各自独立预算。
func (c ReactRuntimeConfig) LoopConfigByRole(roleID string) LoopConfig {
	lc := c.LoopConfig()
	lc.TokenBudget = c.roleTokenBudget(roleID)
	return lc
}

// roleTokenBudget 返回角色 token 预算:显式配置优先,否则按角色默认(domain 120000 / meta 200000 / 其他 40000)。
// meta 不给 0(无限):config tool_call_max_rounds=-1 已使 maxIter 无界,若 budget 也无界,
// 模型不收敛时会无限循环(实证:TUI 重复思考不前进)。200K 安全网让 meta 不收敛时暂停等续跑。
func (c ReactRuntimeConfig) roleTokenBudget(roleID string) int {
	if v, ok := c.TokenBudgetPerRole[roleID]; ok {
		return max(v, 0)
	}
	switch roleID {
	case "domain":
		return 120000
	case "meta":
		return 200000
	default:
		return 40000
	}
}

// SetModelProvider 注入一个 mock 或替代的模型 provider。
// 主要用于需要在无真实 API 密钥情况下运行 ReAct 循环的测试场景。
func (s *ReactService) SetModelProvider(p ModelProvider) {
	s.testProvider = p
}

// workDir 返回会话存储的工作目录，供 ReActAgent 在系统提示词中注入环境信息。
// 优先用 store.workDir；为空时回退到进程 cwd。
func (s *ReactService) workDir() string {
	if wd := s.store.workDir; wd != "" {
		return wd
	}
	return ""
}

// SetLogger 注入结构化日志器，使会话存储的错误类日志以 [ERRO] 级别输出。
// 参数 l：已初始化的 Logger 指针；未注入时回退标准库 log。
func (s *ReactService) SetLogger(l *logger.Logger) {
	s.store.setLogger(l)
}

// sessionLogger 返回绑定 sessionID 与 agentName 的日志器，供 ReActAgent 记录 LLM I/O。
// 未注入全局 logger 时返回 nil，ReActAgent 侧跳过 LLM I/O 日志。
func (s *ReactService) sessionLogger(sessionID, agentName string) *logger.Logger {
	base := s.store.logger()
	if base == nil {
		return nil
	}
	return base.WithSession(sessionID).WithAgent(agentName)
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
		// 轮询间隔 50ms：流式文本/思考文本的增量更新也走该通道推送，
		// 间隔越短，TUI 吐词的跟手度越高。
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		// seen 记录已经推送过的事件数量，避免重复发送。
		seen := 0
		// lastStream/lastThink/lastStatus 记录上次推送时的瞬时状态，
		// 变化时推送一个轻量 live_update 事件，驱动 TUI 立即重绘（不等 tick）。
		var lastStream, lastThink, lastStatus string

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
				// 流式文本/思考文本/会话状态变化：推送轻量刷新事件。
				if sess.StreamingText != lastStream || sess.ThinkingText != lastThink || string(sess.Status) != lastStatus {
					lastStream = sess.StreamingText
					lastThink = sess.ThinkingText
					lastStatus = string(sess.Status)
					select {
					case out <- *toAgentEvent(&internalEvent{Type: "live_update", Timestamp: time.Now()}):
					case <-ctx.Done():
						return
					}
				}
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
	case QueryKindLogs:
		// 返回 session_logs（含完整 LLM I/O prompt/response）。
		// agent/level 可空；limit<=0 时 QuerySessionLogs 默认 100。
		agent, _ := q.Args["agent"].(string)
		level, _ := q.Args["level"].(string)
		limit, _ := q.Args["limit"].(int)
		offset, _ := q.Args["offset"].(int)
		return Result{Data: s.store.queryLogs(ctx, sessionID, agent, level, limit, offset)}, nil
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

// TreeFor 按 sessionID 取得权威 Agent 树（不存在则 lazy 创建并从持久化层恢复）。
// Dispatcher 在派发子 Agent 时调用此方法注入 Register/Finish/SetCancel。
// treeStore 非 nil 时,新建 Tree 会调 LoadFromStore 恢复历史节点(重启不丢);
// 为 nil 时纯内存,与原行为一致。
func (s *ReactService) TreeFor(sessionID string) *orchestrator.Tree {
	if v, ok := s.trees.Load(sessionID); ok {
		return v.(*orchestrator.Tree)
	}
	t := orchestrator.NewTree(sessionID, s.treeStore)
	if s.treeStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = t.LoadFromStore(ctx)
		cancel()
	}
	v, loaded := s.trees.LoadOrStore(sessionID, t)
	if loaded {
		return v.(*orchestrator.Tree)
	}
	return t
}

// Tree 返回会话 Agent 树快照，按启动时间升序。
// 会话不存在返回 ErrSessionNotFound；树为空时返回空切片。
func (s *ReactService) Tree(ctx context.Context, sessionID string) ([]orchestrator.Node, error) {
	if sess := s.store.snapshotSessionByID(sessionID); sess == nil {
		return nil, ErrSessionNotFound
	}
	t := s.TreeFor(sessionID)
	return t.Snapshot(), nil
}

// CancelAgent 取消指定子 Agent 实例。
// instID 对应 call_sub_agent 返回的 sub_agent_id。
// 节点不存在或已 terminal 返回 ErrAgentNotFound。
func (s *ReactService) CancelAgent(ctx context.Context, sessionID, instID string) error {
	if sess := s.store.snapshotSessionByID(sessionID); sess == nil {
		return ErrSessionNotFound
	}
	t := s.TreeFor(sessionID)
	if !t.Cancel(instID) {
		return ErrAgentNotFound
	}
	return nil
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

// SwitchTopic 切换会话的当前话题(轻量话题隔离)。
//
// 流程:
//  1. 终结旧话题:取消当前 Agent 树所有 Running 节点 -> 压缩旧树快照为摘要 ->
//     写入 sharedKV `topic:{oldTopicID}:summary`(store 非 nil 时) -> 删除 PG 旧节点。
//  2. 起新话题:生成本会话内单调递增的 topicID,更新 session.activeTopicID,
//     新话题从空 Agent 树开始(同 Tree 实例,内存已清空)。
//
// 运行中与已完成会话均支持:已完成会话会重启 running 状态承载新话题。
// 无状态机,纯 KV 摘要 + 树切换。MetaAgent 在新话题召回旧摘要依赖步骤 4 part C(未做)。
//
// name 为新话题显示名;goal 为话题目标,空时回退到 name。
func (s *ReactService) SwitchTopic(ctx context.Context, sessionID, name, goal string) (*Session, error) {
	if name == "" {
		return nil, fmt.Errorf("name cannot be empty")
	}
	if goal == "" {
		goal = name
	}

	// 查找会话并记录切换前状态。
	s.store.mu.Lock()
	session, ok := s.store.sessions[sessionID]
	if !ok {
		s.store.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	oldTopicID := session.activeTopicID
	wasRunning := session.Status == enums.SessionStatusRunning
	s.store.mu.Unlock()

	// 终结旧话题的 Agent 树:取消 Running 节点 + 压缩摘要 + 清内存 + 删 PG。
	tree := s.TreeFor(sessionID)
	snapshot := tree.EndCurrentTopic()
	// 旧话题有节点时压缩摘要写入 sharedKV,供新话题召回。
	// key 格式 `topic:{sessionID}:{topicID}:summary`:sessionID 前缀隔离,防跨 session 污染
	// (与块记忆 recall 同原则)。召回侧 recallTopicSummaries 按 session 前缀读取。
	if oldTopicID != "" && s.sharedMemoryStore != nil && len(snapshot) > 0 {
		summary := summarizeTopicSnapshot(oldTopicID, snapshot)
		kvCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := s.sharedMemoryStore.Set(kvCtx, "topic:"+sessionID+":"+oldTopicID+":summary", summary); err != nil {
			s.store.logError(ctx, "switch topic: write old topic summary to KV failed", err)
		}
		cancel()
	}

	// 生成新话题 ID:本会话内单调递增,首话题用 "1",后续 +1。
	newTopicID := nextTopicID(oldTopicID)

	s.store.mu.Lock()
	session.activeTopicID = newTopicID
	// 已完成会话切换话题时重启为 running,承载新任务。
	if !wasRunning {
		session.Status = enums.SessionStatusRunning
		session.Result = ""
		endedAt := time.Time{}
		session.EndedAt = &endedAt
	}
	s.store.mu.Unlock()

	// 记录话题切换事件(含旧/新 topic ID,便于审计)。
	s.store.addEvent(session, eventkind.Progress, "User",
		fmt.Sprintf("切换话题: [%s] -> [%s] (topic %s -> %s)", session.Goal, name, oldTopicID, newTopicID),
		eventkind.TopicSwitch, "", "", "", "", true)
	session.Goal = goal

	return toReactAgentSession(s.store.snapshotSessionByID(sessionID)), nil
}

// nextTopicID 根据旧 topic ID 生成本会话内下一个单调递增 ID。
// 旧 ID 空返回 "1";旧 ID 为纯数字返回 N+1;解析失败回退带 "-2" 后缀保证唯一。
func nextTopicID(old string) string {
	if old == "" {
		return "1"
	}
	if n, err := strconv.Atoi(old); err == nil {
		return strconv.Itoa(n + 1)
	}
	return old + "-2"
}

// summarizeTopicSnapshot 把旧话题 Agent 树快照压缩为 KV 摘要文本。
// 格式:话题 ID 头 + 每节点一行(角色/状态/摘要/错误)。供新话题召回参考。
func summarizeTopicSnapshot(topicID string, nodes []orchestrator.Node) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "【话题 %s 摘要】\n", topicID)
	for _, n := range nodes {
		fmt.Fprintf(&sb, "- [%s/%s] %s", n.Role, n.Status.String(), n.Task)
		if n.Summary != "" {
			fmt.Fprintf(&sb, " => %s", n.Summary)
		}
		if n.Err != "" {
			fmt.Fprintf(&sb, " (err: %s)", n.Err)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// recallTopicSummaries 召回当前 session 历史话题摘要,供新话题 MetaAgent 续接上下文。
// key 格式 `topic:{sessionID}:{topicID}:summary`:按 session 前缀过滤防跨 session 污染,
// 跳过 currentTopicID(当前话题摘要未写,只在切换时落 KV)。无配置/无历史返回空串。
func (s *ReactService) recallTopicSummaries(ctx context.Context, sessionID, currentTopicID string) string {
	if s.sharedMemoryStore == nil {
		return ""
	}
	keys := s.sharedMemoryStore.Keys(ctx)
	prefix := "topic:" + sessionID + ":"
	var summaries []string
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, ":summary") {
			continue
		}
		topicID := strings.TrimSuffix(strings.TrimPrefix(k, prefix), ":summary")
		if topicID == "" || topicID == currentTopicID {
			continue
		}
		v, err := s.sharedMemoryStore.Get(ctx, k)
		if err != nil || strings.TrimSpace(v) == "" {
			continue
		}
		summaries = append(summaries, v)
	}
	return strings.Join(summaries, "\n\n")
}

// injectTopicRecall 按 activeTopicID 召回旧话题摘要并拼到 input 前。
// 用 recalledTopicID 去重:同一话题只注入一次,避免每次 resume 重复烧 token。
// 无历史摘要时也标记 recalledTopicID,避免反复扫 KV。
// 返回注入后的 input(无摘要时原样),以及是否已处理(recalledTopicID 已更新)。
func (s *ReactService) injectTopicRecall(ctx context.Context, session *reactInternalSession, input string) string {
	s.store.mu.Lock()
	active := session.activeTopicID
	done := session.recalledTopicID
	s.store.mu.Unlock()
	if done == active {
		return input
	}
	prefix := s.recallTopicSummaries(ctx, session.ID, active)
	s.store.mu.Lock()
	session.recalledTopicID = active
	s.store.mu.Unlock()
	if prefix == "" {
		return input
	}
	return prefix + "\n\n【当前话题目标】\n" + input
}

// handleToolEvent 接收工具注册表产生的进度事件，并将其注入到对应运行中会话的事件流。
func (s *ReactService) handleToolEvent(ctx context.Context, ev tool.ProgressEvent) {
	// 从 ctx 提取调用方 Agent ID（ReActAgent.Run 经 WithAgentID 注入）。
	// ProgressEvent.Agent 字段未被 emitTool/emitResult 填充，始终为空；
	// 不在此回填会导致日志 agentName 永远空串，sub-agent 工具事件无法归属。
	// MetaAgent 的 agent ID = session-ID（"session-1"），sub-agent = "session-1/code_assistant-5"。
	agentID := AgentIDFromContext(ctx)
	// 展示名优先取 role.Name（"MetaAgent"/"代码助手"/"领域Agent:xxx"），
	// 使日志与对话页可读；缺失时回退到 agentID，保证不空串。
	agentName := AgentDisplayNameFromContext(ctx)
	if agentName == "" {
		agentName = agentID
	}
	// 加读锁判断会话是否存在且处于运行状态。
	s.store.mu.RLock()
	session, ok := s.store.sessions[ev.SessionID]
	isRunning := ok && session != nil && session.Status == enums.SessionStatusRunning
	s.store.mu.RUnlock()
	// 会话不存在或未运行则直接丢弃事件。
	if !isRunning {
		return
	}
	// call_sub_agent 的事件由 LiveEvent 通道以更丰富的形式记录（sub_agent_dispatch
	// 含角色与任务摘要、tool_exec 含子 Agent ID），跳过注册表侧的固定文案事件，避免重复。
	if ev.Tool == "call_sub_agent" {
		return
	}

	// 只要不是 Error 类型事件，就视为成功。
	success := ev.Kind != eventkind.Error
	// 工具调用事件：直接记录工具执行事件。
	if ev.Kind == "tool_call" || ev.Kind == eventkind.ToolCall {
		s.store.addEvent(session, eventkind.ToolExec, agentName, ev.Message, ev.Kind, ev.Tool, toolArgsLabel(ev.Detail), "", "", success)
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
		// 失败时把 path+error 拼进 message，让 logInfo 输出可见失败原因；
		// 否则日志只显示"工具结果 WriteFile"，错误细节只存在 DB 事件里，排查困难。
		msg := ev.Message
		if toolErr != "" {
			extra := ""
			if toolPath != "" {
				extra = " path=" + toolPath
			}
			msg = fmt.Sprintf("%s [FAIL]%s err=%s", ev.Message, extra, toolErr)
		}
		s.store.addEvent(session, eventkind.ToolExec, agentName, msg, ev.Kind, ev.Tool, toolPath, output, toolErr, success)
		return
	}
	// 其他类型事件作为进度事件记录。
	s.store.addEvent(session, eventkind.Progress, agentName, ev.Message, ev.Kind, ev.Tool, "", "", "", success)
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

// subAgentDispatchInfo 从 call_sub_agent 的入参 JSON 中提取角色 ID 与任务摘要（截断 100 字符），
// 供子 Agent 派发事件记录使用；解析失败时返回空角色与原始输入的截断。
func subAgentDispatchInfo(argsJSON string) (roleID, taskBrief string) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", textutil.TruncateRunes(argsJSON, 100, "…")
	}
	if v, ok := args["role_id"].(string); ok {
		roleID = v
	}
	if v, ok := args["task"].(string); ok {
		taskBrief = textutil.TruncateRunes(strings.ReplaceAll(strings.TrimSpace(v), "\n", " "), 100, "…")
	}
	return roleID, taskBrief
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
	// MetaAgent 暴露 call_sub_agent + 只读/信息类工具（ReadFile/ListDir/SearchInFiles/HTTPGet），
	// 不暴露 WriteFile/RunCommand，防止越位直接改文件或跑命令（metaRole.Tools 白名单限定）。
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterWithFilter(s.toolRegistry, metaRole.Tools)).
		WithMailbox(s.mailbox).
		WithMemory(s.memory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole("meta")).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(s.workDir()).
		WithPersonaInjector(s.persona)
	// 注入未决子 Agent 检查器，开启父会话终结保护。
	if s.pendingChecker != nil {
		agent = agent.WithPendingChildrenChecker(s.pendingChecker)
	}
	// 注入 Paused 子 Agent 检查器，使 MetaAgent 在 wait loop 检测 Paused 子 domain 并主动暂停。
	if s.pausedChecker != nil {
		agent = agent.WithPausedChildChecker(s.pausedChecker)
	}

	// 将会话 ID 注入工具上下文，便于工具内部识别当前会话。
	runCtx := tool.WithSessionID(ctx, session.ID)

	// 召回旧话题摘要拼到目标前(切换话题后续接上下文);同一话题只注入一次。
	goal := s.injectTopicRecall(ctx, session, session.Goal)

	// 运行 ReAct 主循环，传入会话目标。
	result, err := agent.Run(runCtx, goal)
	if err != nil {
		// 运行出错时标记会话错误并退出。
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		kind := PauseIterationLimit
		if result.PausedOnChild {
			kind = PauseOnChild
		}
		s.pauseSession(session, result.History, kind)
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
	// MetaAgent 暴露 call_sub_agent + 只读/信息类工具（ReadFile/ListDir/SearchInFiles/HTTPGet），
	// 不暴露 WriteFile/RunCommand，防止越位直接改文件或跑命令（metaRole.Tools 白名单限定）。
	agent := NewReActAgent(session.ID, *metaRole, provider, NewToolRegistryAdapterWithFilter(s.toolRegistry, metaRole.Tools)).
		WithMailbox(s.mailbox).
		WithMemory(s.memory).
		WithLoopConfig(s.runtimeCfg.LoopConfigByRole("meta")).
		WithLiveEvents(func(ev LiveEvent) { s.handleLiveEvent(session, ev) }).
		WithLogger(s.sessionLogger(session.ID, metaRole.Name)).
		WithWorkDir(s.workDir()).
		WithPersonaInjector(s.persona)
	// 注入未决子 Agent 检查器，开启父会话终结保护。
	if s.pendingChecker != nil {
		agent = agent.WithPendingChildrenChecker(s.pendingChecker)
	}
	// 注入 Paused 子 Agent 检查器，使 MetaAgent 在 wait loop 检测 Paused 子 domain 并主动暂停。
	if s.pausedChecker != nil {
		agent = agent.WithPausedChildChecker(s.pausedChecker)
	}

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

	// 召回旧话题摘要拼到本轮输入前(切换话题后续接上下文);同一话题只注入一次。
	input = s.injectTopicRecall(ctx, session, input)

	// 调用带历史的 ReAct 运行接口。
	result, err := agent.RunWithHistory(runCtx, input, history)
	if err != nil {
		s.setSessionError(session, err.Error())
		return
	}

	// 达到轮数上限：不视为失败——暂停会话、保留全部进度，等待用户消息续跑。
	if result.LimitReached {
		kind := PauseIterationLimit
		if result.PausedOnChild {
			kind = PauseOnChild
		}
		s.pauseSession(session, result.History, kind)
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
	// 会话结束（完成/出错/暂停）时清空流式输出与思考过程状态，UI 停止渲染瞬时内容。
	s.store.setStreamingText(session, "")
	s.store.setThinkingText(session, "")
	// 暂停待续（awaiting_clarify / paused_on_child）的会话保留临时目录，用户续跑时仍需其中的中间产物。
	if session.Status != enums.SessionStatusAwaitingClarify && session.Status != enums.SessionStatusPausedOnChild {
		// 清理会话临时目录。
		s.store.cleanupSessionTempDir(session.ID, session.TempDir)
	}
	// 淘汰已完成的会话，避免内存无限增长。
	s.store.evictCompletedSessions()
}

// handleLiveEvent 把 ReAct 运行中的实时进度事件写入会话：
// LLM/思考流式增量原位更新对应文本字段（不产生事件记录，避免事件流/数据库被 token 级事件淹没）；
// 工具调用/执行完成与子 Agent 完成追加为会话事件（少量且有审计价值，随 persistEvents 持久化）。
func (s *ReactService) handleLiveEvent(session *reactInternalSession, ev LiveEvent) {
	switch ev.Kind {
	case LiveEventLLMDelta:
		// 答复文本开始输出时，思考阶段结束，清空瞬时思考展示。
		s.store.setThinkingText(session, "")
		s.store.setStreamingText(session, ev.Text)
	case LiveEventThinkDelta:
		s.store.setThinkingText(session, ev.Text)
	case LiveEventToolCall:
		// call_sub_agent 是子 Agent 派发：记录专用派发事件（角色 ID 与任务摘要），
		// 供 TUI 对话区展示阶段标记、编排面板派生子 Agent 节点。
		if ev.Tool == "call_sub_agent" {
			roleID, taskBrief := subAgentDispatchInfo(ev.Input)
			s.store.addEvent(session, eventkind.Message, ev.Agent, taskBrief, "sub_agent_dispatch", roleID, "", "", "", true)
		}
		// 其他工具的调用事件已由工具注册表的进度回调记录（handleToolEvent），此处不重复。
	case LiveEventToolExec:
		// 其他工具的执行事件已由工具注册表的进度回调记录（handleToolEvent），
		// 这里只补 call_sub_agent：其 Output 是子 Agent ID，记入 ToolPath 供 UI 统计"等待中的子 Agent"。
		if ev.Tool != "call_sub_agent" {
			return
		}
		s.store.addEvent(session, eventkind.ToolExec, ev.Agent, "", "", ev.Tool, strings.TrimSpace(ev.Output), ev.Output, ev.Error, ev.Success)
	case LiveEventSubAgentDone:
		s.store.addEvent(session, eventkind.Message, "SubAgent", ev.Tool, "sub_agent_done", "", "", "", "", true)
	case LiveEventTokenUsage:
		// 单次 LLM 调用 token 用量：记为 token_usage 调试事件，供前端实时累加展示。
		// 消息格式 in=<n> out=<n> 与 textutil.ParseTokenUsage 兼容，便于后端日志解析复用。
		msg := fmt.Sprintf("in=%d out=%d", ev.InputTokens, ev.OutputTokens)
		s.store.addEventDebug(session, eventkind.Stats, ev.Agent, msg, eventkind.TokenUsage, "", "", "", "", true, "", int(ev.InputTokens), int(ev.OutputTokens), "")
	}
}

// PauseKind 区分会话暂停的原因，供 pauseSession 选择目标状态与文案。
type PauseKind int

const (
	// PauseIterationLimit MetaAgent 达到最大轮数上限（budget=0 不触 token，仅轮数）。
	PauseIterationLimit PauseKind = iota
	// PauseTokenBudget 达到 token 预算上限（非 meta 角色路径，预留）。
	PauseTokenBudget
	// PauseOnChild 子 DomainAgent 触达 token 上限暂停，MetaAgent 检测后主动暂停会话。
	PauseOnChild
)

// pauseMessage 按 PauseKind 返回面向用户的暂停提示文案。
func (s *ReactService) pauseMessage(kind PauseKind) string {
	switch kind {
	case PauseOnChild:
		return "子领域 Agent 触达 token 上限已暂停（完整历史已持久化，可恢复）。发送任意消息（如\"继续\"）将优先恢复暂停的领域 Agent 继续执行。"
	case PauseTokenBudget:
		return "已达 token 预算上限，会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。"
	default:
		// PauseIterationLimit：maxIter<=0（不限制）时不显示具体轮数，避免 "0 轮" 误报。
		maxIter := s.runtimeCfg.LoopConfig().MaxIterations
		if maxIter <= 0 {
			return "已达轮数上限，会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。"
		}
		return fmt.Sprintf("已达最大轮数上限（%d 轮），会话已暂停。发送任意消息（如\"继续\"）将从当前进度继续执行。", maxIter)
	}
}

// pauseSession 按暂停原因将会话置为对应暂停态而非错误：
// PauseOnChild -> paused_on_child（优先恢复暂停的 domain）；其他 -> awaiting_clarify（普通续跑）。
// History 完整保留，sendMessage -> resumeSession/resumePausedDomain 从当前进度续跑。
func (s *ReactService) pauseSession(session *reactInternalSession, history []ReactMessage, kind PauseKind) {
	s.store.mu.Lock()
	session.History = history
	switch kind {
	case PauseOnChild:
		session.Status = enums.SessionStatusPausedOnChild
		session.Result = "子领域 Agent 触达 token 上限暂停，发\"继续\"恢复该领域"
	default:
		session.Status = enums.SessionStatusAwaitingClarify
		session.Result = "已达上限，会话暂停，等待用户消息续跑"
	}
	s.store.mu.Unlock()

	s.store.addEvent(session, eventkind.System, "System", s.pauseMessage(kind), "", "", "", "", "", true)

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

	// 新用户消息 = 新任务起点：清空该 session 的 ReadFile 已读记录。
	// 设计意图：原始事故是单任务内反复读同一文件；任务完成后用户提新需求（如修 bug）
	// 需重读已改文件，不应被历史记录卡死。重复读限制为单任务级而非整个 session 级。
	if s.toolRegistry != nil {
		s.toolRegistry.ResetReadHistory(sessionID)
	}

	// 同理重置派发计数：全局派发限额按 session 累计，上一任务的消耗不应卡死下一任务。
	if r, ok := s.pendingChecker.(DispatchCountResetter); ok {
		r.ResetDispatchCounts(sessionID)
	}

	// 记录会话原先状态：Running 在跑；非 Running 需恢复（paused_on_child 优先恢复暂停的 domain）。
	priorStatus := session.Status
	wasRunning := priorStatus == enums.SessionStatusRunning
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
		// PausedOnChild: 优先恢复 earliest paused domain（任意消息，含"继续"与新任务，D1）。
		// 各 Agent 独立上下文：domain 从 agent_messages 加载 history 续跑，fresh budget。
		// 无 paused domain 或未注入恢复器时回退普通 resumeSession（MetaAgent 续跑）。
		if priorStatus == enums.SessionStatusPausedOnChild && s.resumeDispatcher != nil {
			if pausedID := s.findEarliestPausedDomain(session.ID); pausedID != "" {
				go s.resumePausedDomain(session, pausedID)
				return nil
			}
		}
		go s.resumeSession(session)
	}
	return nil
}

// resumePausedDomain 恢复一个因触达 token 上限而 Paused 的 DomainAgent。
// 委托 dispatcher.ResumePaused：从 agent_messages 加载历史，用 fresh budget 重建 domain Agent 续跑
// （各 Agent 独立上下文，不强制压缩，靠 Assemble 步频自动压缩）。
//   - 完成：dispatcher 已 tree.Finish + notify 父 + trackChildDone（父 MetaAgent 解除阻塞）；
//     此处 go resumeSession 让 MetaAgent drain mailbox 整合结果续跑。
//   - 再触限：dispatcher 已覆盖存 history + tree.Pause；此处 pauseSession(PauseOnChild) 等下次"继续"。
//   - 出错：回退 pauseSession(PauseOnChild)，不丢已持久化上下文（防跑飞）。
func (s *ReactService) resumePausedDomain(session *reactInternalSession, pausedNodeID string) {
	ctx := sessionContext(session)
	res, err := s.resumeDispatcher.ResumePaused(tool.WithSessionID(ctx, session.ID), pausedNodeID)
	if err != nil {
		s.store.addEvent(session, eventkind.Error, "System", fmt.Sprintf("恢复暂停领域 Agent 失败，已回退暂停态: %v", err), "", "", "", "", "", false)
		s.pauseSession(session, session.History, PauseOnChild)
		s.finalizeSession(session)
		return
	}
	if res.LimitReached {
		// 再触限：dispatcher 已 re-pause（覆盖存 history + tree.Pause）。会话置 PausedOnChild 等下次"继续"。
		s.pauseSession(session, session.History, PauseOnChild)
		s.finalizeSession(session)
		return
	}
	// 完成：MetaAgent 解除阻塞，go resumeSession 让其 drain mailbox 整合 domain 结果续跑。
	s.store.addEvent(session, eventkind.System, "System", "暂停的领域 Agent 已恢复完成，主 Agent 继续整合。", "", "", "", "", "", true)
	go s.resumeSession(session)
}

// findEarliestPausedDomain 扫描会话 Agent 树，返回最早 Started 的 Paused domain 节点 ID。
// 无则返回空串。供 sendMessage 在 PausedOnChild 态决定恢复目标（一次恢复一个，D1）。
func (s *ReactService) findEarliestPausedDomain(sessionID string) string {
	t := s.TreeFor(sessionID)
	if t == nil {
		return ""
	}
	var earliest string
	var earliestTime time.Time
	for _, n := range t.Snapshot() {
		if n.Role != "domain" || n.Status != orchestrator.StatusPaused {
			continue
		}
		if earliest == "" || n.Started.Before(earliestTime) {
			earliest = n.ID
			earliestTime = n.Started
		}
	}
	return earliest
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
	// 只有处于等待澄清状态的会话才能接收澄清答复（paused_on_child 走 sendMessage 恢复 domain）。
	if session.Status != enums.SessionStatusAwaitingClarify {
		s.store.mu.Unlock()
		if session.Status == enums.SessionStatusPausedOnChild {
			return fmt.Errorf("%w: session is paused on child, send a message to resume the paused domain", ErrInvalidSessionState)
		}
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
	// 运行中或暂停待续（awaiting_clarify / paused_on_child）的会话都可取消：
	// 暂停态没有运行中的 goroutine，但必须允许用户退出暂停死锁/死等场景
	// （实证：paused_on_child 态拒绝取消，会话无任何逃生通道，永久卡死）。
	switch session.Status {
	case enums.SessionStatusRunning, enums.SessionStatusAwaitingClarify, enums.SessionStatusPausedOnChild:
	default:
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
		StreamingText:  s.StreamingText,
		ThinkingText:   s.ThinkingText,
		ActiveBlocks:   []ActiveBlock{},
		PendingClarify: nil,
		ActiveTopicID:  s.activeTopicID,
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
