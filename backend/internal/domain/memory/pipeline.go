package memory

// 导入所需标准库与项目内部包。
import (
	"context" // context 用于持久化存储接口的上下文传递
	"fmt"     // fmt 用于格式化错误信息
	"log/slog" // slog 用于摘要失败时记录警告
	"strings"  // strings 用于事件摘要拼装
	"sync"     // sync 提供读写锁，保证并发安全
	"time"     // time 用于为事件填充发生时间

	"github.com/blockmemory/agent/backend/internal/agent" // agent 包提供 MemoryEvent、MemoryPipeline、ReactMessage 等类型
	"github.com/blockmemory/agent/backend/pkg/types"      // types 包提供 RoleDefinition 类型
)

// DefaultEventLimit 定义当 Assemble 未配置事件数量上限时，默认注入上下文的近期事件条数。
const DefaultEventLimit = 20

// maxRawEventJoinRunes 是【近期事件】注入文本的最大 rune 数（TODO #33）：
// 摘要器不可用降级 raw join 时全文拼接可能吹爆上下文（事故：摘要两次失败 raw join，
// MetaAgent 上下文膨胀），超限截断并附标记。摘要成功路径输出已压缩，一般远低于该值。
const maxRawEventJoinRunes = 4000

// DefaultMaxEventsPerAgent 定义每个 agent 在内存中最多保留的事件条数默认值。
// 该值必须大于 DefaultEventLimit，保证 Assemble 在容量裁减后仍能取满注入上限。
const DefaultMaxEventsPerAgent = 200

// eventSummarizeThreshold 是触发轻量模型摘要的事件条数阈值：事件数 <= 该值时直接拼装，
// 避免短任务为 3-5 条事件也额外调一次轻量模型。超过阈值时走摘要路径压缩 token。
const eventSummarizeThreshold = 8

// EventSummarizer 把一组格式化后的事件文本摘要成更短的上下文片段。
// bootstrap 侧用 modelFactory.CallLightweightWithRetry 实现，注入到 Pipeline。
// 为 nil 时关闭摘要路径，injectEvents 直接拼装原始事件。
type EventSummarizer func(ctx context.Context, events []string) (string, error)

// HistorySummarizer 把一段对话历史文本压成一个结构化压缩包（层级压缩用）。
// merge=false：输入是新滑出保留段的中段历史（formatSegmentText 渲染），输出单个压缩包；
// merge=true：输入是若干旧压缩包的拼接（从旧到新），输出合并后的单个更粗粒度压缩包。
// bootstrap 侧用 modelFactory.CallLightweightWithRetry 实现。为 nil 或调用失败时
// 降级为截断式压缩（user 500 字符 / 其他 200 字符），主流程不受影响。
type HistorySummarizer func(ctx context.Context, text string, merge bool) (string, error)

// Pipeline 实现了 agent.MemoryPipeline 接口，作为一个基于内存的事件流。
// 每个智能体（agent）的事件按插入顺序保存在内存中；如果配置了 Store，事件还可以被持久化。
type Pipeline struct {
	mu     sync.RWMutex                   // mu 保护 events  map，避免并发读写导致数据竞争
	events map[string][]agent.MemoryEvent // events 按 agentID 分组保存内存中的事件列表
	store  Store                          // store 是可选的持久化存储接口；为 nil 时只在内存中保留事件
	limit  int                            // limit 控制 Assemble 时最多向上下文注入多少条近期事件
	// maxEventsPerAgent 控制每个 agent 在内存中最多保留的事件条数，超出时丢弃最旧事件
	maxEventsPerAgent int
	// summarizer 是可选的轻量模型摘要器：事件数超过 eventSummarizeThreshold 时调用，
	// 把原始事件列表压成短摘要注入上下文，显著降低长任务的 token 占用。
	summarizer EventSummarizer
	// summarizeTimeout 是单次摘要调用的超时。默认 5s 只适合秒回的非思考型轻量模型；
	// 思考型模型（如 glm 系列）首 token 就要数十秒，须由 bootstrap 按配置注入更大值，
	// 否则摘要全挂降级 raw join，上下文全量回注导致 token 预算提前耗尽。
	summarizeTimeout time.Duration
	// consecutiveBalanceErrors 记录摘要器连续返回 Insufficient Balance（402）错误的次数。
	// 达到 2 次后关闭摘要路径，直接用 raw join，避免每次调用白等摘要超时（实证 DeepSeek 余额耗尽）。
	// 摘要成功时重置为 0。受 mu 保护。
	consecutiveBalanceErrors int
	// compressEvery 是历史压缩步频：每 N 轮 ReAct 迭代把中段历史暴力压缩为摘要。
	// <=0 关闭压缩，仅用 ReActAgent 的 windowMessages 滑动窗口。
	// 原 summarizeWindow 逻辑从 react_agent.go 迁入此处，职责归位到记忆层。
	compressEvery int
	// compressKeepRecent 是压缩时保留的最近原始消息条数；<=0 视为 10。
	compressKeepRecent int
	// compressCounters 按 agentID 记录 Assemble 调用次数，用于步频触发压缩。
	// 受 mu 保护。进程生命周期内不清理，与 events map 同生命周期。
	compressCounters map[string]int
	// compressStates 按 agentID 保存已冻结的压缩视图（摘要 + 保留段起点）。
	// 冻结视图在两次压缩之间字节级稳定：DeepSeek 前缀缓存只在压缩那一轮失效，
	// 其余轮次 history 纯追加、前缀全命中。受 mu 保护，与 events map 同生命周期。
	compressStates map[string]compressState
	// contextBudget 是按上下文 token 阈值触发压缩的默认上限（每角色可由 contextBudgetPerRole 覆盖）。
	// Assemble 后估算视图 token >= 阈值即触发压缩（保留近 compressKeepRecent，旧压成上下文内摘要块）。
	// <=0 关闭 token 触发，仅靠 compressEvery 步频兜底。默认 150000（bootstrap 注入）。
	contextBudget int
	// contextBudgetPerRole 按 roleID 覆盖 contextBudget。未列出角色用 contextBudget。
	contextBudgetPerRole map[string]int
	// tokenEstimator 估算消息切片的 token 数；为 nil 时不按 token 触发压缩（仅步频兜底）。
	// 由 bootstrap 注入 agent.EstimateMessagesTokens，避免 domain/memory 反向依赖 model 包。
	tokenEstimator func([]agent.ReactMessage) int
	// historySummarizer 是可选的层级压缩摘要器：每次压缩触发把新滑出保留段的中段历史
	// 压成一个结构化压缩包（LLM），并在压缩包超上限时合并最老的一半。
	// 为 nil 或失败时降级截断式压缩（旧行为）。见 pyramid.go。
	historySummarizer HistorySummarizer
	// maxBundles 是每个 agent 压缩包数量上限；超限时把最老的一半合并为 1 个更粗的包。
	// <=0 视为 DefaultMaxBundles。
	maxBundles int
	// compressLoaded 记录已尝试从 store 懒加载压缩状态的 agentID（重启恢复用），
	// 无论成败都只试一次，避免每轮 Assemble 打一次 DB。受 mu 保护。
	compressLoaded map[string]bool
	// eventsLoaded 记录已尝试从 store 懒加载历史事件的 agentID（重启恢复用），语义同上。
	eventsLoaded map[string]bool
}

// compressState 是某 agent 已冻结的压缩视图状态（层级压缩金字塔）。
// 字段导出以便 JSON 序列化落库（agent_compress_states 表），进程重启后懒加载恢复。
type compressState struct {
	Bundles []string `json:"bundles"` // 压缩包列表（从旧到新）；超 maxBundles 时最老的一半已合并为更粗的包
	// TailStart 是压缩包覆盖到的 history 下标（不含）；history[tailStart:] 原样保留。
	// 仅当全量 history 同时被持久化/恢复（MetaAgent 走 agent_messages）时下标语义跨重启有效。
	TailStart int `json:"tail_start"`
}

// Store 抽象了事件流的持久化能力，实现者负责把事件保存到磁盘或数据库。
type Store interface {
	// SaveEvent 将指定 agent 的单条事件持久化保存。
	SaveEvent(ctx context.Context, agentID string, event agent.MemoryEvent) error
	// LoadEvents 从持久化存储中加载指定 agent 的最多 limit 条事件。
	LoadEvents(ctx context.Context, agentID string, limit int) ([]agent.MemoryEvent, error)
	// SaveCompressState 持久化指定 agent 的层级压缩状态（压缩金字塔），供进程重启后恢复。
	SaveCompressState(ctx context.Context, agentID string, state *compressState) error
	// LoadCompressState 加载指定 agent 的压缩状态；无数据返回 (nil, nil)。
	LoadCompressState(ctx context.Context, agentID string) (*compressState, error)
}

// NewPipeline 创建一个基于内存的事件管道。
// 参数 store 可为 nil：传入 nil 时事件仅在内存中保留，不会持久化。
func NewPipeline(store Store) *Pipeline {
	return &Pipeline{
		events:            make(map[string][]agent.MemoryEvent), // 初始化空的 agentID -> 事件列表映射
		store:             store,                                // 保存外部传入的持久化存储实现
		limit:             DefaultEventLimit,                    // 默认使用 DefaultEventLimit 作为注入上限
		maxEventsPerAgent: DefaultMaxEventsPerAgent,             // 默认每个 agent 最多保留 DefaultMaxEventsPerAgent 条事件
		maxBundles:        DefaultMaxBundles,                    // 默认压缩包数量上限
		compressCounters:  make(map[string]int),                 // 初始化空的 agentID -> 步频计数器映射
		compressStates:    make(map[string]compressState),       // 初始化空的 agentID -> 冻结压缩视图映射
		compressLoaded:    make(map[string]bool),                // 初始化压缩状态懒加载记录
		eventsLoaded:      make(map[string]bool),                // 初始化事件懒加载记录
	}
}

// WithLimit 配置 Assemble 注入上下文时使用的近期事件数量。
// 参数 n 为非正数时会回退到 DefaultEventLimit，避免错误配置导致无法注入事件。
func (p *Pipeline) WithLimit(n int) *Pipeline {
	if n <= 0 {
		// n 不合法时回退到默认值，保证后续逻辑始终有可用上限
		n = DefaultEventLimit
	}
	p.limit = n
	// 返回自身以支持链式调用，例如 NewPipeline(nil).WithLimit(10)
	return p
}

// WithMaxEventsPerAgent 配置每个 agent 在内存中最多保留的事件条数。
// 参数 n 不大于 DefaultEventLimit 时回退到 DefaultMaxEventsPerAgent，
// 保证容量上限始终大于 Assemble 注入上限，避免注入逻辑取不满近期事件。
func (p *Pipeline) WithMaxEventsPerAgent(n int) *Pipeline {
	if n <= DefaultEventLimit {
		// n 不合法或过小时回退到默认值，维持 maxEventsPerAgent > DefaultEventLimit 的不变式
		n = DefaultMaxEventsPerAgent
	}
	p.maxEventsPerAgent = n
	// 返回自身以支持链式调用，例如 NewPipeline(nil).WithMaxEventsPerAgent(500)
	return p
}

// WithSummarizer 注入轻量模型摘要器，事件数超过阈值时把原始事件压成短摘要注入上下文。
// 传 nil 关闭摘要路径（默认关闭）。摘要失败时降级为直接拼装，不影响主流程。
func (p *Pipeline) WithSummarizer(s EventSummarizer) *Pipeline {
	p.summarizer = s
	return p
}

// WithSummarizeTimeout 配置单次摘要调用超时。<=0 时回退 5s 旧默认（仅适合秒回的非思考型
// 轻量模型）。思考型/慢推理模型应由装配层注入 60-180s，否则摘要路径形同虚设。
func (p *Pipeline) WithSummarizeTimeout(d time.Duration) *Pipeline {
	p.summarizeTimeout = d
	return p
}

// WithCompression 配置历史压缩步频与保留条数。
// every<=0 关闭压缩（仅用 ReActAgent 的 windowMessages 滑动窗口）。
// WithCompression 配置历史压缩的步频与保留段长度（步频兜底安全网）。
// keepRecent<=0 视为 10。原 summarizeWindow 逻辑从 react_agent.go 迁入此处。
// 职责归位：历史压缩属记忆层，不属 ReAct 层。
//
// token 阈值触发（WithContextBudget + WithTokenEstimator）为主，步频为兜底：
// 估算偏差或 estimator 未注入时，步频保证周期性压缩不缺位。
func (p *Pipeline) WithCompression(every, keepRecent int) *Pipeline {
	p.compressEvery = every
	if keepRecent <= 0 {
		keepRecent = 10
	}
	p.compressKeepRecent = keepRecent
	return p
}

// WithContextBudget 配置上下文 token 阈值：Assemble 后视图 token 达阈值即触发压缩
// （保留近 compressKeepRecent，旧消息压成上下文内摘要块，不落盘）。
// perRole 按 roleID 覆盖 defaultBudget（如 meta/domain/叶子各配不同阈值）。
// defaultBudget<=0 关闭 token 触发，仅靠 WithCompression 步频兜底。
func (p *Pipeline) WithContextBudget(defaultBudget int, perRole map[string]int) *Pipeline {
	p.contextBudget = defaultBudget
	p.contextBudgetPerRole = perRole
	return p
}

// WithTokenEstimator 注入消息切片 token 估算器。
// bootstrap 注入 agent.EstimateMessagesTokens，避免 domain/memory 反向依赖 model 包。
// 为 nil 时关闭 token 触发压缩，仅步频兜底。
func (p *Pipeline) WithTokenEstimator(f func([]agent.ReactMessage) int) *Pipeline {
	p.tokenEstimator = f
	return p
}

// WithHistorySummarizer 注入层级压缩摘要器（轻量模型）。传 nil 关闭 LLM 压缩包路径，
// 降级为截断式压缩（旧行为）。摘要失败时同样降级，不影响主流程。
func (p *Pipeline) WithHistorySummarizer(f HistorySummarizer) *Pipeline {
	p.historySummarizer = f
	return p
}

// WithMaxBundles 配置每个 agent 的压缩包数量上限；超限时合并最老的一半为 1 个包。
// n<=0 时回退 DefaultMaxBundles，避免错误配置导致金字塔无限增长或无法压缩。
func (p *Pipeline) WithMaxBundles(n int) *Pipeline {
	if n <= 0 {
		n = DefaultMaxBundles
	}
	p.maxBundles = n
	return p
}

// resolveContextBudget 返回 roleID 的上下文 token 阈值：
// perRole 命中优先，否则 contextBudget；<=0 表示不限制（不按 token 触发）。
func (p *Pipeline) resolveContextBudget(roleID string) int {
	if p.contextBudgetPerRole != nil {
		if v, ok := p.contextBudgetPerRole[roleID]; ok && v > 0 {
			return v
		}
	}
	return p.contextBudget
}

// Assemble 把 agent 的近期事件作为一条 system 角色上下文消息注入到历史记录中。
// 返回的新切片不会修改传入的 history 参数，调用方可以安全复用原切片。
//
// 历史压缩（层级压缩金字塔，冻结视图版，见 pyramid.go）：
//   - 每 compressEvery 步（或 token 达阈值）触发一次压缩：新滑出保留段的中段历史
//     压成一个结构化压缩包（LLM 摘要，失败降级截断），追加到该 agent 的压缩包列表；
//   - 压缩包数量超 maxBundles 时，最老的一半合并为 1 个更粗的包，循环往复——
//     旧上下文以逐级变粗的形式保留，不再 200 字符截断后等同丢弃；
//   - 两次压缩之间每轮都复用同一冻结视图（压缩包列表与保留段起点不变），history 只在尾部追加，
//     发给模型的消息前缀字节级稳定——DeepSeek 前缀缓存仅压缩那一轮失效，
//     其余轮次全部命中（旧实现每轮从全量 history 重算摘要，前缀每压缩轮即被打断）；
//   - 压缩状态随压缩触发落库（agent_compress_states），进程重启后懒加载恢复；
//   - 近期事件注入永远放在视图末尾（内容每轮变化，尾部变化不破坏前缀缓存）。
func (p *Pipeline) Assemble(role types.RoleDefinition, agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	return p.injectEvents(agentID, p.compressedView(role.ID, agentID, history))
}

// compressedView 返回该 agent 的压缩视图：未触发过压缩时原样返回 history；
// 触发过压缩后返回冻结视图（system 前缀 + 首条 user + 摘要消息 + history[tailStart:]）。
//
// 触发条件（二者或）：
//   - token 阈值：估算候选视图 token >= roleID 的上下文阈值（主，与上下文实际大小挂钩）；
//   - 步频兜底：step%every==0（防估算偏差或 estimator 未注入时缺位）。
//
// 压缩只作用于 history 本体；近期事件消息在 Assemble 里于压缩之后追加，
// 不会进入保留段、也不会在下个周期被压进中段摘要。
func (p *Pipeline) compressedView(roleID, agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	if p.compressEvery <= 0 && p.tokenEstimator == nil {
		// 压缩触发全关：仍尝试恢复已持久化的压缩视图（重启场景），无状态则原样返回。
		if st, ok := p.loadCompressStateOnce(agentID, len(history)); ok && st.TailStart <= len(history) {
			return buildCompressedView(history, st)
		}
		return history
	}

	p.mu.Lock()
	p.compressCounters[agentID]++
	step := p.compressCounters[agentID]
	p.mu.Unlock()

	// 候选视图：现冻结状态应用后的视图（或原 history）。token 触发据此判定。
	p.mu.RLock()
	st, ok := p.compressStates[agentID]
	p.mu.RUnlock()
	if !ok {
		// 重启后内存无状态：尝试从 store 懒加载压缩金字塔（每 agent 只试一次）。
		// TailStart 下标语义依赖全量 history 同时被恢复（MetaAgent 走 agent_messages），
		// 未恢复全量历史时加载结果会因 TailStart 越界被丢弃，退化为不压缩，安全。
		st, ok = p.loadCompressStateOnce(agentID, len(history))
	}
	var candidate []agent.ReactMessage
	if ok && st.TailStart <= len(history) {
		candidate = buildCompressedView(history, st)
	} else {
		candidate = history
	}

	threshold := p.resolveContextBudget(roleID)
	overBudget := threshold > 0 && p.tokenEstimator != nil && p.tokenEstimator(candidate) >= threshold
	stepHit := p.compressEvery > 0 && step%p.compressEvery == 0
	if overBudget || stepHit {
		// 层级压缩：把新滑出保留段的中段历史压成一个压缩包并冻结新视图（见 pyramid.go）。
		p.advanceCompression(agentID, history)
	}

	p.mu.RLock()
	st, ok = p.compressStates[agentID]
	p.mu.RUnlock()
	// history 在单次运行内只增不减；tailStart 越界说明状态陈旧（如外部重建 history），原样返回。
	if !ok || st.TailStart > len(history) || len(st.Bundles) == 0 {
		return history
	}
	return buildCompressedView(history, st)
}

// injectEvents 完成实际的事件注入工作：先按 agentID 取事件，再截取最近 limit 条，
// 格式化为文本后拼装成一条 system 消息并追加到 history 末尾（不破坏前缀缓存）。
func (p *Pipeline) injectEvents(agentID string, history []agent.ReactMessage) []agent.ReactMessage {
	if agentID == "" {
		// agentID 为空无法定位事件列表，直接返回原始历史，不做任何注入
		return history
	}

	p.mu.RLock()
	// 获取该 agent 在内存中的全部事件；这里只是读指针，不需要深拷贝
	events := p.events[agentID]
	p.mu.RUnlock()

	if len(events) == 0 {
		// 重启后内存无事件：尝试从 store 懒加载（每 agent 只试一次），恢复"近期事件"连续性。
		events = p.loadEventsOnce(agentID)
	}

	if len(events) == 0 {
		// 没有事件时无需生成上下文消息，避免插入空内容
		return history
	}

	// 计算需要展示的事件起始下标，保证最多只取 limit 条最新的
	start := 0
	if len(events) > p.limit {
		start = len(events) - p.limit
	}
	// summary 收集每个事件格式化后的简短文本
	var summary []string
	for _, ev := range events[start:] {
		// 对单条事件按类型格式化并追加到 summary
		summary = append(summary, formatEvent(ev))
	}

	// 注入文本：事件数超过阈值且配置了摘要器时，调轻量模型压缩；
	// 否则直接 join 原始事件文本。摘要失败降级为直接 join，不影响主流程。
	body := joinNonEmpty("\n", summary)
	if p.summarizer != nil && len(summary) > eventSummarizeThreshold {
		// 若连续 2 次 Insufficient Balance，关闭摘要路径降级到 raw join 直到进程重启，
		// 避免每次调用白等超时（实证 DeepSeek 余额耗尽持续 402）。
		if !p.summarizerDisabled() {
			// 超时取装配层注入值（默认 5s 仅适合秒回模型；思考型模型需 60-180s）。
			// 超时立即降级 raw join，避免主循环无限卡顿。
			timeout := p.summarizeTimeout
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			summarized, err := p.summarizer(ctx, summary)
			cancel()
			if err != nil {
				p.noteSummarizeError(agentID, err)
				// 摘要失败：记录警告，降级为原始 join，主流程不中断。
				slog.Warn("pipeline: summarize events failed, fallback to raw join",
					"agent_id", agentID, "event_count", len(summary), "err", err)
			} else if trimmed := strings.TrimSpace(summarized); trimmed != "" {
				// 摘要成功：重置连续错误计数。
				p.noteSummarizeSuccess()
				// 摘要成功且非空：用摘要替换原始 body，显著降低 token 占用。
				// 保留原始事件数标注，便于 LLM 识别这是压缩后的快照。
				body = fmt.Sprintf("[已摘要 %d 条事件] %s", len(summary), trimmed)
			}
		}
	}

	// ctxMsg 是注入到上下文的 system 消息，头部用中文标签便于大模型识别。
	// 放在 history 末尾而非开头：内容每轮随事件增长变化，放末尾不破坏前缀缓存
	// （DeepSeek 自动前缀缓存命中 system 指令 + history 前缀，events 摘要位于不可缓存尾部）。
	// 截断兜底（TODO #33）：摘要器缺失/失败走 raw join 时全文可能极长，超限截断防上下文膨胀。
	if r := []rune(body); len(r) > maxRawEventJoinRunes {
		body = string(r[:maxRawEventJoinRunes]) + fmt.Sprintf("\n...（近期事件超过 %d 字，已截断；只保留最新 %d 条事件中的前缀部分）", maxRawEventJoinRunes, len(summary))
	}
	ctxMsg := agent.ReactMessage{
		Role:    "system",
		Content: "【近期事件】\n" + body,
	}
	// out 预先按 history 长度 +1 分配容量，减少扩容开销
	out := make([]agent.ReactMessage, 0, len(history)+1)
	// 先追加原始历史消息（构成稳定前缀，供 DeepSeek 前缀缓存命中）
	out = append(out, history...)
	// 再把上下文消息放到末尾，避免每轮变化的内容破坏前缀缓存。
	out = append(out, ctxMsg)
	return out
}

// Write 把一条事件追加到指定 agent 的事件流中；如果配置了 Store，还会异步超时持久化。
func (p *Pipeline) Write(agentID string, event agent.MemoryEvent) error {
	if agentID == "" {
		// agentID 为空无法归属事件，直接忽略，不报错也不保存
		return nil
	}
	if event.Occurred.IsZero() {
		// 事件未设置发生时间时，自动填充当前时间，保证时间线完整
		event.Occurred = time.Now()
	}

	p.mu.Lock()
	// 在内存中追加事件：直接 append 到该 agent 对应切片末尾
	p.events[agentID] = append(p.events[agentID], event)
	// 超过每 agent 容量上限时丢弃最旧事件，仅保留最新 maxEventsPerAgent 条，防止内存无限增长
	if p.maxEventsPerAgent > 0 && len(p.events[agentID]) > p.maxEventsPerAgent {
		p.events[agentID] = p.events[agentID][len(p.events[agentID])-p.maxEventsPerAgent:]
	}
	p.mu.Unlock()

	if p.store != nil {
		// 创建 5 秒超时的后台上下文，避免持久化操作无限阻塞
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel() // 函数退出时取消上下文，释放资源
		if err := p.store.SaveEvent(ctx, agentID, event); err != nil {
			// 持久化失败向上返回错误，调用方可据此重试或记录日志
			return fmt.Errorf("persist event: %w", err)
		}
	}
	return nil
}

// Events 返回指定 agent 事件的深拷贝副本，主要用于测试和诊断。
func (p *Pipeline) Events(agentID string) []agent.MemoryEvent {
	p.mu.RLock()
	// defer 在 return 后释放读锁，避免函数多返回点遗漏解锁
	defer p.mu.RUnlock()
	// 创建与内部切片等长的新切片
	out := make([]agent.MemoryEvent, len(p.events[agentID]))
	// copy 实现深拷贝（切片元素 agent.MemoryEvent 为值类型，复制后互不影响）
	copy(out, p.events[agentID])
	return out
}

// formatEvent 根据事件类型把 MemoryEvent 转换成适合上下文展示的短文本。
func formatEvent(ev agent.MemoryEvent) string {
	// 按事件类型分支处理，输出统一格式的摘要字符串
	switch ev.Type {
	case "answer":
		// 回答类型：展示 agentID 与回答内容摘要
		return fmt.Sprintf("[%s] answer: %s", ev.AgentID, truncate(ev.Content, 120))
	case "tool_call":
		// 工具调用类型：展示工具名、输入输出摘要，便于回溯调用链
		return fmt.Sprintf("[%s] tool=%s input=%s output=%s", ev.AgentID, ev.ToolName, truncate(ev.Input, 80), truncate(ev.Output, 120))
	case "call_sub_agent":
		// 调用子智能体类型：展示目标角色与触发内容摘要
		return fmt.Sprintf("[%s] called sub-agent %s (%s)", ev.AgentID, ev.Role, truncate(ev.Content, 120))
	case "sub_agent_summary":
		// 子智能体汇总类型：展示子智能体角色与返回摘要
		return fmt.Sprintf("[%s] sub-agent %s summary: %s", ev.AgentID, ev.Role, truncate(ev.Content, 120))
	default:
		// 未知或自定义类型：展示类型名与内容摘要
		return fmt.Sprintf("[%s] %s: %s", ev.AgentID, ev.Type, truncate(ev.Content, 120))
	}
}

// truncate 将字符串 s 截断到最多 n 个字符，超过时尾部补 "..." 表示省略。
func truncate(s string, n int) string {
	if len(s) <= n {
		// 长度未超限，原样返回
		return s
	}
	// 超过限制则保留前 n 个字符并附加省略号
	return s[:n] + "..."
}

// joinNonEmpty 用分隔符 sep 连接切片中非空的字符串片段。
func joinNonEmpty(sep string, parts []string) string {
	// 第一轮遍历过滤掉空字符串，避免输出中出现多余分隔符
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	// 第二轮遍历用 sep 拼接非空片段，仅在 i>0 时添加分隔符
	var result string
	for i, p := range out {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}

// compressBoundary 计算历史压缩的边界：首条 user 下标与保留段起点。
//   - 首条 user 消息原样保留（任务目标，防"失忆"）；无 user 消息时 ok=false（不压缩）；
//   - 最近 K 条原样保留（含 tool_call/tool_result 对，边界避开 tool 起刀）。
//     不锚 user 边界：纯工作段（assistant/tool 交替）无 user，锚 user 会走空保留段
//     （实证：windowMessages 同款缺陷致塔防配置 Agent 上下文塌缩成 2 条失忆空转）。
//
// keepRecent<=0 视为 10；消息总数不足时 ok=false。
func compressBoundary(messages []agent.ReactMessage, keepRecent int) (firstUserIdx, recentStart int, ok bool) {
	if keepRecent <= 0 {
		keepRecent = 10
	}
	if len(messages) <= keepRecent+2 {
		return 0, 0, false
	}

	// 1) system 前缀
	keep := 0
	for keep < len(messages) && messages[keep].Role == "system" {
		keep++
	}

	// 2) 首条 user（任务目标）。若无 user（仅 system），不压缩。
	firstUserIdx = -1
	for i := keep; i < len(messages); i++ {
		if messages[i].Role == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx < 0 {
		return 0, 0, false
	}

	// 3) 最近 K 条边界：避开孤立的 tool 结果起刀（tool 结果须跟随其 assistant tool_calls）。
	if keepRecent > len(messages)-firstUserIdx-1 {
		keepRecent = len(messages) - firstUserIdx - 1
	}
	recentStart = len(messages) - keepRecent
	for recentStart < len(messages) && messages[recentStart].Role == "tool" {
		recentStart++
	}
	return firstUserIdx, recentStart, true
}

// renderTruncated 把一段消息按行渲染为截断文本：user 消息保前 500 字符
// （指令语义不可压，TODO #34/#35 结论），其余 200 字符。供截断式压缩降级路径复用。
func renderTruncated(sb *strings.Builder, messages []agent.ReactMessage) {
	const (
		midChunkMax     = 200
		midUserChunkMax = 500
	)
	for _, m := range messages {
		role := m.Role
		if role == "" {
			role = "?"
		}
		content := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
		if content == "" && len(m.ToolCalls) > 0 {
			content = fmt.Sprintf("[tool_calls: %d]", len(m.ToolCalls))
		}
		limit := midChunkMax
		if role == "user" {
			limit = midUserChunkMax
		}
		if r := []rune(content); len(r) > limit {
			content = string(r[:limit]) + "…"
		}
		fmt.Fprintf(sb, "- [%s] %s\n", role, content)
	}
}

// compressMiddle 计算历史的中段压缩摘要与保留段起点（截断式，单次全段）。
// 用于每 N 步触发一次的上下文压缩，避免长任务 token O(N²) 增长。
//
// 压缩规则：
//   - system 前缀全保留（若存在；记忆流水线已将近期事件移至末尾，此处通常无 system 前缀）；
//   - 首条 user 消息原样保留（任务目标，防"失忆"）；
//   - 中段压缩（TODO #34/#35 结论）：assistant/tool 消息压成 "[role] 前 200 字符"，
//     **user 消息保前 500 字符**——用户指令语义不可压（多轮会话第二条指令如"重新执行/
//     自检"被压成 200 字符是"指令歧义→全量重跑"事故的直接推手）；
//   - 最近 K 条原样保留（边界规则见 compressBoundary）。
//
// 返回 summary 摘要正文、tailStart 保留段起点（messages[tailStart:] 原样保留）。
// keepRecent<=0 视为 10；消息总数不足或无 user 消息时 ok=false（不压缩）。
//
// 该函数从 react_agent.go 迁入，职责归位到记忆层。ReActAgent 不再直接做历史压缩。
// 现为层级压缩（pyramid.go）的截断降级底仓，供无 LLM 摘要器时整段压平使用与测试直调。
func compressMiddle(messages []agent.ReactMessage, keepRecent int) (summary string, tailStart int, ok bool) {
	firstUserIdx, recentStart, ok := compressBoundary(messages, keepRecent)
	if !ok {
		return "", 0, false
	}

	// 中段暴力压缩：user 消息保前 500 字符（指令语义不可压），其余 200。
	middle := messages[firstUserIdx+1 : recentStart]
	var sb strings.Builder
	sb.WriteString("【历史压缩摘要】\n")
	renderTruncated(&sb, middle)
	sb.WriteString("\n（以上为早期对话压缩摘要，关键结论见下方近期事件与下方最近消息）")
	return sb.String(), recentStart, true
}

// buildCompressedView 按冻结状态拼装压缩视图：
// system 前缀 + 首条 user 任务目标 + 压缩金字塔消息 + messages[tailStart:]（原样保留段）。
// 同一 compressState 下输出前缀字节级稳定（DeepSeek 前缀缓存命中）；
// 调用方保证 st.TailStart <= len(messages)。无首条 user 或无压缩包时原样返回（防御）。
func buildCompressedView(messages []agent.ReactMessage, st compressState) []agent.ReactMessage {
	keep := 0
	for keep < len(messages) && messages[keep].Role == "system" {
		keep++
	}
	firstUserIdx := -1
	for i := keep; i < len(messages); i++ {
		if messages[i].Role == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx < 0 || st.TailStart < firstUserIdx+1 || len(st.Bundles) == 0 {
		return messages
	}

	out := make([]agent.ReactMessage, 0, keep+2+(len(messages)-st.TailStart))
	out = append(out, messages[:keep]...)
	out = append(out, messages[firstUserIdx]) // 首条 user 任务目标
	out = append(out, agent.ReactMessage{Role: "system", Content: renderBundles(st.Bundles)})
	out = append(out, messages[st.TailStart:]...)
	return out
}
