// Package agent 实现单代理的 ReAct（推理-行动）循环：
// LLM 生成 -> 工具调用 -> 工具结果 -> 重复，直到产生最终答案或达到迭代上限。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/domain/tool"
	"github.com/blockmemory/agent/backend/internal/logger"
	"github.com/blockmemory/agent/backend/internal/mailbox"
	"github.com/blockmemory/agent/backend/internal/middleware"
	"github.com/blockmemory/agent/backend/internal/project"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// ReActAgent 实现单个 ReAct 循环：LLM 生成 -> 工具调用 -> 工具结果 -> 重复。
// 它在多次运行之间刻意保持无状态；所有可变状态都保存在传入并返回的
// History 切片中，便于上层按需持久化或续跑。
type ReActAgent struct {
	llm     ModelProvider        // llm 是当前使用的语言模型提供者，负责生成回复。
	tools   ToolRegistry         // tools 是已注册的工具集合，提供 JSON Schema 与分发执行能力。
	memory  MemoryPipeline       // memory 是记忆流水线，用于在每次 LLM 调用前组装上下文、写入事件。
	mailbox *mailbox.Mailbox     // mailbox 是共享邮箱，用于接收异步子代理摘要；为空时不轮询。
	role    types.RoleDefinition // role 是当前代理的角色定义，包含系统提示等配置。
	name    string               // name 是代理唯一标识，也用于上下文中的 agent ID。
	maxIter int                  // maxIter 是单次 Run 中允许的最大 LLM 调用次数，防止死循环。

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
	// tokenBudget 单次 RunWithHistory 累计 token 上限；<=0 不限制。超限 break 返回部分完成。
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
	// pausedChecker 可选的"是否有 Paused 子 DomainAgent"检查器，由 WithPausedChildChecker 注入。
	// 父终结保护 wait loop 中检查：若有 Paused 子节点（触达 token 上限），父 MetaAgent
	// 无限 budget 不会自行暂停，需靠此检查跳出 wait loop 返回 PausedOnChild，由上层 pauseSession
	// 置会话暂停态。为 nil 时不检查（默认关闭，仅 MetaAgent 注入）。
	pausedChecker PausedChildChecker
	// activityReporter 可选的活动上报回调，由 Dispatcher 心跳巡检注入。
	// 每次 generateOnce 与工具派发时触发，更新 Dispatcher 侧最后活动时间戳；
	// 巡检发现超阈值无活动则判定子 Agent 假死（LLM 流式挂起等），主动 cancel。
	// 为 nil 时跳过（测试场景或 DomainAgent/MetaAgent 不注入），不影响主流程。
	activityReporter func()
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
	if current == nil {
		return nil
	}
	if maxRunes <= 0 {
		maxRunes = 2000
	}
	return &userProfileInjector{current: current, maxRunes: maxRunes}
}

type userProfileInjector struct {
	current  func() string
	maxRunes int
}

func (p *userProfileInjector) Inject(systemPrompt string) string {
	content := strings.TrimSpace(p.current())
	if content == "" {
		return systemPrompt
	}
	if len([]rune(content)) > p.maxRunes {
		content = string([]rune(content)[:p.maxRunes]) + "\n...（画像截断）"
	}
	return "【用户画像】\n" + content + "\n\n" + systemPrompt
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
	// TokenBudget 单次 RunWithHistory 累计 token 上限（input+output 之和，跨轮累加）。
	// <=0 不限制；>0 超限后主循环 break 返回部分完成（LimitReached）。
	TokenBudget int
}

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

// WithActivityReporter 注入活动上报回调，供 Dispatcher 心跳巡检判断子 Agent 是否假死。
// 每次 generateOnce 与工具派发触发；传 nil 关闭（默认关闭）。
// 仅叶子 Agent 注入：DomainAgent/MetaAgent 有自身 wait loop，注入会误杀合法等待。
func (a *ReActAgent) WithActivityReporter(fn func()) *ReActAgent {
	a.activityReporter = fn
	return a
}

// touchActivity 上报一次活动；未注入回调时为空操作。
func (a *ReActAgent) touchActivity() {
	if a.activityReporter != nil {
		a.activityReporter()
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

// historyToolCallInputMaxRunes 是写入历史的工具入参单字符串值最大 rune 数。
// 与 ToolOutputMaxRunes（工具输出截断）对称：工具入参（WriteFile 全文、codegen 大段代码）
// 不截断会在滑动窗口内累积成单轮 100K+ input tokens，使续跑预算一次耗尽、暂停/恢复零进展。
const historyToolCallInputMaxRunes = 2000

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

	// 如果外部传入 nil 历史，则初始化为空切片，保证后续 append 安全。
	if history == nil {
		history = []ReactMessage{}
	}

	// 将本轮用户输入作为一条 user 消息追加到历史中，开启新一轮 ReAct。
	history = append(history, ReactMessage{Role: "user", Content: input})

	// 根据当前角色构建系统提示词，作为模型行为约束。
	system := a.systemPrompt()

	// 进入 ReAct 主循环，最多执行 maxIter 次 LLM 调用（maxIter<=0 时不限制）。
	// 每次循环对应一次“思考-行动-观察”的迭代。
	// emptyStreak 记录连续空响应次数，用于空响应保护（见循环内注释）。
	emptyStreak := 0
	// usedTokens 累计本次 RunWithHistory 的 LLM token 消耗（input+output 之和）。
	// tokenBudget>0 时，超限即 break 返回部分完成，防止单目标 token 成本无上限累积。
	var usedTokens int64
	for i := 0; a.maxIter <= 0 || i < a.maxIter; i++ {
		// 在每次调用 LLM 之前，通过记忆流水线组装上下文消息。
		// 这可能会压缩历史、注入相关记忆或做其他上下文管理。
		assembled := a.memory.Assemble(a.role, a.name, history)

		// 上下文裁剪策略：仅 windowMessages 滑动窗口（硬上限，防 API 上下文溢出）。
		// 历史压缩（hot/cold 分层）已迁入 memory.Pipeline.Assemble，按步频触发；
		// ReActAgent 不再直接做历史压缩，职责归位到记忆层。
		// windowMessages 只影响本次请求，不修改 history（完整历史仍用于持久化与续跑）。
		messages := windowMessages(assembled, a.historyMaxMessages)
		// 兜底防线：任何裁剪/注入路径若留下不配对的 tool 调用（assistant tool_calls
		// 未紧随 tool 结果，或孤立 tool 结果），Anthropic/OpenAI 会以 400 拒绝整轮请求，
		// 此前所有 LLM 耗时全部作废（实证 domain-2 白跑 31m43s）。发送前强制配对。
		messages = sanitizeToolPairing(messages)

		// 将内部消息格式转换为 blades 库所需的模型消息格式。
		bladesMsgs := ToBladesMessages(messages)

		// 构造模型请求：包含系统提示、历史消息和可用工具 schema。
		req := &blades.ModelRequest{
			Instruction: blades.SystemMessage(system),
			Messages:    bladesMsgs,
			Tools:       a.tools.Schema(),
		}

		// 调用 LLM 生成回复（带重试与单次超时）；重试耗尽后返回错误。
		resp, err := a.generate(ctx, req)
		if err != nil {
			// 出错时返回已累计的历史，便于上层回传部分进度或排查。
			return ReactResult{History: history}, fmt.Errorf("llm generate: %w", err)
		}

		// 推送本次 LLM 调用的 token 用量；provider 未填充用量时跳过（mock/测试）。
		if usage := resp.Message.TokenUsage; usage.InputTokens > 0 || usage.OutputTokens > 0 {
			a.emitLive(LiveEvent{
				Kind:         LiveEventTokenUsage,
				InputTokens:  usage.InputTokens,
				OutputTokens: usage.OutputTokens,
			})
		}
		// 累计 token 预算：优先 TotalTokens（provider 填充时），否则用 input+output 之和。
		// 空响应（mock/测试）不计入，避免误触预算上限。
		if usage := resp.Message.TokenUsage; usage.InputTokens > 0 || usage.OutputTokens > 0 {
			delta := usage.TotalTokens
			if delta <= 0 {
				delta = usage.InputTokens + usage.OutputTokens
			}
			usedTokens += delta
		}
		// 超预算：不视为错误，返回部分完成 + 完整历史，由上层暂停会话等用户续跑。
		// 与 maxIter 轮数上限正交：先到哪个用哪个。
		if a.tokenBudget > 0 && usedTokens > int64(a.tokenBudget) {
			log.Printf("[react] token budget exceeded: role=%s used=%d budget=%d", a.role.Name, usedTokens, a.tokenBudget)
			return ReactResult{History: history, LimitReached: true}, nil
		}

		// 将 blades 返回的消息转换为内部 Assistant 消息。
		assistant := AssistantMessageFromBlades(resp.Message)

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
			history = append(history, ReactMessage{Role: "user", Content: emptyResponseNudge})
			continue
		}
		emptyStreak = 0

		// 非空响应才追加到历史中。入史副本截断超长工具入参（如 WriteFile 全文件内容）：
		// 完整入参仅用于本次派发执行；历史/持久化/续跑只保留截断副本。
		// 否则大文件内容在滑动窗口内逐轮重发，单轮 input 即可耗尽整份 token 预算
		// （实证：塔防 domain 续跑首轮即再触限，pause/resume 零进展死锁）。
		history = append(history, truncateToolCallInputsForHistory(assistant, historyToolCallInputMaxRunes))

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
				continue
			}
			// 父会话终结保护：若仍有未决子 Agent（call_sub_agent 派发后尚未回传结果），
			// 阻塞等待其完成而非立即终结，防止迟到 mailbox 消息随会话销毁丢失。
			// 多 Agent 协作验证闭环（code<->test 互问互答）的关键正确性保障。
			if a.pendingChecker != nil && a.pendingChecker.PendingChildren(a.name) > 0 {
				// 等待期间不烧 LLM 轮次：纯阻塞等子 Agent 完成信号，仅当 mailbox
				// 取到新摘要时才 break 回主循环调 LLM 整合；超时无新消息则继续等。
				// 旧实现每 30s 超时白跑一次 LLM，50 轮上限烧完后会话停摆等用户
				// 人工续跑（实证：塔防任务死等 46 分钟）。
				for a.pendingChecker.PendingChildren(a.name) > 0 {
					// Paused 子 DomainAgent 检查：MetaAgent 无限 budget 不会因自身 token 暂停，
					// 但子 domain 触达上限进入 Paused 后,父在此 wait loop 会永久阻塞。
					// 检测到 Paused 子节点时跳出，返回 PausedOnChild 让上层 pauseSession
					// 置会话暂停态，等用户"继续"恢复该 domain（各 Agent 独立上下文）。
					if a.pausedChecker != nil && a.pausedChecker.HasPausedChild(a.name) {
						return ReactResult{History: history, LimitReached: true, PausedOnChild: true}, nil
					}
					a.pendingChecker.WaitForAnyChild(a.name, 30*time.Second)
					var n int
					history, n = a.drainMailbox(history)
					if n > 0 || a.mailbox == nil || ctx.Err() != nil {
						break
					}
				}
				continue
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

		// 否则，按顺序执行助手请求的所有工具调用，并将结果反馈回会话。
		// 工具调用顺序执行，以保持追踪顺序与模型调用顺序一致。
		for _, tc := range assistant.ToolCalls {
			// 实时推送工具调用开始事件，UI 可据此展示"执行中"状态。
			a.emitLive(LiveEvent{Kind: LiveEventToolCall, Tool: tc.Name, Input: mustMarshal(tc.Input)})

			// 工具派发前上报活动：工具 hang 时无后续活动，心跳巡检可捕获。
			a.touchActivity()
			// 分发执行单个工具调用；出错时构造包含错误信息的 ToolResult。
			result, err := a.tools.Dispatch(ctx, tc)
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

			// 截断工具输出后再写入历史，避免单次大输出（如整文件/长日志）
			// 在历史中无限累积，导致后续每轮请求 token 爆炸。
			if a.toolOutputMaxRunes > 0 {
				result.Output = truncateRunes(result.Output, a.toolOutputMaxRunes)
			}

			// 将工具执行结果以 tool 角色消息追加到历史中，供下一次 LLM 调用使用。
			// ToolCallID 携带本次调用的 ID：Anthropic/OpenAI 原生工具协议要求
			// tool_result 必须引用对应的 tool_use id，否则下一轮请求会被 API 拒绝。
			history = append(history, ReactMessage{
				Role:       "tool",
				Content:    ToolResultJSON(result),
				ToolCallID: tc.ID,
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
		history, _ = a.drainMailbox(history)
	}

	// 达到最大迭代次数上限（仅 maxIter>0 时可能触发）：
	// 不视为错误——返回 LimitReached 标记与完整历史，
	// 由上层将会话置为暂停并提示用户发送消息续跑，而不是判定任务失败。
	return ReactResult{History: history, LimitReached: true}, nil
}

// generate 包装一次 LLM 调用：带单次超时与指数退避重试（TODO #19 LLM 链）。
// 重试/退避/超时/空响应判定迁移到 middleware 包（RetryLLM + CallLLM + TerminalCall），
// 行为不变：重试 retryCount+1 次尝试；会话取消（ctx.Err() 非空）与单次调用超时
// （DeadlineExceeded，慢推理模型重试只会重复超时——实证 180s×4=12min）不重试；
// 空响应不重试。链式形态即未来新增横切中间件的挂载点。
func (a *ReActAgent) generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	c := &middleware.LLMCtx{Request: req, Call: a.generateOnce}
	chain := middleware.New[middleware.LLMCtx]().
		Use(middleware.RetryLLM(a.retryCount, a.retryBackoff, nil)).
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
	a.touchActivity()
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
	var inTok, outTok int64
	if resp != nil && resp.Message != nil {
		response = serializeResponseForLog(resp.Message)
		inTok = resp.Message.TokenUsage.InputTokens
		outTok = resp.Message.TokenUsage.OutputTokens
	}
	errStr := ""
	if callErr != nil {
		errStr = callErr.Error()
		response = "[ERROR] " + errStr + "\n" + response
	}
	a.log.LLMCall(ctx, logger.LLMCallRecord{
		Agent:        a.role.Name,
		Model:        a.llmModelName(),
		Prompt:       prompt,
		Response:     response,
		InputTokens:  int(inTok),
		OutputTokens: int(outTok),
		LatencyMs:    int(dur.Milliseconds()),
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
// 单个 part 截断 2000 runes：与入史截断（historyToolCallInputMaxRunes）对齐，防日志爆炸。
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
	for resp, err := range sp.NewStreaming(ctx, req) {
		if err != nil {
			return nil, err
		}
		if resp == nil || resp.Message == nil {
			continue
		}
		final = resp
		// 思考过程增量（provider 经 Metadata 传递）：瞬时推送，答复文本开始输出后由
		// 上层清除；思考内容不进入答复文本，避免与正式输出混淆。
		if thinking, ok := resp.Message.Metadata["thinking"].(string); ok && thinking != "" {
			a.emitLive(LiveEvent{Kind: LiveEventThinkDelta, Text: thinking})
			continue
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

// summarizeWindow 已迁入 domain/memory/pipeline.go 的 compressHistory 函数。
// 历史压缩（hot/cold 分层）属记忆层职责，ReActAgent 不再直接做历史压缩。
// 触发由 memory.Pipeline.WithCompression(every, keepRecent) 配置，bootstrap 注入。

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

// truncateRunes 按 rune 数截断字符串并追加省略提示。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...(truncated)"
}

// systemPrompt 为当前角色构建系统提示词。
// 基础提示来自角色配置；头部插入运行环境（OS/时区/时间/工作目录），
// 末尾追加一段统一的执行纪律，用于减少常见反模式
// （无目的工具调用、未验证就声称完成、忘记 mailbox 消息语义等）。
func (a *ReActAgent) systemPrompt() string {
	// 取角色配置中的系统提示作为基础。
	base := a.role.SystemPrompt

	// 如果角色未配置系统提示，则使用默认兜底文案。
	if base == "" {
		base = "You are a helpful assistant."
	}

	// 头部环境信息：OS、时区、当前时间、工作目录。让 LLM 用对 OS 的 shell 语法
	// （Windows 用 PowerShell，Linux/macOS 用 bash/sh）与正确的相对路径。
	envBlock := buildEnvBlock(a.workDir)

	// 在基础提示后追加执行纪律块，与角色提示同语言（中文），覆盖：
	// 工具使用节制、修改后验证、完成即停、mailbox 消息语义。
	prompt := envBlock + "\n\n" + base + "\n\n" +
		"【执行纪律】\n" +
		"1. 只在必要时调用工具；先用 SearchInFiles/ListDir 定位，再按需 ReadFile；不重复读取已读过的文件。\n" +
		"2. 修改代码或文件后，用 RunCommand 验证（构建/测试/检查），没有验证证据不得声称完成。\n" +
		"3. 任务完成立即停止调用工具，输出最终答复；答复必须自包含：做了什么、结果如何、关键文件路径。\n" +
		"4. 形如 [mailbox from <agent_id>] 的消息是异步子 Agent 回传的结果摘要，阅读后整合进当前结论；若摘要表明失败，决定重试、自己接手或在答复中说明。\n"
	// 人格注入器非 nil 时，把人格内容拼到完整 prompt 最前（envBlock 之前），
	// 作为用户级人格前缀。人格为空时 Inject 原样返回，无副作用。
	if a.persona != nil {
		prompt = a.persona.Inject(prompt)
	}
	return prompt
}

// buildEnvBlock 构造环境信息块，注入到系统提示词头部。
// 包含 OS（含 Windows 主版本判断）、时区、当前时间、工作目录。
// workDir 为空时回退到进程 cwd。
func buildEnvBlock(workDir string) string {
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

	// 时区与当前时间：用本地时区名 + RFC3339 时间，便于 LLM 处理时间相关任务。
	tzName := "UTC"
	now := time.Now()
	if loc := now.Location(); loc != nil && loc.String() != "" {
		tzName = loc.String()
	}
	timeStr := now.Format("2006-01-02 15:04:05 MST")

	// 工作目录：为空时回退到 cwd，保证始终有值。
	wd := workDir
	if wd == "" {
		wd = "(进程当前目录)"
	}

	env := "【运行环境】\n" +
		fmt.Sprintf("- 操作系统: %s\n", osLabel) +
		fmt.Sprintf("- 时区: %s\n", tzName) +
		fmt.Sprintf("- 当前时间: %s\n", timeStr) +
		fmt.Sprintf("- 工作目录: %s", wd)

	// 项目概览：注入 .bma/PROJECT.md 的 managed 区正文（首个 session 启动时启发式生成）。
	// 缺失或无标记返回空串，略去本段。让 Agent 了解工作目录的模块/领域拆分/命令/文档地图。
	if projDoc := project.LoadProjectDoc(workDir); projDoc != "" {
		env += "\n\n【项目概览】\n" + projDoc
	}
	return env
}

// mailboxMessageToReact 把异步 mailbox 消息转换为模型可见的 ReactMessage。
// 使用 user role 并在内容前加 [mailbox] 前缀，兼容 Anthropic Messages API
// （该 API 不允许在会话中途插入 system 消息）。
func mailboxMessageToReact(m *mailbox.Message) ReactMessage {
	// 主题作为消息正文的基础部分。
	body := m.Subject

	// 升级消息（TODO #23）加 [升级] 前缀，父 LLM 一眼识别"需要干预"类消息，
	// 按 meta prompt 的升级处置规程（重派/接手/回报用户）决策。
	if m.Type == mailbox.MsgEscalate {
		body = "[升级] " + body
	}

	// 如果邮件有正文，则追加到主题之后。
	if m.Body != "" {
		body += "\n" + m.Body
	}

	// 如果邮件携带结构化载荷，则序列化为 JSON 字符串并追加，方便模型读取。
	if len(m.Payload) > 0 {
		b, _ := json.Marshal(m.Payload)
		body += "\n" + string(b)
	}

	// Layer 5：展示子 Agent 修改的文件清单，使父 LLM 知晓子改了哪些文件。
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
	for _, m := range msgs {
		// 将 mailbox 消息转为模型可见的 user 角色消息并加入历史。
		history = append(history, mailboxMessageToReact(m))

		// 实时推送子 Agent 完成事件，UI 可据此更新"等待子 Agent"状态。
		a.emitLive(LiveEvent{Kind: LiveEventSubAgentDone, Tool: m.From, Text: truncateRunes(m.Body, 200)})

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
