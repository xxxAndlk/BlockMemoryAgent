// Package agent 实现单代理的 ReAct（推理-行动）循环：
// LLM 生成 -> 工具调用 -> 工具结果 -> 重复，直到产生最终答案或达到迭代上限。
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/middleware"
	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/pkg/textutil"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/tools"
)

// ReActAgent 实现单个 ReAct 循环：LLM 生成 -> 工具调用 -> 工具结果 -> 重复。
// 它在多次运行之间刻意保持无状态；所有可变状态都保存在传入并返回的
// History 切片中，便于上层按需持久化或续跑。
type ReActAgent struct {
	llm ModelProvider // llm 是当前使用的语言模型提供者，负责生成回复。
	// providerFn 可选的 provider 每次调用重解析函数（WithProviderFunc 注入）：generateOnce
	// 开头按当前角色模型绑定重取 provider 并替换 a.llm，使运行中的 Agent 在下一次 LLM
	// 调用即用上新模型（否则热驻槽/长任务把 provider 钉在构造时刻）。解析失败保留旧值。
	providerFn func(context.Context) (ModelProvider, error)
	tools      ToolRegistry         // tools 是已注册的工具集合，提供 JSON Schema 与分发执行能力。
	memory     MemoryPipeline       // memory 是记忆流水线，用于在每次 LLM 调用前组装上下文、写入事件。
	mailbox    *mailbox.Mailbox     // mailbox 是共享邮箱，用于接收异步子代理摘要；为空时不轮询。
	role       types.RoleDefinition // role 是当前代理的角色定义，包含系统提示等配置。
	name       string               // name 是代理唯一标识，也用于上下文中的 agent ID。
	maxIter    int                  // maxIter 是单次 Run 中允许的最大 LLM 调用次数，防止死循环。
	// sysPromptOnce/sysPromptCache 冻结 systemPrompt 结果（TODO #40 块 4）：
	// 环境块（含 PROJECT.md）与画像/人格在 Agent 存活期内字节稳定，跨轮前缀命中
	// DeepSeek 缓存；resume 重建新实例时读到最新文件。sync.Once 保证并发安全。
	sysPromptOnce  sync.Once
	sysPromptCache string

	// llmTimeout 是单次 LLM 调用的超时；<=0 时仅受会话 ctx 取消控制。
	llmTimeout time.Duration
	// retryCount 是 LLM 调用失败后的重试次数（不含首次）。
	retryCount int
	// retryBackoff 是重试初始退避时长，每次重试翻倍。
	retryBackoff time.Duration
	// historyMaxMessages 是单次 LLM 请求携带的最大历史消息数（滑动窗口）；<=0 不裁剪。
	historyMaxMessages int
	// toolOutputMaxRunes 是写入历史的单条工具输出最大字符数；<=0 不截断。
	toolOutputMaxRunes int
	// toolResultDumpRunes 工具结果统一收口阈值（TODO 第9项②，rune 计数）：单条工具输出
	// 超该阈值时全文落盘 <workDir>/.bma/tool_outputs/，历史只留头部摘录 + 【全文已落盘】
	// 路径，模型按需 ReadFile 取全文。落盘失败降级原样（回落 toolOutputMaxRunes 截断）。
	// <=0 关闭收口。bootstrap 从 config tool_result_dump_runes 注入。
	toolResultDumpRunes int
	// toolResultDigestRunes 收口后保留的头部摘录 rune 数；<=0 按默认 2000。
	toolResultDigestRunes int
	// staleToolEvictRounds 陈旧工具结果驱逐轮数（TODO 第9项③）：请求构建期把 N 轮前
	//（按 assistant 轮序）的 ReadFile/SearchInFiles 结果替换为"已驱逐需重读"占位符，
	// 只影响本次请求视图，canonical history 与持久化不动。<=0 关闭。
	staleToolEvictRounds int
	// agentsMDMaxRunes AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune 计数）。
	// workDir 根部 AGENTS.md 优先、其次 CLAUDE.md，注入 envBlock【项目自述】段。
	// <=0 关闭注入。bootstrap 从 config agents_md_max_runes 注入。
	agentsMDMaxRunes int
	// parallelTools 轮内并行工具执行开关（TODO 第9项①）：同轮多个 tool_calls 并发派发
	//（信号量限 toolParallelMax），结果按原序串行回填。零值=关闭（测试稳定），
	// 生产配置 tool_parallel_enabled: true 打开。
	parallelTools bool
	// toolParallelMax 并行工具并发上限；<=0 用默认 defaultToolParallelConcurrency。
	toolParallelMax int
	// pathMu/pathMutexes 轮内并行的同路径写互斥锁表：写类工具按目标路径分键
	//（相对路径按 agent workDir 解析，worktree 副本天然不同键），同路径串行、
	// 异路径并行。仅并行派发路径使用；串行路径零开销。
	pathMu      sync.Mutex
	pathMutexes map[string]*sync.Mutex
	// dispatchMu 轮内并行的编排类工具互斥锁（P0-1：同轮并发 call_sub_agent 使
	// dispatcher 同领域查重的"查树快照→Register"临界区 TOCTOU 失效，可产生两个同名
	// 并行 domain 互写同批文件）。编排类工具只做起始登记（毫秒级），串行化不损并行度。
	dispatchMu sync.Mutex
	// stableHashLast 上次记录的 stable 层（系统指令）内容哈希（TODO 第10项①缓存纪律）。
	// stable 段按实例 sync.Once 冻结，跨轮哈希应恒定；变化即 WARN（防回归断言）。
	// 仅 Run 循环 goroutine 读写，无需加锁。
	stableHashLast string
	// tokenBudget 上下文 token 阈值（替换原累计跨轮 token 预算）：Assemble 压缩后
	// 估算 messages token >= 阈值即 LimitReached（近 N 单独就超、压不下去），暂停等续跑。
	// <=0 不限制。默认 150000（service_react roleTokenBudget 按角色配）。
	tokenBudget int
	// liveFn 是实时进度事件回调，由 WithLiveEvents 注入；nil 时不推送任何进度事件。
	liveFn func(LiveEvent)
	// pendingChecker 可选的未决子 Agent 检查器，由 WithPendingChildrenChecker 注入；
	// 为 nil 时关闭终结保护（默认关闭，仅在 bootstrap 装配 Dispatcher 后开启）。
	pendingChecker PendingChildrenChecker
	// log 是会话级日志器，用于记录每次 LLM 调用的完整 prompt/response 到 session_logs。
	// 为 nil 时跳过 LLM I/O 日志（不影响主流程）。
	log *logger.Logger
	// workDir 是当前 Agent 的工作目录，注入到系统提示词中供 LLM 使用相对路径。
	// 为空时回退到进程 cwd（systemPrompt 中仍会显示）。
	workDir string
	// persona 可选的人格注入器（soul.Loader 实现该接口）；为 nil 时不注入人格前缀。
	// 在 systemPrompt() 头部把人格内容拼到环境块之前，使所有 Agent 共享用户级人格。
	persona PersonaInjector
	// skillBlock 技能元数据块（渐进披露第一层：仅名称+一句话描述），由 WithSkillBlock
	// 在构造期注入（一次性 Agent 在派发时、Meta 在会话构建时）。追加在系统提示词
	// 纪律块之后 = 只 fork 提示词尾部，envBlock+base 公共前缀跨 Agent 前缀缓存不受影响。
	// 正文获取经 load_skill 工具按需进行，整块内容不随提示词重复展开。
	skillBlock string
	// memoryIndex 记忆索引槽（TODO #20③+#22③）：会话启动注入的一行式沉淀索引
	//（renderMemoryIndex 渲染，行数/runes 双配额），详情走向量召回。同 skillBlock
	// 尾部注入位（前缀缓存安全）；空串=未接线/无沉淀。
	memoryIndex string
	// pausedChecker 可选的"是否有 Paused 子 DomainAgent"检查器，由 WithPausedChildChecker 注入。
	// 父终结保护 wait loop 中检查：若有 Paused 子节点（触达 token 上限），父 MetaAgent
	// 无限 budget 不会自行暂停，需靠此检查跳出 wait loop 返回 PausedOnChild，由上层 pauseSession
	// 置会话暂停态。为 nil 时不检查（默认关闭，仅 MetaAgent 注入）。
	pausedChecker PausedChildChecker
	// suspendOnChildWait 顶层 Agent 挂起等子语义（WithSuspendOnChildWait 注入）：终答轮
	// 仍有未决子 Agent 时置 awaiting_child 立即返回，而非原地阻塞 waitForChildren。
	// 仅会话顶层 Agent（runSession/resumeSession 构造）开启；dispatcher 子 Agent 保持
	// false（其运行由 runSubAgent 同步承载，无会话态可落，提前返回会被当终答回传）。
	// 原按 role.ID=="meta" 硬判——daily 档（domain 顶层）放开为显式开关。
	suspendOnChildWait bool
	// activityReporter 可选的活动上报回调，由 Dispatcher 心跳巡检注入。
	// 语义化 kind 上报（TODO 第10项②证据化）：llm_start/llm_end（LLM 调用首尾）、
	// tool:<名>/tool_end（工具派发首尾）、stream（流式 chunk）、keepalive（保活 tick，
	// 证明进程活着但不刷新 lastTS）。Dispatcher 侧据此区分"真静默"与"在飞 LLM/长工具"。
	// 为 nil 时跳过（测试场景或未注入巡检的 Agent），不影响主流程。
	activityReporter func(kind string)
	// streamKeepalive 流式生成期间的保活上报间隔：thinking 模型首 token 前可能长时间
	// 静默（零 chunk 无上报，实证 42K 输入 5m58s 无首 chunk 被心跳误杀），
	// 定时器补上报防误判；真实挂死由 sub_agent_timeout 墙钟兜底。<=0 默认 30s。
	streamKeepalive time.Duration
	// streamIdleTimeout 流式块间空闲超时：首块之后 chunk 间隔超过它即判流死，
	// 取消流让 generate 重试（2026-08-20 ark glm-5.3 一天三次流中途静默）。
	// <=0 用 defaultStreamIdleTimeout（300s）。
	streamIdleTimeout time.Duration
	// streamFirstChunkTimeout 首块前独立卡口（任务137）：健康端点首 chunk（含思考
	// 增量）秒级到达，5min 才来首块 = 端点假连接（实证 2026-09-08 domain-4/5/6
	// 三连 5min 零首块，重试再烧 5min，10min 墙钟全烧光零交付）。首块前与首块后
	// 分开设卡：首块前用短卡口快速判死重试，首块后仍用 300s 容忍长生成停顿。
	// <=0 用 defaultStreamFirstChunkTimeout（120s）。
	streamFirstChunkTimeout time.Duration
	// suspendGate 可选的会话级挂起检查点，由 WithSuspendGate 注入。
	// 主循环顶部与 waitForChildren 内调用 Park：会话挂起期间阻塞（goroutine 真挂起），
	// 恢复返回 nil 继续。为 nil 时零变化（热驻关闭时不注入）。
	suspendGate SuspendGate
	// msgLogger 可选的消息热层记录器（编排页对话视图）：每条入史消息同步热写。
	// nil 时零行为。
	msgLogger MessageLogger
	// promptStatsSegs 提示词构成分段计量（TODO #15①，rune 数）：buildSystemPrompt
	// 在 sysPromptOnce 内写入（单 goroutine，无竞争）——persona/env/role_base/discipline/skill
	// 各分段规模。首轮 LLM 调用经 logPromptStats 以明细行输出，供提示词瘦身边际对照。
	promptStatsSegs map[string]int
}

// PersonaInjector 把人格内容拼接到系统提示词之前；soul.Loader 实现该接口。
// 接口分离避免 agent 包反向依赖 soul 包；为 nil 时 systemPrompt 原样返回。
type PersonaInjector interface {
	// Inject 返回拼入人格前缀后的系统提示词；人格为空时原样返回 systemPrompt。
	Inject(systemPrompt string) string
}

// CombinePersonaInjectors 顺序组合多个注入器（TODO #28 用户画像）：
// MetaAgent 需要"人格 + 用户画像"两段前缀，子 Agent 只需人格（dispatcher 注入 soul 单一注入器，
// 画像不下发子 Agent）。任一注入器为 nil 时跳过。
func CombinePersonaInjectors(injs ...PersonaInjector) PersonaInjector {
	var live []PersonaInjector
	for _, i := range injs {
		if i != nil {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return &compositeInjector{injs: live}
}

type compositeInjector struct {
	injs []PersonaInjector
}

func (c *compositeInjector) Inject(systemPrompt string) string {
	for _, i := range c.injs {
		systemPrompt = i.Inject(systemPrompt)
	}
	return systemPrompt
}

// NewUserProfileInjector 构造用户画像注入器（TODO #28 第四层记忆）：
// 每次 Inject 读取画像全文，以【用户画像】前缀拼入系统提示词（带 rune 上限截断防膨胀）。
// 仅注入 MetaAgent（runSession/resumeSession）；子 Agent 不注入（画像不下发）。
// current 为 nil 或返回空串时原样返回（零副作用）。
func NewUserProfileInjector(current func() string, maxRunes int) PersonaInjector {
	return NewSectionInjector("【用户画像】", current, maxRunes)
}

// NewSectionInjector 构造通用文本段注入器（2026-09-02 偏好与自进化）：
// 以 header 为前缀把 current() 全文拼入系统提示词（rune 截断防膨胀）。
// 用户画像与项目偏好共用；current 为 nil 或返回空串时原样返回。
func NewSectionInjector(header string, current func() string, maxRunes int) PersonaInjector {
	if current == nil || strings.TrimSpace(header) == "" {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 2000
	}
	return &sectionInjector{header: header, current: current, maxRunes: maxRunes}
}

type sectionInjector struct {
	header   string
	current  func() string
	maxRunes int
}

func (p *sectionInjector) Inject(systemPrompt string) string {
	content := strings.TrimSpace(p.current())
	if content == "" {
		return systemPrompt
	}
	if len([]rune(content)) > p.maxRunes {
		content = string([]rune(content)[:p.maxRunes]) + "\n...（截断）"
	}
	return p.header + "\n" + content + "\n\n" + systemPrompt
}

// LoopConfig 是 ReAct 主循环的运行时参数，由 WithLoopConfig 注入。
// 各字段 <=0 的语义见 ReActAgent 对应字段注释。
type LoopConfig struct {
	MaxIterations      int           // 最大 LLM 轮数；<=0 不限制
	LLMTimeout         time.Duration // 单次 LLM 调用超时；<=0 仅受会话取消控制
	RetryCount         int           // 失败重试次数（不含首次）；<0 视为 0
	RetryBackoff       time.Duration // 重试初始退避；<=0 用默认 100ms
	HistoryMaxMessages int           // 单次请求最大历史消息数；<=0 不裁剪
	ToolOutputMaxRunes int           // 写入历史的工具输出最大字符数；<=0 不截断
	// TokenBudget 上下文 token 阈值（替换原累计跨轮 token 预算）：Assemble 压缩后
	// 估算 messages token >= 阈值即 LimitReached（近 N 单独就超、压不下去）。<=0 不限制。
	TokenBudget int
	// ToolResultDumpRunes 工具结果统一收口阈值（TODO 第9项②，rune）：单条工具输出超该值
	// 全文落盘 .bma/tool_outputs/，历史只留头部摘录 + 【全文已落盘】路径。<=0 关闭。
	ToolResultDumpRunes int
	// ToolResultDigestRunes 收口后保留的头部摘录 rune 数；<=0 按默认 2000。
	ToolResultDigestRunes int
	// StaleToolEvictRounds 陈旧只读工具结果（ReadFile/SearchInFiles）驱逐轮数（TODO 第9项③）；
	// 请求构建期替换为"已驱逐需重读"占位符，不改 canonical history。<=0 关闭。
	StaleToolEvictRounds int
	// AgentsMDMaxRunes AGENTS.md/CLAUDE.md 项目自述注入上限（TODO 第10项⑦，rune）。<=0 关闭。
	AgentsMDMaxRunes int
	// ToolParallelEnabled 轮内并行工具执行开关（TODO 第9项①）：同轮多个 tool_calls
	// 并发派发、结果按原序串行回填。nil=关闭（零值稳定，生产配置默认 true）。
	ToolParallelEnabled *bool
	// ToolParallelMaxConcurrency 并行工具并发上限；<=0 用默认 4。
	ToolParallelMaxConcurrency int
}

// defaultToolParallelConcurrency 轮内并行工具执行的默认并发上限（TODO 第9项①）。
const defaultToolParallelConcurrency = 4

// NewReActAgent 根据具体角色构造一个 ReActAgent 实例。
// 参数 name 为代理标识；role 为角色定义；llm 为模型提供者；tools 为工具注册表。
func NewReActAgent(name string, role types.RoleDefinition, llm ModelProvider, tools ToolRegistry) *ReActAgent {
	// 使用构造参数与默认值填充结构体字段。
	return &ReActAgent{
		name:    name,
		role:    role,
		llm:     llm,
		tools:   tools,
		memory:  NopMemoryPipeline{}, // 默认使用空实现，避免 nil 调用 panic。
		maxIter: 50,                  // 默认最多 50 轮 LLM 调用。
	}
}

// WithProviderFunc 注入 provider 每次调用重解析函数：每次 LLM 调用前按最新模型绑定
// 重新解析 provider，使运行中的 Agent（含热驻槽复用、暂停续跑）在下一次调用即用上新模型。
// 传 nil 关闭（默认关闭，provider 固定在构造时刻）。
func (a *ReActAgent) WithProviderFunc(fn func(context.Context) (ModelProvider, error)) *ReActAgent {
	a.providerFn = fn
	return a
}

// WithMemory 注入记忆流水线。
// 参数 m 为要实现记忆逻辑的对象；如果传入 nil，则视为无操作记忆。
func (a *ReActAgent) WithMemory(m MemoryPipeline) *ReActAgent {
	// 显式处理 nil 值，确保内部 memory 字段始终非空，后续调用无需重复判空。
	if m == nil {
		a.memory = NopMemoryPipeline{}
	} else {
		a.memory = m
	}
	return a
}

// WithWorkDir 注入工作目录，会在系统提示词中作为环境信息暴露给 LLM。
// 用于让 LLM 用相对路径定位文件、判断 OS 上下文。空字符串表示回退到进程 cwd。
func (a *ReActAgent) WithWorkDir(wd string) *ReActAgent {
	a.workDir = wd
	return a
}

// WithSkillBlock 注入【可用技能】元数据块（构造期一次性，冻结进 systemPrompt 缓存）。
// 空串为零行为（未启用技能/无持有技能时 dispatcher 传空即自动跳过）。
func (a *ReActAgent) WithSkillBlock(block string) *ReActAgent {
	a.skillBlock = block
	return a
}

// WithMemoryIndex 注入【沉淀索引】块（TODO #20③，构造期一次性冻结进 systemPrompt）。
// 空串为零行为。索引超限时调用方（renderMemoryIndex）已带重写指令，不静默截断。
func (a *ReActAgent) WithMemoryIndex(block string) *ReActAgent {
	a.memoryIndex = block
	return a
}

// WithMailbox 注入共享邮箱，使主循环可以轮询异步子代理摘要。
// 参数 m 为共享邮箱实例；传入 nil 将禁用轮询。
func (a *ReActAgent) WithMailbox(m *mailbox.Mailbox) *ReActAgent {
	// 直接保存邮箱引用，RunWithHistory 中通过判空决定是否轮询。
	a.mailbox = m
	return a
}

// WithMaxIterations 限制单次运行中 LLM 调用的最大次数。
// 参数 n 为期望的上限；默认值是 50，传入非正数会恢复默认值。
func (a *ReActAgent) WithMaxIterations(n int) *ReActAgent {
	// 对非法输入做兜底，避免因为 0 或负数导致循环无法执行或逻辑异常。
	if n <= 0 {
		n = 50
	}
	a.maxIter = n
	return a
}

// WithLoopConfig 注入主循环运行时参数（轮数/超时/重试/历史滑窗/输出截断）。
func (a *ReActAgent) WithLoopConfig(c LoopConfig) *ReActAgent {
	a.maxIter = c.MaxIterations // <=0 表示不限制（循环条件已兼容）
	a.llmTimeout = c.LLMTimeout
	a.retryCount = c.RetryCount
	if a.retryCount < 0 {
		a.retryCount = 0
	}
	a.retryBackoff = c.RetryBackoff
	a.historyMaxMessages = c.HistoryMaxMessages
	a.toolOutputMaxRunes = c.ToolOutputMaxRunes
	a.tokenBudget = c.TokenBudget
	a.toolResultDumpRunes = c.ToolResultDumpRunes
	a.toolResultDigestRunes = c.ToolResultDigestRunes
	a.staleToolEvictRounds = c.StaleToolEvictRounds
	a.agentsMDMaxRunes = c.AgentsMDMaxRunes
	if c.ToolParallelEnabled != nil {
		a.parallelTools = *c.ToolParallelEnabled
	}
	a.toolParallelMax = c.ToolParallelMaxConcurrency
	if a.toolParallelMax <= 0 {
		a.toolParallelMax = defaultToolParallelConcurrency
	}
	return a
}

// WithLiveEvents 注入实时进度事件回调，用于向 UI 推送 LLM 流式输出与工具执行进度。
// 传 nil 表示关闭进度推送（默认关闭）。
func (a *ReActAgent) WithLiveEvents(fn func(LiveEvent)) *ReActAgent {
	a.liveFn = fn
	return a
}

// WithPendingChildrenChecker 注入未决子 Agent 检查器，开启父会话终结保护：
// 主循环在产生最终答复前会先查询 checker，若有未决子 Agent 则阻塞等待，
// 防止迟到 mailbox 消息随会话销毁丢失。传 nil 关闭保护（默认关闭）。
func (a *ReActAgent) WithPendingChildrenChecker(p PendingChildrenChecker) *ReActAgent {
	a.pendingChecker = p
	return a
}

// WithLogger 注入会话级日志器，使每次 LLM 调用的完整 prompt/response 落 session_logs，
// 供 TUI/Web 完整查看输入输出。传 nil 关闭 LLM I/O 日志（默认关闭）。
func (a *ReActAgent) WithLogger(l *logger.Logger) *ReActAgent {
	a.log = l
	return a
}

// WithPersonaInjector 注入人格注入器（soul.Loader），在 systemPrompt 头部拼入人格前缀。
// 传 nil 关闭人格注入（默认关闭）。人格为空时 systemPrompt 原样返回，无副作用。
func (a *ReActAgent) WithPersonaInjector(p PersonaInjector) *ReActAgent {
	a.persona = p
	return a
}

// WithPausedChildChecker 注入 Paused 子 Agent 检查器，用于父终结保护 wait loop。
// MetaAgent 无限 budget 不会因 token 暂停，需在有 Paused 子 DomainAgent 时主动暂停会话。
// 传 nil 关闭检查（默认关闭，仅 MetaAgent 注入）。
func (a *ReActAgent) WithPausedChildChecker(p PausedChildChecker) *ReActAgent {
	a.pausedChecker = p
	return a
}

// WithSuspendOnChildWait 开启顶层挂起等子语义（会话顶层 Agent 专用）：
// 终答轮仍有未决子 Agent 时返回 SuspendOnChildWait，由上层置 awaiting_child。
// 默认关闭（dispatcher 子 Agent 保持阻塞 waitForChildren 旧行为）。
func (a *ReActAgent) WithSuspendOnChildWait(v bool) *ReActAgent {
	a.suspendOnChildWait = v
	return a
}

// WithActivityReporter 注入活动上报回调，供 Dispatcher 心跳巡检判断子 Agent 是否假死。
// 语义化 kind 上报（TODO 第10项②证据化）：llm_start/llm_end/tool:<名>/tool_end/stream/
// keepalive；传 nil 关闭（默认关闭）。
// 仅叶子 Agent 注入：DomainAgent/MetaAgent 有自身 wait loop，注入会误杀合法等待。
func (a *ReActAgent) WithActivityReporter(fn func(kind string)) *ReActAgent {
	a.activityReporter = fn
	return a
}

// WithStreamKeepalive 设置流式保活上报间隔；<=0 用默认 30s。测试注入短间隔。
func (a *ReActAgent) WithStreamKeepalive(d time.Duration) *ReActAgent {
	a.streamKeepalive = d
	return a
}

// WithStreamIdleTimeout 设置流式块间空闲超时；<=0 用 defaultStreamIdleTimeout（300s）。
// 测试注入短间隔。仅对首块之后的 chunk 间隔生效。
func (a *ReActAgent) WithStreamIdleTimeout(d time.Duration) *ReActAgent {
	a.streamIdleTimeout = d
	return a
}

// WithSuspendGate 注入会话级挂起检查点。主循环顶部与 waitForChildren 内调用
// Park：挂起期间阻塞，恢复返回 nil 继续循环。传 nil 关闭（默认关闭，热驻模式
// 下由 dispatcher 注入给 domain 与叶子 Agent）。
func (a *ReActAgent) WithSuspendGate(g SuspendGate) *ReActAgent {
	a.suspendGate = g
	return a
}

// WithMessageLogger 注入消息热层记录器（编排页对话视图数据源）。传 nil 关闭。
func (a *ReActAgent) WithMessageLogger(l MessageLogger) *ReActAgent {
	a.msgLogger = l
	return a
}

// appendLogged 追加一条消息到 history 并同步热写消息日志（编排页对话视图）。
// seq = 追加前 history 长度，与 agent_messages 全量落库的下标口径一致。
func (a *ReActAgent) appendLogged(history []ReactMessage, msg ReactMessage) []ReactMessage {
	if a.msgLogger != nil {
		a.msgLogger.Log(a.name, len(history), msg)
	}
	return append(history, msg)
}

// touchActivity 按语义化 kind 上报一次活动（TODO 第10项②证据化）；
// 未注入回调时为空操作。kind 约定：llm_start/llm_end/tool:<名>/tool_end/stream/keepalive。
func (a *ReActAgent) touchActivity(kind string) {
	if a.activityReporter != nil {
		a.activityReporter(kind)
	}
}

// emitLive 发送一条实时进度事件；未注册回调时直接丢弃，Agent 标识在此统一填充。
func (a *ReActAgent) emitLive(ev LiveEvent) {
	if a.liveFn == nil {
		return
	}
	// 用 role.Name 作展示名（"MetaAgent"/"代码助手"），避免 MetaAgent 的 a.name=session-ID
	// 让日志全显 "session-1"。mailbox 路由仍用 a.name，与此处无关。
	ev.Agent = a.role.Name
	// AgentID 填实例 ID（MetaAgent=session ID，子 Agent=session-1/code_assistant-5），
	// 供会话层把 think/tool 事件归属到具体 Agent 实例（TUI 领域进度面板按此匹配）。
	ev.AgentID = a.name
	a.liveFn(ev)
}

// Run 针对给定的用户输入执行 ReAct 循环。
// 参数 ctx 用于取消/超时控制；input 为用户输入文本。
// 返回值 ReactResult 包含助手最终回复与完整会话历史；error 表示执行过程中的错误。
func (a *ReActAgent) Run(ctx context.Context, input string) (ReactResult, error) {
	// 委托给 RunWithHistory，从空历史开始新的会话。
	return a.RunWithHistory(ctx, input, nil)
}

// maxEmptyResponses 是允许的连续空响应次数上限：超过即判定模型异常，显式报错，
// 避免任务被"无声完成"（用户看到的就是任务莫名其妙中断）。
const maxEmptyResponses = 3

// emptyResponseNudge 是收到空响应时注入的用户提示，要求模型继续推进任务。
const emptyResponseNudge = "（系统提示：你上一条回复为空，未包含任何文本或工具调用。请继续推进当前任务；若任务确已全部完成，请直接输出完整的最终答复。）"

// maxBadToolCallResponses 是允许的连续"工具调用参数非法被丢弃"次数上限：超过即判定
// 模型异常，显式报错，而不是把伴随的中间陈述文本当作终答静默收官。
const maxBadToolCallResponses = 3

// badToolCallNudge 是工具调用参数 JSON 解析失败被丢弃后注入的用户提示（%s=被丢弃
// 调用的描述列表），要求模型修正参数后重新发起调用。
const badToolCallNudge = "（系统提示：你刚才发起的工具调用因参数不是合法 JSON 已被系统丢弃、未执行：%s。请修正参数后重新发起该工具调用（注意嵌套的 JSON 字符串必须正确转义），继续推进当前任务。）"

// historyToolCallInputMaxRunes 是写入历史的工具入参单字符串值最大 rune 数。
// 与 ToolOutputMaxRunes（工具输出截断）对称：工具入参（WriteFile 全文、codegen 大段代码）
// 不截断会在滑动窗口内累积成单轮 100K+ input tokens，使续跑预算一次耗尽、暂停/恢复零进展。
// 取值 12000（与 tool_output_history_max_runes 20000 同量级，保持硬编码、无对应配置项）：
// 原 2000 截得太狠，WriteFile/EditFile 刚写入的内容几轮后就从历史消失，而 EditFile 又要求
// old_string 逐字符一致，Agent 只能改前重读（实证：领域 Agent 反复 ReadFile 同一批文件）。
const historyToolCallInputMaxRunes = 12000

// defaultStreamIdleTimeout 是流式块间空闲超时默认值：首块之后 chunk 间隔超过它
// 即判流死（2026-08-20 ark glm-5.3 一天三次流中途静默，等满整次墙钟才报错代价太高）。
const defaultStreamIdleTimeout = 300 * time.Second

// defaultStreamFirstChunkTimeout 是首块前独立卡口默认值：健康端点首 chunk（含思考
// 增量）秒级到达，超过它 = 端点假连接，快速判死让 RetryLLM 重试而不是白烧墙钟。
const defaultStreamFirstChunkTimeout = 120 * time.Second

// truncateToolCallInputsForHistory 返回 assistant 消息的入史副本：
// ToolCalls 的 Input 中超长字符串值被截断（附原始长度标记），其余字段与原消息共享。
// 派发执行仍使用原消息，截断只影响历史持久化与后续请求的上下文回发。
func truncateToolCallInputsForHistory(m ReactMessage, maxRunes int) ReactMessage {
	if len(m.ToolCalls) == 0 || maxRunes <= 0 {
		return m
	}
	out := m
	out.ToolCalls = make([]ToolCall, len(m.ToolCalls))
	for i, tc := range m.ToolCalls {
		out.ToolCalls[i] = ToolCall{ID: tc.ID, Name: tc.Name, Input: truncateStringValues(tc.Input, maxRunes)}
	}
	return out
}

// truncateStringValues 返回 map 的浅拷贝，其中超过 maxRunes 的字符串值被截断并附长度标记。
func truncateStringValues(in map[string]any, maxRunes int) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			if r := []rune(s); len(r) > maxRunes {
				v = string(r[:maxRunes]) + fmt.Sprintf("...(truncated, total %d runes)", len(r))
			}
		}
		out[k] = v
	}
	return out
}

// RunWithHistory 从已有历史开始执行 ReAct 循环。
// 参数 ctx 用于取消/超时控制；input 为新的用户输入；history 为已有会话历史。
// 新输入会被追加到传入的历史中，便于会话续跑并保留之前的轮次。
// 返回值 ReactResult 包含最终回复与完整历史；error 表示执行错误。
func (a *ReActAgent) RunWithHistory(ctx context.Context, input string, history []ReactMessage) (ReactResult, error) {
	// 将当前代理标识写入上下文，便于链路追踪、日志和工具调用时识别身份。
	ctx = WithAgentID(ctx, a.name)
	// 注入角色 ID，供 WriteFile 角色级写沙箱（Layer 4）按角色限制写入路径。
	// 未配 Sandbox 的角色 enforceRoleWritePath 跳过，零开销。
	ctx = WithRoleID(ctx, a.role.ID)
	// 同步注入展示名（role.Name），供 handleToolEvent 写日志与 UI 时显示
	// "MetaAgent"/"代码助手" 而非 session-ID（"session-1"）。sub-agent 的
	// 展示名在 dispatcher 侧覆写 roleDef.Name 后同样经此注入。
	ctx = WithAgentDisplayName(ctx, a.role.Name)
	// 读图能力门控注入（2026-09-16）：ReadMedia / 图片透传兜底据此在"模型不支持图片输入"
	// 时改走文本说明，不再把图像塞进对话触发 provider 400。闭包实时读进程缓存，
	// 运行中换模型（set_agent_model）即自动恢复；子 Agent 走同一 RunWithHistory 同样注入。
	ctx = tool.WithImageInputSupported(ctx, func() bool { return !a.noImageInput() })

	// 把会话级 logger 挂到 ctx：轻量 LLM 调用（记忆流水线事件摘要等经
	// ModelFactory.CallLightweightWithRetry）链路只持有 ctx，由此取出 logger 写 session_logs。
	if a.log != nil {
		ctx = logger.NewContext(ctx, a.log)
	}

	// 如果外部传入 nil 历史，则初始化为空切片，保证后续 append 安全。
	if history == nil {
		history = []ReactMessage{}
	}

	// 将本轮用户输入作为一条 user 消息追加到历史中，开启新一轮 ReAct。
	// 当前轮用户图片（Alt+V 粘贴，ctx 带外注入）挂到该消息：meta 主会话与
	// 子 Agent 派发首条 user 消息共用此收口（dispatcher 侧重注入子 Agent ctx）。
	userMsg := ReactMessage{Role: "user", Content: input}
	if imgs := UserImagesFromContext(ctx); len(imgs) > 0 {
		userMsg.Images = imgs
	}
	history = a.appendLogged(history, userMsg)
	// 已知当前模型不支持图片输入：本轮图片不会进入请求（视图层剥图，见循环内
	// stripImagesForRequest），先给模型一条说明，避免它对着 [image:N] 占位符空想。
	if len(userMsg.Images) > 0 && a.noImageInput() {
		history = a.appendLogged(history, ReactMessage{Role: "user", Content: imageUnsupportedNotice(a.llmModelName())})
	}

	// 根据当前角色构建系统提示词，作为模型行为约束。
	system := a.systemPrompt()

	// 进入 ReAct 主循环，最多执行 maxIter 次 LLM 调用（maxIter<=0 时不限制）。
	// 每次循环对应一次“思考-行动-观察”的迭代。
	// emptyStreak 记录连续空响应次数，用于空响应保护（见循环内注释）。
	emptyStreak := 0
	// badToolCallStreak 记录连续"工具调用参数非法被丢弃"轮数（见循环内注释）。
	badToolCallStreak := 0
	// unproductiveStreak 记录连续"无产出"轮数（无产出性工具 ∧ 无 mailbox 新消息 ∧ 无终答），
	// 供循环尾部的停滞守卫（stagnationGuard）使用。
	unproductiveStreak := 0
	// visionDegraded 记录本 run 是否已因"模型不支持图片输入"降级过一次（见 generate 错误分支）：
	// 降级是单向的一次性动作，二次复现说明剥图路径有漏，按普通错误上报而非继续空转。
	visionDegraded := false
	for i := 0; a.maxIter <= 0 || i < a.maxIter; i++ {
		// 会话级挂起检查点（热驻模式）：挂起期间阻塞在此，恢复返回 nil 继续本轮。
		// 在飞 LLM/工具调用跑完（有界超时）才到达这里，非抢占式。
		if a.suspendGate != nil {
			if err := a.suspendGate.Park(ctx); err != nil {
				return ReactResult{History: history}, err
			}
		}
		// P0-2 steering（TODO #14 T8）：generate 前补一次 drainMailbox，压缩用户注入
		// 消息的插入延迟上界——上一轮工具执行期间经邮箱注入的用户消息，不必等到本轮
		// 无 tool_calls 分支才可见，本轮请求即可带上。此处 history 以 tool 结果
		// （或首轮 user 消息）收尾，追加 user 角色消息不会破坏 tool_calls 配对。
		var preGen int
		history, preGen = a.drainMailbox(history)
		if preGen > 0 {
			unproductiveStreak = 0 // 新信息注入，重置停滞计数（同循环尾 drain 语义）
		}

		// 在每次调用 LLM 之前，通过记忆流水线组装上下文消息。
		// 这可能会压缩历史、注入相关记忆或做其他上下文管理。
		assembled := a.memory.Assemble(a.role, a.name, history)
		// 陈旧工具结果驱逐（TODO 第9项③）：请求构建期视图变换，把 N 轮前的只读探查
		// 结果（ReadFile/SearchInFiles）替换为"已驱逐需重读"占位符。canonical history
		// 与持久化不受影响；驱逐保留 tool 消息本体，sanitizeToolPairing 配对不受影响。
		assembled = evictStaleToolResults(assembled, a.staleToolEvictRounds)

		// 当前时间尾部注入（TODO #40 块 1）：时间每轮变化，放尾部不破坏前缀缓存。
		// 所有 Agent（meta/domain/叶子）统一注入，与看板段同位（不可缓存尾部）。
		assembled = append(assembled, buildTimeMessage())

		// 上下文裁剪策略：仅 windowMessages 滑动窗口（硬上限，兜底防 API 上下文溢出）。
		// 历史压缩（hot/cold 分层）已迁入 memory.Pipeline.Assemble，仅按 token 阈值触发
		// （步频扳机已退役）；压缩先于窗口动手，窗口默认值已放大到 400 避免抢先裁剪。
		// windowMessages 只影响本次请求，不修改 history（完整历史仍用于持久化与续跑）。
		messages := windowMessages(assembled, a.historyMaxMessages)
		// 兜底防线：任何裁剪/注入路径若留下不配对的 tool 调用（assistant tool_calls
		// 未紧随 tool 结果，或孤立 tool 结果），Anthropic/OpenAI 会以 400 拒绝整轮请求，
		// 此前所有 LLM 耗时全部作废（实证 domain-2 白跑 31m43s）。发送前强制配对。
		messages = sanitizeToolPairing(messages)

		// 视觉能力缺失降级（2026-09-16）：当前模型已知不支持图片输入时，请求视图剥掉
		// 全部图像（含用户上传与 ReadMedia/插件截图透传），避免每次调用都撞 provider 400；
		// canonical history 与持久化不受影响，换模型后图片自动恢复。
		if a.noImageInput() {
			messages = stripImagesForRequest(messages)
		}

		// 上下文 token 预算（替换原累计 token 预算，150K 唯一上限）：Assemble 已按阈值
		// 压缩（保留近 N，旧压成上下文内摘要块）；压缩后仍超阈值 = 近 N 单独就超、压不下去，
		// 触达上限返回部分完成，由上层暂停会话等续跑。与 windowMessages（消息数硬上限）正交。
		if a.tokenBudget > 0 {
			if est := EstimateMessagesTokens(messages); est >= a.tokenBudget {
				log.Printf("[react] context budget exceeded: role=%s est_tokens=%d budget=%d", a.role.Name, est, a.tokenBudget)
				return ReactResult{History: history, LimitReached: true}, nil
			}
		}

		// 将内部消息格式转换为 blades 库所需的模型消息格式。
		bladesMsgs := ToBladesMessages(messages)

		// 构造模型请求：包含系统提示、历史消息和可用工具 schema。
		req := &blades.ModelRequest{
			Instruction: blades.SystemMessage(system),
			Messages:    bladesMsgs,
			Tools:       a.tools.Schema(),
		}

		// 提示词构成计量（TODO #15①）：每轮一行紧凑统计，首轮附 sys 分段明细，
		// 供提示词瘦身边际对照（#15② meta 收窄前后降幅即以此为准）。
		a.logPromptStats(i, system, messages, history, req.Tools)

		// 调用 LLM 生成回复（带重试与单次超时）；重试耗尽后返回错误。
		a.reportStableHash(system, i)
		resp, err := a.generate(ctx, req)
		if err != nil {
			// 视觉能力缺失（模型不支持图片输入）不判死（2026-09-16）：这是环境性错误，
			// 首次命中时记住该模型、注入说明并剥图重试（下轮请求已不含图像，工具侧也已门控）；
			// 同一 run 已因该原因降级过仍复现（剥图路径漏了）则照常报错，防 400 空转。
			if model.IsImageInputUnsupported(err) && !visionDegraded {
				visionDegraded = true
				markModelNoImage(a.llmModelName())
				notice := imageUnsupportedNotice(a.llmModelName())
				log.Printf("[react] image input unsupported: role=%s model=%s，剥图降级继续", a.role.Name, a.llmModelName())
				a.emitLive(LiveEvent{Kind: LiveEventNotify, Text: notice})
				history = a.appendLogged(history, ReactMessage{Role: "user", Content: notice})
				continue
			}
			// 出错时返回已累计的历史，便于上层回传部分进度或排查。
			return ReactResult{History: history}, fmt.Errorf("llm generate: %w", err)
		}

		// 推送本次 LLM 调用的 token 用量；provider 未填充用量时跳过（mock/测试）。
		// 缓存命中/未命中（TODO #40 可观测）经 Metadata 透传，会话层聚合展示命中率。
		if usage := resp.Message.TokenUsage; usage.InputTokens > 0 || usage.OutputTokens > 0 {
			ev := LiveEvent{
				Kind:         LiveEventTokenUsage,
				InputTokens:  usage.InputTokens,
				OutputTokens: usage.OutputTokens,
			}
			if md := resp.Message.Metadata; md != nil {
				if v, ok := md["cache_hit_tokens"].(int64); ok {
					ev.CacheHitTokens = v
				}
				if v, ok := md["cache_miss_tokens"].(int64); ok {
					ev.CacheMissTokens = v
				}
			}
			a.emitLive(ev)
		}

		// 将 blades 返回的消息转换为内部 Assistant 消息。
		assistant, droppedToolCalls := AssistantMessageFromBlades(resp.Message)

		// 坏工具调用保护：模型发起了工具调用但参数不是合法 JSON（如嵌套 JSON
		// 未转义、max_tokens 截断），AssistantMessageFromBlades 会丢弃这些调用。
		// 若照常落入"无工具调用=终答"分支，任务会带着一句中间陈述无声收官且被
		// 标记为 success（实证：SWE 修复会话中 WriteSpec 参数双重编码被丢弃，
		// 派发从未发生，会话却正常完成）。改为注入提示让模型修正后重发该调用；
		// 连续超限则显式报错，把失败暴露出来。
		if len(droppedToolCalls) > 0 {
			badToolCallStreak++
			log.Printf("[react] role=%s dropped %d tool call(s) with invalid JSON args: %s", a.name, len(droppedToolCalls), strings.Join(droppedToolCalls, "; "))
			if badToolCallStreak >= maxBadToolCallResponses {
				return ReactResult{History: history}, fmt.Errorf("model returned %d consecutive tool calls with invalid JSON args (%s)", badToolCallStreak, strings.Join(droppedToolCalls, "; "))
			}
			// 附带文本时保留 assistant 消息（模型据此知道自己尝试过什么），
			// 无文本则与空响应保护同理不写入历史（空 content 块可能被 API 拒绝）。
			if strings.TrimSpace(assistant.Content) != "" {
				history = a.appendLogged(history, truncateToolCallInputsForHistory(assistant, historyToolCallInputMaxRunes))
			}
			history = a.appendLogged(history, ReactMessage{Role: "user", Content: fmt.Sprintf(badToolCallNudge, strings.Join(droppedToolCalls, "；"))})
			continue
		}
		badToolCallStreak = 0

		// 空响应保护：模型既未输出文本也未调用工具（常见于思考阶段耗尽
		// max_tokens、端点异常或流被中途截断）。若当作最终答复返回，任务会
		// "无声完成"——用户看到的就是任务莫名其妙中断。改为注入提示消息让
		// 模型继续；连续空响应达到上限则显式报错，让上层以可见错误结束。
		if len(assistant.ToolCalls) == 0 && strings.TrimSpace(assistant.Content) == "" {
			emptyStreak++
			if emptyStreak >= maxEmptyResponses {
				return ReactResult{History: history}, fmt.Errorf("model returned %d consecutive empty responses (finish_reason=%s)", emptyStreak, resp.Message.FinishReason)
			}
			// 空 assistant 消息不写入历史（空 content 块可能被 API 拒绝），
			// 仅以一条提示消息要求模型继续。
			history = a.appendLogged(history, ReactMessage{Role: "user", Content: emptyResponseNudge})
			continue
		}
		emptyStreak = 0

		// 非空响应才追加到历史中。入史副本截断超长工具入参（如 WriteFile 全文件内容）：
		// 完整入参仅用于本次派发执行；历史/持久化/续跑只保留截断副本。
		// 否则大文件内容在滑动窗口内逐轮重发，单轮 input 即可耗尽整份 token 预算
		// （实证：塔防 domain 续跑首轮即再触限，pause/resume 零进展死锁）。
		history = a.appendLogged(history, truncateToolCallInputsForHistory(assistant, historyToolCallInputMaxRunes))

		// 如果助手消息中没有任何工具调用，说明本轮已产生最终答案。
		if len(assistant.ToolCalls) == 0 {
			// 在判断本轮是否结束之前，先轮询邮箱并注入任何新的异步消息。
			// mailbox 由服务装配层注入共享邮箱（用于接收异步子代理摘要）；未注入时跳过。
			// 仅在无 tool_calls 分支注入：mailbox 消息是 user 角色，若插在 assistant
			// tool_calls 与其 tool 结果之间，Anthropic 配对校验会以 400 拒绝整轮请求。
			var drained int
			history, drained = a.drainMailbox(history)
			// 竞态修复：子 Agent 完成摘要在本轮 LLM 生成期间才抵达 mailbox 时，刚生成的文本
			// 并未整合该摘要。典型序列：模型生成"请稍候"等待文本期间子 Agent 恰好完成，
			// PendingChildren 已归 0，下方终结保护不再拦截，进度汇报被误当终答
			// （实证：塔防 run4 meta 以"请稍候。"提前终结会话，domain-2 摘要从未进入终答）。
			// 只要 drain 到新消息就 continue 回主循环，让模型基于完整摘要重新生成本轮答复。
			if drained > 0 {
				unproductiveStreak = 0 // mailbox 新消息 = 新信息注入，重置停滞计数
				continue
			}
			// 父会话终结保护：若仍有未决子 Agent（call_sub_agent 派发后尚未回传结果），
			// 阻塞等待其完成而非立即终结，防止迟到 mailbox 消息随会话销毁丢失。
			// 多 Agent 协作验证闭环（code<->test 互问互答）的关键正确性保障。
			if a.pendingChecker != nil && a.pendingChecker.PendingChildren(a.name) > 0 {
				// Paused 子 DomainAgent 检查（先于挂起）：MetaAgent 无限 budget 不会因自身
				// token 暂停，但子 domain 触达上限进入 Paused 后不会再有完成事件——
				// 若挂起等子将永无唤醒源。检测到 Paused 子节点走旧 PausedOnChild 路径，
				// 由上层 pauseSession 置会话暂停态，等用户"继续"恢复该 domain。
				if a.pausedChecker != nil && a.pausedChecker.HasPausedChild(a.name) {
					return ReactResult{History: history, LimitReached: true, PausedOnChild: true}, nil
				}
				// Meta 挂起等子（awaiting_child 会话态）：任务已全部派发、终答轮仍有
				// 未决子 Agent 时不再原地阻塞 waitForChildren，立即带本轮中继文本返回，
				// 由上层置 awaiting_child——子完成（dispatcher childDoneFn 回调）或用户
				// 新消息唤醒 resumeSession 续跑整合，token 开销与原阻塞等待平价。
				// 仅会话顶层 Agent 适用（WithSuspendOnChildWait，meta/domain 顶层）：
				// dispatcher 子 Agent 仍走 waitForChildren 阻塞——其运行由 runSubAgent
				// 同步承载，无会话态可落，提前返回会被当终答回传。
				if a.suspendOnChildWait {
					return ReactResult{Text: assistant.Content, History: history, SuspendOnChildWait: true}, nil
				}
				// 等待期间不烧 LLM 轮次：纯阻塞等子 Agent 完成信号，仅当 mailbox
				// 取到新摘要时才 break 回主循环调 LLM 整合；超时无新消息则继续等。
				// 旧实现每 30s 超时白跑一次 LLM，50 轮上限烧完后会话停摆等用户
				// 人工续跑（实证：塔防任务死等 46 分钟）。
				var paused bool
				history, paused = a.waitForChildren(ctx, history)
				if paused {
					return ReactResult{History: history, LimitReached: true, PausedOnChild: true}, nil
				}
				continue
			}

			// 终答前补刀 drain（P1-4）：上方 drain 与 PendingChildren 检查之间存在毫秒
			// 窗口——子 Agent 的 notify(Send) 与计数递减都落在窗口内时，drain 未取到消息、
			// 计数已归零，终答将不含该子结果（摘要滞留已完成会话的邮箱成死信；心跳杀路径
			// 递减先于 Send，把窗口从微秒级拉大到毫秒级）。pending==0 分支再补一次 drain，
			// 有消息则回主循环整合。
			if a.mailbox != nil {
				var late int
				history, late = a.drainMailbox(history)
				if late > 0 {
					unproductiveStreak = 0
					continue
				}
			}

			// 将最终答案作为记忆事件写入。
			a.memory.Write(a.name, MemoryEvent{
				Type:     "answer",
				AgentID:  a.name,
				Content:  assistant.Content,
				Occurred: time.Now(),
			})

			// 返回最终结果与完整历史。
			return ReactResult{Text: assistant.Content, History: history}, nil
		}

		// 否则执行助手请求的全部工具调用（TODO 第9项①：轮内并行 + 串行回填两段）。
		// 并行段只做"派发+收集"，全部既有不变量（ToolCallID 配对、收口/截断、记忆写入、
		// mailbox 注入时机、停滞守卫）留在下方串行回填段单处执行，行为与旧串行版一致。
		calls := assistant.ToolCalls
		results := make([]ToolResult, len(calls))
		execErrs := make([]error, len(calls))
		for _, tc := range calls {
			// 实时推送工具调用开始事件，UI 可据此展示"执行中"状态；
			// 并行时先批量发出，让全部工具同时显示执行中。
			a.emitLive(LiveEvent{Kind: LiveEventToolCall, Tool: tc.Name, Input: mustMarshal(tc.Input)})
		}
		if a.parallelTools && len(calls) > 1 {
			// 并行执行段：每 call 一个 goroutine（信号量限并发），各自 keepalive/活动上报；
			// 写类工具经同路径互斥（pathLockFor）防并发竞写。
			sem := make(chan struct{}, a.toolParallelMax)
			var wg sync.WaitGroup
			for i := range calls {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					a.touchActivity("tool:" + calls[i].Name)
					results[i], execErrs[i] = a.dispatchToolSerialized(ctx, calls[i])
					a.touchActivity("tool_end")
				}(i)
			}
			wg.Wait()
		} else {
			for i := range calls {
				// 工具派发前上报活动（tool:<名> 证据）：挂死工具可被巡检按阈值识别。
				a.touchActivity("tool:" + calls[i].Name)
				results[i], execErrs[i] = a.dispatchToolWithKeepalive(ctx, calls[i])
				// 长工具（大文件写/长命令）执行完成后同样刷新活动，防巡检在工具执行期间误判。
				a.touchActivity("tool_end")
			}
		}
		// 串行回填段：按模型给定顺序逐个处理结果；首个 ErrLoopExit 命中即终止并带原因
		// 返回（并行段已发生的副作用属已知微小语义偏移：模型本只对无依赖调用并行发）。
		for i := range calls {
			tc := calls[i]
			result, err := results[i], execErrs[i]
			if err != nil {
				// 循环守卫命中（连读死循环/探索预算耗尽/连续失败）：终止循环并带原因返回，
				// 不吞成普通工具结果继续烧轮次。子 Agent 经 runSubAgent 走 Failed 语义
				//（失败打捞 + 父 mailbox 通知）；MetaAgent 直达时上层以错误结束会话。
				if errors.Is(err, tool.ErrLoopExit) {
					return ReactResult{History: history}, err
				}
				result = ToolResult{Tool: tc.Name, Error: err.Error()}
			}

			// 实时推送工具执行完成事件（成功/失败与输出）。
			a.emitLive(LiveEvent{
				Kind:    LiveEventToolExec,
				Tool:    tc.Name,
				Output:  result.Output,
				Error:   result.Error,
				Success: err == nil && result.Error == "",
			})

			// 工具结果统一收口（TODO 第9项②）：超阈值全文落盘 <workDir>/.bma/tool_outputs/，
			// 历史只留头部摘录 + 【全文已落盘】路径；落盘失败降级原样（走下方截断路径）。
			a.condenseToolResult(ctx, tc.Name, &result)

			// 截断工具输出后再写入历史，避免单次大输出（如整文件/长日志）
			// 在历史中无限累积，导致后续每轮请求 token 爆炸。
			if a.toolOutputMaxRunes > 0 {
				result.Output = truncateRunes(result.Output, a.toolOutputMaxRunes)
			}

			// 将工具执行结果以 tool 角色消息追加到历史中，供下一次 LLM 调用使用。
			// ToolCallID 携带本次调用的 ID：Anthropic/OpenAI 原生工具协议要求
			// tool_result 必须引用对应的 tool_use id，否则下一轮请求会被 API 拒绝。
			// Images 仅内存透传（ui_preview 截图等 image_passthrough 插件），
			// 挂载边界由 ToBladesMessages 控制（仅最新一批）。
			history = a.appendLogged(history, ReactMessage{
				Role:       "tool",
				Content:    ToolResultJSON(result),
				ToolCallID: tc.ID,
				Images:     result.Images,
			})

			// 把工具调用细节与结果写入记忆流水线（保留完整输出，展示层自行截断）。
			a.memory.Write(a.name, MemoryEvent{
				Type:     "tool_call",
				AgentID:  a.name,
				ToolName: tc.Name,
				Input:    mustMarshal(tc.Input),
				Output:   result.Output,
				Occurred: time.Now(),
			})
		}

		// 工具结果全部入史后再注入 mailbox：user 角色的 mailbox 消息若插在 assistant
		// tool_calls 与其 tool 结果之间，会触发 Anthropic 配对校验 400（整轮请求作废，
		// 实证：domain 派发子 Agent 后收到子 Agent 完成通知，白跑 31m43s 后失败）。
		// 注意不可用 := 短声明：循环体内会遮蔽外层 history，mailbox 消息随循环体结束丢失。
		var drainedMbx int
		history, drainedMbx = a.drainMailbox(history)

		// 停滞守卫：本轮无产出性工具且无 mailbox 新消息则累加计数（有则清零），
		// 连续无产出 10 轮注入预警、20 轮最终通牒、30 轮 ErrLoopExit 硬杀（meta 豁免硬杀）。
		var stagnationErr error
		history, unproductiveStreak, stagnationErr = a.stagnationGuard(unproductiveStreak, assistant.ToolCalls, drainedMbx, history)
		if stagnationErr != nil {
			return ReactResult{History: history}, stagnationErr
		}
	}

	// 达到最大迭代次数上限（仅 maxIter>0 时可能触发）：
	// 不视为错误——返回 LimitReached 标记与完整历史，
	// 由上层将会话置为暂停并提示用户发送消息续跑，而不是判定任务失败。
	return ReactResult{History: history, LimitReached: true}, nil
}

// 停滞守卫阈值：连续 N 轮"无产出"（无产出性工具调用 ∧ 无 mailbox 新消息 ∧ 无终答）逐级响应。
// 10 轮注入停滞预警、20 轮注入最终通牒、30 轮返回 ErrLoopExit 硬杀（meta 豁免，见 stagnationGuard）。
// 阈值 2026-09-15 由 6/12/24 放宽为 10/20/30：6 轮对合法长验证期（跑测试/逐条核对）催得过紧，
// 模型被推着草率收口（补丁式绕过根因，文件反复报错反复改）。硬杀仍须先于 2h 子 Agent 墙钟
// （sub_agent_timeout_min=120）：30 轮 × 单轮 2-5 分钟 ≈ 60-150 分钟，快模型远早于墙钟，慢模型
// 最坏端贴墙钟——打捞回灌父 Agent 可立即重派的价值保留在多数场景（2026-08-28 渲染领域 Agent
// 两小时纯读零产出撞墙事故即此守卫的由来）。
const (
	stagnationWarnRounds      = 10
	stagnationFinalWarnRounds = 20
	stagnationExitRounds      = 30
)

// productiveToolNames 产出性工具集合（裸名）：调用即视为"有产出"，重置停滞计数。
// 取"改外部状态类"（写文件/派发/消息/浏览器驱动）；只读探查（ReadFile/搜索/RunCommand
// 验证类）不算产出——连续 30 轮纯探查零写入本身就是异常信号，且 10/20 轮两次预警给了
// 正常长验证充足的自我收口窗口（预警只提示不杀）。
// 匹配前经 tool.BareToolName 剥离 MCP 插件前缀（browser_* 注册名为 ui_preview__browser_* 等）。
var productiveToolNames = map[string]bool{
	"WriteFile": true, "EditFile": true, "RestoreFile": true,
	"call_sub_agent": true, "call_sub_agents": true,
	"send_message": true, "ask_user": true,
	"WriteSharedMemory": true, "WriteSpec": true,
	"browser_click": true, "browser_run_code_unsafe": true,
}

// stagnationGuard 无产出停滞守卫。覆盖既有守卫够不着的两类语义死循环：
// 重复探针死循环（点击开始按钮 state 仍 idle 反复重试 19 分钟——连读同参×3 要求参数
// 相同拦不住）与完美主义不收口（"还剩最后一处缺口"无限取证——探索预算只拦读类拦不住）。
// 返回更新后的 history 与计数；达硬杀阈值返回 ErrLoopExit（dispatcher 侧 kind=loop_guard、
// 失败打捞回灌父 Agent 语义现成）。meta 豁免硬杀（ErrLoopExit 会终止整个会话），仅收预警。
func (a *ReActAgent) stagnationGuard(streak int, calls []ToolCall, mailboxDrained int, history []ReactMessage) ([]ReactMessage, int, error) {
	productive := mailboxDrained > 0
	if !productive {
		for _, tc := range calls {
			if productiveToolNames[tool.BareToolName(tc.Name)] {
				productive = true
				break
			}
		}
	}
	if productive {
		return history, 0, nil
	}
	streak++
	switch {
	case streak >= stagnationExitRounds && a.role.ID != "meta":
		return history, streak, fmt.Errorf("%w: 连续 %d 轮无任何产出性动作（未写文件/未派发/未收发消息/未终答），判定停滞强制终止。已有部分产出已保留，可缩小任务范围后重派", tool.ErrLoopExit, streak)
	case streak == stagnationFinalWarnRounds:
		history = a.appendLogged(history, ReactMessage{Role: "user", Content: fmt.Sprintf(
			"【停滞最终警告】已连续 %d 轮没有任何产出（未写文件/未派发/未收发消息/未终答）。换个策略推进：若当前路径走不通，改变方法或上报阻塞（ask_user/send_message 说明卡点）；确认确实无进展再收敛输出终答。", streak)})
	case streak == stagnationWarnRounds:
		history = a.appendLogged(history, ReactMessage{Role: "user", Content: fmt.Sprintf(
			"【停滞预警】已连续 %d 轮没有任何产出性动作（仅只读探查/验证）。若证据已足够，直接基于已有信息推进下一步（写文件/派发/终答）；若仍缺信息，换一个检索角度或换策略，不要重复同一探查。", streak)})
	}
	return history, streak, nil
}

// condenseToolResult 工具结果统一收口（TODO 第9项②）：单条工具输出超过 toolResultDumpRunes
// 时全文落盘 <workDir>/.bma/tool_outputs/<agent>-<unix>-<tool>.md，Output 替换为头部摘录 +
// 【全文已落盘】路径，模型按需 ReadFile 取全文。落盘失败降级原样（不丢内容，回落截断路径）。
// 收口后输出远小于 toolOutputMaxRunes，既有截断自然不再触发。
func (a *ReActAgent) condenseToolResult(ctx context.Context, toolName string, result *ToolResult) {
	if a.toolResultDumpRunes <= 0 {
		return
	}
	total := len([]rune(result.Output))
	if total <= a.toolResultDumpRunes {
		return
	}
	path, err := dumpToolOutputToDisk(ctx, a.workDir, a.name, toolName, result.Output)
	if err != nil {
		log.Printf("[react] tool output dump failed (degrade to truncate): agent=%s tool=%s err=%v", a.name, toolName, err)
		return
	}
	digest := a.toolResultDigestRunes
	if digest <= 0 {
		digest = 2000
	}
	result.Output = truncateRunes(result.Output, digest) + "\n\n【全文已落盘】" + path + "（如需全文请 ReadFile 该路径）"
	log.Printf("[react] tool output dumped: agent=%s tool=%s path=%s total=%d runes", a.name, toolName, path, total)
}

// dumpToolOutputToDisk 把超限工具输出全文写入 <workDir>/.bma/tool_outputs/，返回绝对路径。
// workDir 优先取 ctx 注入的会话目录，回退 agent 自身 workDir，再回退系统临时目录
//（与 mailbox 收口 dumpReturnToDisk 同容错口径：.bma 是项目级目录，各回退目录下语义一致）。
func dumpToolOutputToDisk(ctx context.Context, agentWorkDir, agentID, toolName, output string) (string, error) {
	wd := tool.WorkDirFromContext(ctx)
	if wd == "" {
		wd = agentWorkDir
	}
	base := wd
	if base == "" {
		base = filepath.Join(os.TempDir(), "bma-tool-outputs")
	}
	dir := filepath.Join(base, ".bma", "tool_outputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%d-%s.md", filepath.Base(agentID), time.Now().Unix(), sanitizeToolFilePart(toolName)))
	if err := os.WriteFile(path, []byte(output), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizeToolFilePart 把工具名收敛为安全文件名片段（MCP 工具名形如 mcp__server__tool，
// 子代理派发名形如 call_sub_agent；异常字符统一替换为下划线）。
func sanitizeToolFilePart(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}

// evictableToolNames 可驱逐的工具集合（TODO 第9项③）：只读探查类，结果内容仍在文件系统/
// 可重取，驱逐后按原参数重跑即可恢复。验证证据类（RunCommand/http/git/WriteSpec 等）
// 一律不驱逐——它们是"已完成验证"的凭证，驱逐会诱发无意义的重复验证。
var evictableToolNames = map[string]bool{
	"ReadFile":      true,
	"SearchInFiles": true,
}

// evictPlaceholderMark 占位符指纹：已驱逐的消息本轮不再重写，保持占位文本跨轮稳定
//（前缀缓存友好；重写只会把"原文 N runes"换成占位符自身长度，毫无收益）。
const evictPlaceholderMark = "[内容已驱逐]"

// evictStaleToolResults 陈旧工具结果驱逐（TODO 第9项③）：对请求视图做变换，把超过
// keepRounds 轮前的 ReadFile/SearchInFiles 结果替换为合法 ToolResultJSON 占位符
//（"已驱逐需重读"，防止模型幻觉记得旧内容）。只影响本次请求：
// canonical history / 持久化 / 续跑数据无损；驱逐保留 tool 消息本体与 ToolCallID，
// sanitizeToolPairing 配对校验不受影响；token 估算天然看到驱逐后的大小。
//
// 轮次界定 = assistant 消息序数（0 起）：tool 结果归属其前方最近的 assistant，
// 与最新 assistant 序数之差 > keepRounds 即候选。
func evictStaleToolResults(messages []ReactMessage, keepRounds int) []ReactMessage {
	if keepRounds <= 0 || len(messages) == 0 {
		return messages
	}
	// tool_call id -> 工具名（同 ToBladesMessages 的索引手法），供判定结果可否驱逐。
	names := make(map[string]string)
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				names[tc.ID] = tc.Name
			}
		}
	}
	// 每条消息归属的 assistant 轮序；非 assistant 之前的（如首条 user）记 -1。
	ords := make([]int, len(messages))
	last := -1
	for i, m := range messages {
		if m.Role == "assistant" {
			last++
		}
		ords[i] = last
	}
	cur := ords[len(messages)-1]
	if cur < 0 {
		return messages
	}
	out := make([]ReactMessage, len(messages))
	copy(out, messages)
	for i := range out {
		m := &out[i]
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		if cur-ords[i] <= keepRounds {
			continue
		}
		if strings.Contains(m.Content, evictPlaceholderMark) {
			continue // 已是占位符：保持文本稳定，不重写
		}
		name := names[m.ToolCallID]
		if !evictableToolNames[name] {
			continue
		}
		origRunes := len([]rune(m.Content))
		m.Content = ToolResultJSON(ToolResult{
			Tool:    name,
			Success: true,
			Output: fmt.Sprintf("%s 该结果是 %d 轮前的 %s 输出（原文 %d runes）。文件内容仍在磁盘上未被删除；"+
				"如需再次查看，请按上方对应调用的参数重新执行 %s。", evictPlaceholderMark, cur-ords[i], name, origRunes, name),
		})
		m.Images = nil
	}
	return out
}

// 盲区修复（2026-08-19）：旧实现只在工具派发前/后 touch，工具执行期间零上报--
// 长命令/大文件操作（构建、依赖安装）超过心跳阈值即被巡检误判假死杀掉，
// 全部工作从零重派（实证：战斗实体首任 10 分钟被杀，损失约 20 分钟）。
// 定时器与流式保活同口径：真实挂死由 sub_agent_timeout 墙钟兜底，本保活不无限续命。
func (a *ReActAgent) dispatchToolWithKeepalive(ctx context.Context, tc ToolCall) (ToolResult, error) {
	interval := a.streamKeepalive
	if interval <= 0 {
		interval = 30 * time.Second
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 保活 tick 只证明进程活着（kind=keepalive 不刷新 lastTS——盲报根除，
				// TODO 第10项②）：挂死工具由巡检按 toolStartTS 阈值识别并击杀。
				a.touchActivity("keepalive")
			}
		}
	}()
	return a.tools.Dispatch(ctx, tc)
}

// writeLockedTools 轮内并行时需要按目标路径互斥的写类工具（TODO 第9项①）：
// 同路径并发写会产生交错损坏（半截内容/双写冲突），同路径串行、异路径并行。
var writeLockedTools = map[string]bool{"WriteFile": true, "EditFile": true, "RestoreFile": true}

// dispatchSerializedTools 轮内并行时需要全互斥的编排类工具（P0-1）：它们修改共享
// 编排状态（权威树注册/同领域查重/派发配额/git worktree index），并发执行会击穿
// 串行时代成立的"同父同 domain 唯一活跃"不变量；与子 Agent 派发额度无关的两个
// 不同 domain 单派也经 call_sub_agents 批量入口，串行化无损并行度。
var dispatchSerializedTools = map[string]bool{
	"call_sub_agent": true, "call_sub_agents": true, "map_sub_agents": true,
	"merge_worktree": true,
	"cancel_agent": true, "pause_agent": true, "resume_agent": true,
}

// dispatchToolSerialized 带互斥的工具派发：写类工具按目标路径加锁、编排类工具
// 全互斥（相对路径按 agent workDir 解析，worktree 副本天然不同键），其余零开销直通。
func (a *ReActAgent) dispatchToolSerialized(ctx context.Context, tc ToolCall) (ToolResult, error) {
	if dispatchSerializedTools[tc.Name] {
		a.dispatchMu.Lock()
		defer a.dispatchMu.Unlock()
	}
	if mu := a.pathLockFor(tc); mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	return a.dispatchToolWithKeepalive(ctx, tc)
}

// pathLockFor 返回该调用目标路径的互斥锁（按 agent 实例懒建）；非写类工具/无路径参数
// 返回 nil。绝对路径规范化后作键，避免同文件不同写法绕过互斥。
func (a *ReActAgent) pathLockFor(tc ToolCall) *sync.Mutex {
	if !writeLockedTools[tc.Name] {
		return nil
	}
	p, _ := tc.Input["path"].(string)
	if strings.TrimSpace(p) == "" {
		return nil
	}
	if !filepath.IsAbs(p) {
		base := a.workDir
		if base == "" {
			base = "."
		}
		p = filepath.Join(base, p)
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	a.pathMu.Lock()
	defer a.pathMu.Unlock()
	if a.pathMutexes == nil {
		a.pathMutexes = map[string]*sync.Mutex{}
	}
	mu, ok := a.pathMutexes[p]
	if !ok {
		mu = &sync.Mutex{}
		a.pathMutexes[p] = mu
	}
	return mu
}

// 提示词三层分级（TODO 第10项①缓存纪律，对标 Hermes）：前缀缓存命中依赖"稳定段在前、
// 易变段置尾"，三层易变度递增、位置与冻结纪律如下——
//   - stable（system instruction）：systemPrompt() 按实例 sync.Once 冻结（人格/技能块/
//     envBlock 均在冻结前拼入），跨轮字节级不变；reportStableHash 逐呼打哈希，变化即 WARN。
//   - context（压缩视图）：memory.Pipeline 冻结视图，仅压缩触发轮变化（该轮前缀缓存失效，
//     变更点在 pipeline.go compressedView 内记录）。
//   - volatile（时间戳/近期事件/文件地图/mailbox/sibling 摘要）：每轮可变，全部置尾注入，
//     不破坏前缀。
//
// reportStableHash 每呼记录 stable 层（发往模型的 Instruction=system 消息）内容哈希：
// 正常恒定（冻结保证），变化即 WARN——冻结被破坏（回归）时第一现场暴露。
func (a *ReActAgent) reportStableHash(instruction string, round int) {
	sum := sha256.Sum256([]byte(instruction))
	hash := hex.EncodeToString(sum[:])[:12]
	if a.stableHashLast == "" {
		a.stableHashLast = hash
		log.Printf("[cache] stable_hash=%s agent=%s round=%d (init)", hash, a.name, round)
		return
	}
	if hash != a.stableHashLast {
		log.Printf("[cache] WARN stable_hash changed: agent=%s round=%d %s -> %s (stable layer must be frozen per instance)", a.name, round, a.stableHashLast, hash)
		a.stableHashLast = hash
		return
	}
	log.Printf("[cache] stable_hash=%s agent=%s round=%d", hash, a.name, round)
}

// logPromptStats 输出提示词构成计量行（TODO #15①）：每轮 LLM 调用一行紧凑统计——
//   - sys：系统指令 rune 数（buildSystemPrompt 冻结值）
//   - tools：工具数/名称+描述 rune 数（角色工具白名单规模，#15② 收窄的直接观测项）
//   - hist：请求视图历史 rune 数（窗口裁剪后、发往模型的全部非 system 消息）
//   - dyn：请求视图相对 canonical history 的净增 rune（Assemble 注入的压缩摘要/事件/
//     看板段减去压缩缩减；负值=本轮压缩净缩），时间消息等尾部注入一并计入
//
// 首轮（round==0）追加一行 sys 分段明细（persona/env/role_base/discipline/skill），
// 供瘦身边际对照。纯 log 输出，不入史不落事件。
func (a *ReActAgent) logPromptStats(round int, system string, messages, history []ReactMessage, toolSchemas []tools.Tool) {
	sysRunes := utf8.RuneCountInString(system)
	toolDescRunes := 0
	for _, t := range toolSchemas {
		toolDescRunes += utf8.RuneCountInString(t.Name()) + utf8.RuneCountInString(t.Description())
	}
	histRunes := 0
	for _, m := range messages {
		histRunes += utf8.RuneCountInString(m.Content)
	}
	dynRunes := 0
	for _, m := range history {
		dynRunes -= utf8.RuneCountInString(m.Content)
	}
	dynRunes += histRunes
	log.Printf("[prompt-stats] agent=%s round=%d sys=%d tools=%d/%dr hist=%d dyn=%d",
		a.name, round, sysRunes, len(toolSchemas), toolDescRunes, histRunes, dynRunes)
	if round == 0 && len(a.promptStatsSegs) > 0 {
		log.Printf("[prompt-stats] sys segments agent=%s persona=%d env=%d role_base=%d discipline=%d skill=%d memidx=%d",
			a.name, a.promptStatsSegs["persona"], a.promptStatsSegs["env"],
			a.promptStatsSegs["role_base"], a.promptStatsSegs["discipline"], a.promptStatsSegs["skill"],
			a.promptStatsSegs["memory_index"])
	}
}

// generate 包装一次 LLM 调用：带单次超时与指数退避重试（LLM 链）。
// 重试/退避/超时/空响应判定迁移到 middleware 包（RetryLLM + CallLLM + TerminalCall），
// 行为不变：重试 retryCount+1 次尝试；会话取消（ctx.Err() 非空）与单次调用超时
// （DeadlineExceeded，慢推理模型重试只会重复超时——实证 180s×4=12min）不重试；
// 空响应不重试。链式形态即未来新增横切中间件的挂载点。
func (a *ReActAgent) generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	c := &middleware.LLMCtx{Request: req, Call: a.generateOnce}
	chain := middleware.New[middleware.LLMCtx]().
		Use(middleware.RetryLLM(a.retryCount, a.retryBackoff, shouldRetryLLMCall)).
		Use(middleware.CallLLM(a.llmTimeout)).
		Then(middleware.TerminalCall)
	if err := chain(ctx, c); err != nil {
		return nil, err
	}
	return c.Resp, nil
}

// streamingModelProvider 是 blades.ModelProvider 的可选流式接口子集。
// 具体 provider（openai-chat/openai-responses/anthropic 等）通常同时实现 Generate 与 NewStreaming；
// 仅实现 Generate 的 provider（含测试 mock）自动回退到一次性调用。
type streamingModelProvider interface {
	// NewStreaming 执行请求并返回一个逐块产出响应的生成器；
	// 最后一个产出值是完整累积响应（各 provider 由内部累积器保证）。
	NewStreaming(context.Context, *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error]
}

// generateOnce 执行单次 LLM 调用：provider 支持流式时走流式并推送 llm_delta 实时事件，
// 否则回退到一次性 Generate。
func (a *ReActAgent) generateOnce(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	// 模型绑定可能在 Agent 存活期内被切换（set_role_model / TUI / Web 选择器）：
	// 每次调用重解析 provider，失败保留旧 provider（不打断当前任务）。
	if a.providerFn != nil {
		if p, err := a.providerFn(ctx); err == nil && p != nil {
			a.llm = p
		} else if err != nil {
			log.Printf("[react] provider refresh failed, keep current: role=%s err=%v", a.role.Name, err)
		}
	}
	a.touchActivity("llm_start")
	start := time.Now()
	role := a.role.Name
	log.Printf("[react] llm start: role=%s model=%s msgs=%d", role, a.llmModelName(), len(req.Messages))
	var resp *blades.ModelResponse
	var err error
	if sp, ok := a.llm.(streamingModelProvider); ok {
		resp, err = a.generateStreaming(ctx, req, sp)
	} else {
		resp, err = a.llm.Generate(ctx, req)
	}
	dur := time.Since(start)
	if err != nil {
		log.Printf("[react] llm FAIL: role=%s dur=%s err=%v", role, dur, err)
	} else {
		log.Printf("[react] llm done: role=%s dur=%s", role, dur)
	}
	// 无论成功失败都记录完整 LLM I/O 到 session_logs，便于排查 token 暴涨/失忆问题。
	a.logLLMCall(ctx, req, resp, err, dur)
	// LLM 调用收尾证据：解除在飞 LLM 豁免，巡检恢复按 lastTS 判步间静默（TODO 第10项②）。
	a.touchActivity("llm_end")
	return resp, err
}

// logLLMCall 把单次 LLM 调用的完整输入输出写入 session_logs。
// prompt 序列化为含 system/messages/tools 的 JSON；response 取消息文本+工具调用摘要。
// 未注入 logger 时跳过；resp 为 nil（调用失败）时只记录 prompt 与错误。
func (a *ReActAgent) logLLMCall(ctx context.Context, req *blades.ModelRequest, resp *blades.ModelResponse, callErr error, dur time.Duration) {
	if a.log == nil {
		return
	}
	prompt := serializePromptForLog(req)
	response := ""
	var inTok, outTok, cacheHit, cacheMiss int64
	if resp != nil && resp.Message != nil {
		response = serializeResponseForLog(resp.Message)
		inTok = resp.Message.TokenUsage.InputTokens
		outTok = resp.Message.TokenUsage.OutputTokens
		// TODO #40 缓存可观测：provider 侧经 Metadata 透传 cache_hit/miss tokens。
		if md := resp.Message.Metadata; md != nil {
			if v, ok := md["cache_hit_tokens"].(int64); ok {
				cacheHit = v
			}
			if v, ok := md["cache_miss_tokens"].(int64); ok {
				cacheMiss = v
			}
		}
	}
	errStr := ""
	if callErr != nil {
		errStr = callErr.Error()
		response = "[ERROR] " + errStr + "\n" + response
	}
	a.log.LLMCall(ctx, logger.LLMCallRecord{
		Agent:           a.role.Name,
		Model:           a.llmModelName(),
		Prompt:          prompt,
		Response:        response,
		InputTokens:     int(inTok),
		OutputTokens:    int(outTok),
		CacheHitTokens:  int(cacheHit),
		CacheMissTokens: int(cacheMiss),
		LatencyMs:       int(dur.Milliseconds()),
	})
}

// llmModelName 返回 llm provider 的模型名标识，用于日志记录。
// 通过类型断言读取可选的 ModelNamer 接口；未实现时返回空串。
func (a *ReActAgent) llmModelName() string {
	if mn, ok := a.llm.(interface{ ModelName() string }); ok {
		return mn.ModelName()
	}
	return ""
}

// serializePromptForLog 把 ModelRequest 序列化为可读 JSON 字符串，含 system/messages/tools。
func serializePromptForLog(req *blades.ModelRequest) string {
	if req == nil {
		return ""
	}
	type msgOut struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type toolOut struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := struct {
		System   string    `json:"system"`
		Messages []msgOut  `json:"messages"`
		Tools    []toolOut `json:"tools"`
	}{
		System:   systemText(req),
		Messages: make([]msgOut, 0, len(req.Messages)),
		Tools:    make([]toolOut, 0, len(req.Tools)),
	}
	for _, m := range req.Messages {
		if m == nil {
			continue
		}
		out.Messages = append(out.Messages, msgOut{Role: string(m.Role), Content: messageLogText(m)})
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, toolOut{Name: t.Name(), Description: t.Description()})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}

// messageLogText 提取消息的可读日志文本：文本部分 + 工具调用/结果部分。
// bladesText 只读 TextPart，纯 tool_call 的 assistant 消息与 tool 结果消息会被
// 序列化成空 content（实证：日志里大量 "role":"tool"/"assistant","content":""
// 被误以为上下文为空；实际请求中 tool 数据完整，只是日志没渲染）。
// 单个 part 截断 2000 runes：仅防本地日志爆炸，与入史截断（historyToolCallInputMaxRunes，
// 已提至 12000）相互独立，不影响上下文回发内容。
func messageLogText(m *blades.Message) string {
	var sb strings.Builder
	sb.WriteString(bladesText(m))
	for _, p := range m.Parts {
		tp, ok := p.(blades.ToolPart)
		if !ok {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		if tp.Response != "" {
			sb.WriteString("[tool_result] id=")
			sb.WriteString(tp.ID)
			sb.WriteString("\n")
			sb.WriteString(truncateRunes(tp.Response, 2000))
		} else {
			sb.WriteString("[tool_call] name=")
			sb.WriteString(tp.Name)
			sb.WriteString(" id=")
			sb.WriteString(tp.ID)
			sb.WriteString("\n")
			sb.WriteString(truncateRunes(tp.Request, 2000))
		}
	}
	return sb.String()
}

// serializeResponseForLog 把 blades.Message 序列化为含文本与工具调用的可读字符串。
func serializeResponseForLog(m *blades.Message) string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	if text := bladesText(m); text != "" {
		sb.WriteString(text)
	}
	for _, p := range m.Parts {
		if tp, ok := p.(blades.ToolPart); ok {
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString("[tool_call] name=")
			sb.WriteString(tp.Name)
			sb.WriteString(" id=")
			sb.WriteString(tp.ID)
			sb.WriteString("\n")
			sb.WriteString(tp.Request)
		}
	}
	if m.FinishReason != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[finish_reason=")
		sb.WriteString(m.FinishReason)
		sb.WriteString("]")
	}
	return sb.String()
}

// systemText 提取 ModelRequest.Instruction 的文本内容。
func systemText(req *blades.ModelRequest) string {
	if req == nil || req.Instruction == nil {
		return ""
	}
	return bladesText(req.Instruction)
}

// generateStreaming 消费流式响应：中间块只用于向 UI 推送累积文本，
// 最后一个产出值作为本次调用的完整响应返回（文本/工具调用均以它为准）。
func (a *ReActAgent) generateStreaming(ctx context.Context, req *blades.ModelRequest, sp streamingModelProvider) (*blades.ModelResponse, error) {
	var final *blades.ModelResponse
	// display 是截至当前累积的展示文本。
	// 不同 provider 的块语义不同：增量块追加、全量（累积）块替换——
	// 用"块文本是否以已有累积文本为前缀"区分两种形态，兼容两类 provider。
	display := ""
	// 长考保活：thinking 模型首 token 前可静默数分钟，期间零 chunk 触发不了 touchActivity，
	// 心跳巡检会误判假死杀掉活跃流；定时器补上报。真实挂死（流永久静默）由
	// sub_agent_timeout 墙钟兜底，本保活不无限续命。
	keepalive := a.streamKeepalive
	if keepalive <= 0 {
		keepalive = 30 * time.Second
	}
	// 流式块间空闲超时（2026-08-20）：ark glm-5.3 流式一天三次中途静默
	// （440s / 15min / 20min 零 chunk，均等满整次调用墙钟才报错）。chunk 间隔超过
	// 阈值即判流死、取消流让 generate 重试；2026-08-27 起首块前同样设卡
	// （endpoint 假死零字节连接原先只有 provider 600s 硬超时且不重试）。
	idleTimeout := a.streamIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	firstChunkTimeout := a.streamFirstChunkTimeout
	if firstChunkTimeout <= 0 {
		firstChunkTimeout = defaultStreamFirstChunkTimeout
	}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	started := time.Now()
	var idleMu sync.Mutex
	lastChunk := time.Now()
	firstChunk := false
	idleKilled := false
	killPhase := "" // "before first chunk" / "after first chunk"
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 流保活 tick 证明进程活着，但不刷新 lastTS（盲报根除，TODO 第10项②）；
				// 在飞 LLM 期间巡检本就豁免（llm_start 已置位），无需续命。
				a.touchActivity("keepalive")
				idleMu.Lock()
				// 首块前也设卡口：endpoint 假死（连接建立后零字节）只有 provider
				// http.Client 600s 整体兜底，且该错误属 DeadlineExceeded 不重试，
				// 整个 Agent 白死 10 分钟（实证 2026-08-27 domain-1/4 连续 10m0.0s
				// FAIL partial=""）。首块前与首块后同用 idleTimeout 判静默。
				stall := time.Since(lastChunk) > idleTimeout
				phase := "after first chunk"
				if !firstChunk {
					// 首块前用独立短卡口：健康端点首 chunk 秒级到达，等满 300s 才判死
					// 会在假连接上白烧墙钟（实证 domain-4/5/6 三连 5min 零首块）。
					stall = time.Since(started) > firstChunkTimeout
					phase = "before first chunk"
				}
				if stall {
					idleKilled = true
					killPhase = phase
				}
				idleMu.Unlock()
				if stall {
					cancelStream()
					return
				}
			}
		}
	}()
	for resp, err := range sp.NewStreaming(streamCtx, req) {
		if err != nil {
			idleMu.Lock()
			killed, phase := idleKilled, killPhase
			idleMu.Unlock()
			if killed && ctx.Err() == nil {
				// cancelStream 触发的底层错误是 context canceled（非 DeadlineExceeded），
				// RetryLLM 判为瞬时故障走重试——正是本卡口的目的。
				return nil, fmt.Errorf("stream stall timeout (no chunk for %s %s): %w", idleTimeout, phase, err)
			}
			return nil, err
		}
		idleMu.Lock()
		lastChunk = time.Now()
		firstChunk = true
		idleMu.Unlock()
		// 流式块即活动证据（kind=stream）：thinking 模型单次长生成可达 5-7 分钟
		//（大段代码+深度推理），chunk 持续刷新 lastTS；即使在飞 LLM 豁免兜不住的场景
		//（跨入下一轮工具前）也有真实步进证据（TODO 第10项②）。
		a.touchActivity("stream")
		if resp == nil || resp.Message == nil {
			continue
		}
		final = resp
		// 思考过程增量（provider 经 Metadata 传递）：瞬时推送，答复文本开始输出后由
		// 上层清除；思考内容不进入答复文本，避免与正式输出混淆。
		// Anthropic 用 "thinking" 键（独立思考块，本帧无正文，跳过安全）。
		if thinking, ok := resp.Message.Metadata["thinking"].(string); ok && thinking != "" {
			a.emitLive(LiveEvent{Kind: LiveEventThinkDelta, Text: thinking})
			continue
		}
		// OpenAI 系（deepseek/ark/glm 等 openai 兼容端点）用 "reasoning_content" 键
		//（provider_openai_chat/responses 累积写入）。此前只认 "thinking"，该键从未被
		// 消费 → OpenAI 系模型既无实时思考也无 think 事件（2026-09-26 实证）。
		// 不 continue：这类 provider 单帧可同时携带累积正文（下方按前缀去重吸收），
		// 跳过会丢本帧文本增量（末帧丢失无法由后续帧补回）。
		if reasoning, ok := resp.Message.Metadata["reasoning_content"].(string); ok && reasoning != "" {
			a.emitLive(LiveEvent{Kind: LiveEventThinkDelta, Text: reasoning})
		}
		text := bladesText(resp.Message)
		if text == "" {
			continue
		}
		if display == "" || strings.HasPrefix(text, display) {
			display = text
		} else {
			display += text
		}
		a.emitLive(LiveEvent{Kind: LiveEventLLMDelta, Text: display})
	}
	if final == nil {
		return nil, errors.New("empty model stream")
	}
	// provider 被取消后可能不 yield 错误而是直接 return（generator 契约允许）：
	// 此时流被截断但 final 已有首块内容，若当正常结束返回会把半截内容当终答。
	idleMu.Lock()
	killed := idleKilled
	idleMu.Unlock()
	if killed && ctx.Err() == nil {
		return nil, fmt.Errorf("stream idle timeout (no chunk for %s after first chunk): stream terminated", idleTimeout)
	}
	return final, nil
}

// bladesText 提取 blades 消息中的全部文本部分（忽略工具调用等非文本部分）。
func bladesText(m *blades.Message) string {
	if m == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		if tp, ok := p.(blades.TextPart); ok {
			sb.WriteString(tp.Text)
		}
	}
	return sb.String()
}

// waitForChildren 事件驱动阻塞等待任一子 Agent 完成：等待期间不烧 LLM 轮次。
// mailbox 取到新摘要、无 mailbox 或上下文取消时返回；检测到 Paused 子节点时
// 返回 paused=true，调用方应以 PausedOnChild 结束并让上层置会话暂停态。
func (a *ReActAgent) waitForChildren(ctx context.Context, history []ReactMessage) ([]ReactMessage, bool) {
	for a.pendingChecker.PendingChildren(a.name) > 0 {
		// 展示态上报（编排页"等待下级返回"标识）：dispatcher 侧特判只换 lastKind，
		// 不刷 lastTS、不冒泡，等子存活性仍由后代活动冒泡决定。
		a.touchActivity("child_wait")
		// 会话级挂起检查点（热驻模式）：domain 等叶子期间的挂起点，
		// 恢复返回 nil 继续等待子 Agent。
		if a.suspendGate != nil {
			if err := a.suspendGate.Park(ctx); err != nil {
				return history, false
			}
		}
		if a.pausedChecker != nil && a.pausedChecker.HasPausedChild(a.name) {
			return history, true
		}
		a.pendingChecker.WaitForAnyChild(a.name, 30*time.Second)
		var n int
		history, n = a.drainMailbox(history)
		if n > 0 || a.mailbox == nil || ctx.Err() != nil {
			return history, false
		}
	}
	return history, false
}

// summarizeWindow 已迁入 domain/memory/pipeline.go 的 compressHistory 函数。
// 历史压缩（hot/cold 分层）属记忆层职责，ReActAgent 不再直接做历史压缩。
// 触发仅由 token 阈值驱动（memory.Pipeline.WithContextBudget + WithTokenEstimator，
// bootstrap 注入；步频扳机已退役），保留段长度由 WithCompression(keepRecent) 配置。

// windowMessages 把发送给 LLM 的消息裁剪到最多 max 条（滑动窗口）：
// 保留开头的 system 消息（记忆注入）与最近的对话，下刀处避开孤立的 tool 结果
// （assistant 的 tool_calls 与后续 tool 结果须成对，部分 provider 校验不成对会报错；
// 从 assistant/user 起刀即天然成对，仅 tool 起刀会孤立）。被省略的条数以一条说明消息占位。
// max<=0 或未超限时原样返回。
func windowMessages(messages []ReactMessage, max int) []ReactMessage {
	if max <= 0 || len(messages) <= max {
		return messages
	}
	// 保留开头的 system 消息（防御性扫描；记忆流水线已将近期事件移至末尾，此处通常无 system 前缀）。
	keep := 0
	for keep < len(messages) && messages[keep].Role == "system" {
		keep++
	}
	// 首条 user（任务目标）必须保留：与 summarizeWindow 行为对齐。
	// 不保留会导致子 Agent 跑几轮后 task 被中间占位替换，报告"只看到前导语，看不到 task 正文"。
	firstUserIdx := -1
	for i := keep; i < len(messages); i++ {
		if messages[i].Role == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx < 0 {
		return messages
	}
	// 预算：system 前缀 + 首条 user + 省略说明各占 1 条，其余留给最近消息。
	budget := max - keep - 2
	if budget < 1 {
		budget = 1
	}
	start := len(messages) - budget
	if start < firstUserIdx+1 {
		start = firstUserIdx + 1
	}
	// 向前移动 start 避开孤立的 tool 结果消息：tool 结果必须跟随其 assistant tool_calls，
	// 窗口从 tool 结果开始会被 API 拒绝（orphaned tool_result）。
	// 不能锚定 user 边界：工作型历史是 [user, assistant, tool, assistant, tool…]，
	// 最近窗口内常无 user，锚 user 会走空整个窗口，只剩首条 user + 占位符两条消息
	// （实证：塔防配置 Agent 上下文塌缩成 msgs=2，每轮失忆重写 config.js 不收敛）。
	for start < len(messages) && messages[start].Role == "tool" {
		start++
	}
	omitted := start - firstUserIdx - 1
	if omitted <= 0 {
		return messages
	}
	out := make([]ReactMessage, 0, len(messages)-omitted+1)
	out = append(out, messages[:keep]...)
	out = append(out, messages[firstUserIdx]) // 首条 user 任务目标
	// 占位文本固定（不含动态计数）：避免每轮 omitted 变化导致前缀缓存失效。
	out = append(out, ReactMessage{
		Role:    "user",
		Content: "（上下文已省略早期对话，关键结论见下方近期事件）",
	})
	out = append(out, messages[start:]...)
	return out
}

// sanitizeToolPairing 发送给 LLM 前强制修正 tool 调用配对（Anthropic/OpenAI 协议要求）：
//  1. assistant 消息的每个 tool_calls 必须紧随对应的 tool 结果消息；
//     缺失的（如被窗口裁剪/注入打断）就地补一条合成错误结果，保证整轮请求不被 400 拒绝；
//  2. 孤立的 tool 结果消息（前面没有匹配的 tool_calls）同样会被 API 拒绝，直接丢弃。
//
// 正常路径（mailbox 已在 tool 结果入史后注入、windowMessages/compressHistory 避开 tool 起刀）
// 不会触发修正，原样返回；这是各裁剪/续跑路径的最后防线。
func sanitizeToolPairing(messages []ReactMessage) []ReactMessage {
	var out []ReactMessage
	changed := false
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role == "tool" {
			// 配对的 tool 结果已在下方 assistant 分支里被消费（i 随之前移）；
			// 能走到这里的是孤立结果，丢弃。
			changed = true
			continue
		}
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		// 收集紧随其后的连续 tool 结果并按 ToolCallID 配对。
		pending := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			pending[tc.ID] = true
		}
		for i+1 < len(messages) && messages[i+1].Role == "tool" {
			i++
			t := messages[i]
			if pending[t.ToolCallID] {
				delete(pending, t.ToolCallID)
				out = append(out, t)
			} else {
				changed = true // 与当前 tool_calls 不匹配的结果：丢弃
			}
		}
		// 缺失响应的 tool_calls 按原顺序补合成错误结果。
		for _, tc := range m.ToolCalls {
			if !pending[tc.ID] {
				continue
			}
			changed = true
			out = append(out, ReactMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content: ToolResultJSON(ToolResult{
					Tool:  tc.Name,
					Error: "tool result missing: interrupted by context windowing or message injection",
				}),
			})
		}
	}
	if !changed {
		return messages
	}
	return out
}

// truncateRunes 按 rune 数截断字符串并追加省略提示（textutil 单源）。
func truncateRunes(s string, n int) string {
	return textutil.TruncateRunes(s, n, "...(truncated)")
}

// systemPrompt 为当前角色构建系统提示词。
// 基础提示来自角色配置；头部插入运行环境（OS/时区/工作目录），
// 末尾追加一段统一的执行纪律，用于减少常见反模式
// （无目的工具调用、未验证就声称完成、忘记 mailbox 消息语义等）。
// TODO #40 块 4：结果按实例冻结（sync.Once）——envBlock（含 PROJECT.md）、画像/人格
// 在 Agent 存活期内字节稳定，跨轮 DeepSeek 前缀缓存命中；resume 重建新实例读最新文件。
func (a *ReActAgent) systemPrompt() string {
	a.sysPromptOnce.Do(func() {
		a.sysPromptCache = a.buildSystemPrompt()
	})
	return a.sysPromptCache
}

// buildSystemPrompt 是 systemPrompt 的实质实现（每实例只构建一次）。
func (a *ReActAgent) buildSystemPrompt() string {
	// 取角色配置中的系统提示作为基础。
	base := a.role.SystemPrompt

	// 如果角色未配置系统提示，则使用默认兜底文案。
	if base == "" {
		base = "You are a helpful assistant."
	}

	// 头部环境信息：OS、时区、工作目录（全部跨轮字节稳定）。让 LLM 用对 OS 的 shell 语法
	// （Windows 用 PowerShell，Linux/macOS 用 bash/sh）与正确的相对路径。
	// 注意：当前时间不在此处（TODO #40 缓存修复）——动态时间会打碎 system Instruction
	// 前缀导致 DeepSeek 前缀缓存每轮失效，改为独立尾部 system 消息注入（buildTimeMessage）。
	envBlock := buildEnvBlock(a.workDir, a.agentsMDMaxRunes)

	// 在基础提示后追加执行纪律块，与角色提示同语言（中文），覆盖：
	// 工具使用节制、产出后验证（代码走机器校验、非代码走纸面对照）、完成即停、mailbox 消息语义。
	discipline :=
		"【执行纪律】\n" +
			"1. 只在必要时调用工具；先用 SearchInFiles/ListDir 定位，再按需 ReadFile；不重复读取已读过的文件。\n" +
			"2. 产出或修改文件后必须验证：代码类产出用 RunCommand 跑构建/测试/语法检查；非代码产出对照任务验收标准逐条核对。没有验证证据不得声称完成。\n" +
			"3. 任务完成立即停止调用工具，输出最终答复；答复必须自包含：做了什么、结果如何、关键产出与文件路径。\n" +
			"4. 形如 [mailbox from <agent_id>] 的消息是异步子 Agent 回传的结果摘要，阅读后整合进当前结论；若摘要表明失败，决定重试、自己接手或在答复中说明。\n"
	prompt := envBlock + "\n\n" + base + "\n\n" + discipline
	// 技能元数据块（渐进披露第一层）追加在纪律块之后：持有技能的名称+一句话描述 +
	// load_skill 取全文/派发下放提示。放在尾部只 fork 提示词尾部，envBlock+base+纪律块
	// 的公共前缀跨 Agent 保持逐字节一致（前缀缓存跨实例复用，口径同 responsibility 注入）。
	if a.skillBlock != "" {
		prompt += "\n\n" + a.skillBlock
	}
	// 记忆索引槽（TODO #20③）：再往尾部追加——与 skillBlock 同为尾部注入位，
	// 公共前缀（env+base+纪律+技能）跨实例/跨会话字节稳定。
	if a.memoryIndex != "" {
		prompt += "\n\n" + a.memoryIndex
	}
	// 提示词构成分段计量（TODO #15①）：各分段 rune 快照，首轮 LLM 调用输出明细行，
	// 供瘦身边际对照（T13 meta 收窄前后对比 sys 分段降幅）。sync.Once 内写入，无竞争。
	seps := 4 // env+base、base+discipline 两处 "\n\n"
	if a.skillBlock != "" {
		seps += 2
	}
	if a.memoryIndex != "" {
		seps += 2
	}
	a.promptStatsSegs = map[string]int{
		"env":          utf8.RuneCountInString(envBlock),
		"role_base":    utf8.RuneCountInString(base),
		"discipline":   utf8.RuneCountInString(discipline),
		"skill":        utf8.RuneCountInString(a.skillBlock),
		"memory_index": utf8.RuneCountInString(a.memoryIndex),
	}
	// 人格注入器非 nil 时，把人格内容拼到完整 prompt 最前（envBlock 之前），
	// 作为用户级人格前缀。人格为空时 Inject 原样返回，无副作用。
	if a.persona != nil {
		prompt = a.persona.Inject(prompt)
		// 人格规模按差值计（Injector 可能拼多段前缀）：总量 − 其余分段与分隔符。
		if total := utf8.RuneCountInString(prompt); total > 0 {
			other := a.promptStatsSegs["env"] + a.promptStatsSegs["role_base"] +
				a.promptStatsSegs["discipline"] + a.promptStatsSegs["skill"] +
				a.promptStatsSegs["memory_index"] + seps
			if total > other {
				a.promptStatsSegs["persona"] = total - other
			}
		}
	}
	return prompt
}

// buildEnvBlock 构造环境信息块，注入到系统提示词头部。
// 包含 OS（含 Windows 主版本判断）、时区、工作目录——全部跨轮字节稳定
// （TODO #40：动态时间已移出，见 buildTimeMessage；本函数输出可被 DeepSeek 前缀缓存命中）。
// agentsMDMaxRunes > 0 时追加 workDir 根部 AGENTS.md/CLAUDE.md 的【项目自述】段
//（TODO 第10项⑦冷启动注入；mtime 缓存命中时零读取，缺失零开销）。
// workDir 为空时回退到进程 cwd。
func buildEnvBlock(workDir string, agentsMDMaxRunes int) string {
	osName := runtime.GOOS
	// Windows 主版本细判：仅给 LLM "windows" 足够，但显式标注能让 LLM 选择正确的 shell 语法。
	osLabel := osName
	switch osName {
	case "windows":
		osLabel = "Windows（PowerShell，命令需用 PS 语法：2>$null 而非 2>nul，Get-ChildItem 而非 dir）"
	case "linux":
		osLabel = "Linux（bash/sh）"
	case "darwin":
		osLabel = "macOS（bash/zsh）"
	}

	// 时区：本地时区名。会话内稳定（用户不换时区），可入缓存前缀。
	tzName := "UTC"
	if loc := time.Now().Location(); loc != nil && loc.String() != "" {
		tzName = loc.String()
	}

	// 工作目录：为空时回退到 cwd，保证始终有值。
	wd := workDir
	if wd == "" {
		wd = "(进程当前目录)"
	}

	env := "【运行环境】\n" +
		fmt.Sprintf("- 操作系统: %s\n", osLabel) +
		fmt.Sprintf("- 时区: %s\n", tzName) +
		fmt.Sprintf("- 工作目录: %s", wd)

	// 项目概览：注入 .bma/PROJECT.md 的 managed 区正文（首个 session 启动时启发式生成）。
	// 缺失或无标记返回空串，略去本段。让 Agent 了解工作目录的模块/领域拆分/命令/文档地图。
	// TODO #40 块 4：会话级冻结（projectRefresher 刷新不污染当前会话前缀）另行处理。
	if projDoc := project.LoadProjectDoc(workDir); projDoc != "" {
		env += "\n\n【项目概览】\n" + projDoc
	}
	// 项目自述（TODO 第10项⑦）：workDir 根部 AGENTS.md（优先）或 CLAUDE.md 的项目级说明，
	// 截断至 agentsMDMaxRunes 防膨胀。与 PROJECT.md 同属跨轮字节稳定段（mtime 缓存命中
	// 零读取）；缺失/关闭时零注入。
	if brief := project.LoadProjectBrief(workDir, agentsMDMaxRunes); brief != "" {
		env += "\n\n【项目自述】\n" + brief
	}
	return env
}

// buildTimeMessage 构造当前时间尾部 system 消息（TODO #40 块 1 缓存修复核心）：
// 时间每轮必变，放在系统提示词头部会把 system Instruction 前缀打碎、整条缓存失效；
// 放在消息尾部（近期事件/看板段之后）——内容变化只影响不可缓存尾部，前缀稳定命中。
// 时间精度不降（仍秒级），模型每轮可见。
func buildTimeMessage() ReactMessage {
	now := time.Now()
	return ReactMessage{
		Role:    "system",
		Content: "【当前时间】" + now.Format("2006-01-02 15:04:05 MST"),
	}
}

// mailboxMessageToReact 把异步 mailbox 消息转换为模型可见的 ReactMessage。
// 使用 user role 并在内容前加 [mailbox] 前缀，兼容 Anthropic Messages API
// （该 API 不允许在会话中途插入 system 消息）。
//
// 安全（TODO #18-4 防线延伸到 Agent 间通道）：主题/正文/载荷对其他 Agent 而言
// 均为不可信内容（LLM 生成，可能转述过被污染的外部源），包进 untrusted 围栏；
// 框架信号留在围栏外——[升级] 前缀、[mailbox from X] 前缀与"修改文件"清单。
// From=user 的用户直接指令不围栏：用户指令优先级最高，降格为"数据"会违义。
func mailboxMessageToReact(m *mailbox.Message) ReactMessage {
	// 主题作为不可信正文的基础部分。
	untrusted := m.Subject

	// 如果邮件有正文，则追加到主题之后。
	if m.Body != "" {
		untrusted += "\n" + m.Body
	}

	// 如果邮件携带结构化载荷，则序列化为 JSON 字符串并追加，方便模型读取。
	if len(m.Payload) > 0 {
		b, _ := json.Marshal(m.Payload)
		untrusted += "\n" + string(b)
	}

	body := untrusted
	if m.From != "user" {
		body = tool.WrapUntrusted("mail:"+m.From, untrusted)
	}

	// 升级消息（TODO #23）加 [升级] 前缀（围栏外），父 LLM 一眼识别"需要干预"类消息，
	// 按 meta prompt 的升级处置规程（重派/接手/回报用户）决策。
	if m.Type == mailbox.MsgEscalate {
		body = "[升级] " + body
	}

	// Layer 5：展示子 Agent 修改的文件清单（框架生成，围栏外），使父 LLM 知晓子改了哪些文件。
	if len(m.FilesModified) > 0 {
		body += "\n修改文件: " + strings.Join(m.FilesModified, ", ")
	}

	// 组合成带发送者标记的 user 消息返回。
	return ReactMessage{Role: "user", Content: fmt.Sprintf("[mailbox from %s] %s", m.From, body)}
}

// drainMailbox 取出所有以当前 Agent 为收件人的未读 mailbox 消息，
// 转为 user 消息追加到 history，并推送实时事件 + 写记忆事件。
// 返回更新后的 history 与新注入的消息数；mailbox 未注入时原样返回 0。
func (a *ReActAgent) drainMailbox(history []ReactMessage) ([]ReactMessage, int) {
	if a.mailbox == nil {
		return history, 0
	}
	msgs := a.mailbox.Drain(a.name)
	// 注入测量（TODO 第七项③）：本回合邮箱入站总 runes，量化回传对上下文的贡献。
	if len(msgs) > 0 {
		inbound := 0
		for _, m := range msgs {
			inbound += len([]rune(m.Body))
		}
		log.Printf("[agent] ctx_inject: agent=%s stage=mailbox_inbound msgs=%d inbound=%d runes", a.name, len(msgs), inbound)
	}
	for _, m := range msgs {
		// 将 mailbox 消息转为模型可见的 user 角色消息并加入历史。
		history = a.appendLogged(history, mailboxMessageToReact(m))

		// 实时推送子 Agent 完成事件，UI 可据此更新"等待子 Agent"状态。
		// 用户注入（From=user，如运行中重新下达指令）不是子 Agent 完成，不推该事件——
		// 否则 UI 会多出一条 "子Agent 完成: user" 的假完成记录。
		// 系统通知（From=system，依赖就绪等）同理：非完成也非询问，不推活动事件，
		// 正文照常入史供 LLM 消化。
		// 协作询问（request/escalate）单列 peer_ask：语义是"需要本 Agent 回答"而非
		// "某子 Agent 完成"，混在 sub_agent_done 里既误导用户也淹没问答使用情况。
		if m.From != "user" && m.From != "system" {
			kind := LiveEventSubAgentDone
			switch {
			case m.Type == mailbox.MsgRequest || m.Type == mailbox.MsgEscalate:
				kind = LiveEventPeerAsk
			case m.Type == mailbox.MsgMilestone || (m.Type == mailbox.MsgInfo && strings.HasPrefix(m.Subject, "里程碑:")):
				// 里程碑是中途播报不是终态回传，单列 kind 防前端误读为完成（C-2）。
				kind = LiveEventMilestone
			}
			a.emitLive(LiveEvent{Kind: kind, Tool: m.From, Text: truncateRunes(m.Subject+m.Body, 200)})
		}

		// 同时把子代理摘要作为记忆事件写入，供后续上下文组装使用。
		a.memory.Write(a.name, MemoryEvent{
			Type:     "sub_agent_summary",
			AgentID:  a.name,
			Role:     m.From,
			Content:  m.Body,
			Occurred: time.Now(),
		})
	}
	return history, len(msgs)
}

// mustMarshal 将任意值序列化为 JSON 字符串；如果序列化失败则返回空字符串。
// 用于工具调用参数等场景的容错记录。
func mustMarshal(v any) string {
	// 尝试 JSON 序列化。
	b, err := json.Marshal(v)
	if err != nil {
		// 失败时静默返回空字符串，避免影响主流程。
		return ""
	}
	return string(b)
}
