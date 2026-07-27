// Package agent 实现单代理的 ReAct（推理-行动）循环：
// LLM 生成 -> 工具调用 -> 工具结果 -> 重复，直到产生最终答案或达到迭代上限。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
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
	// liveFn 是实时进度事件回调，由 WithLiveEvents 注入；nil 时不推送任何进度事件。
	liveFn func(LiveEvent)
	// pendingChecker 可选的未决子 Agent 检查器，由 WithPendingChildrenChecker 注入；
	// 为 nil 时关闭终结保护（默认关闭，仅在 bootstrap 装配 Dispatcher 后开启）。
	pendingChecker PendingChildrenChecker
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

// RunWithHistory 从已有历史开始执行 ReAct 循环。
// 参数 ctx 用于取消/超时控制；input 为新的用户输入；history 为已有会话历史。
// 新输入会被追加到传入的历史中，便于会话续跑并保留之前的轮次。
// 返回值 ReactResult 包含最终回复与完整历史；error 表示执行错误。
func (a *ReActAgent) RunWithHistory(ctx context.Context, input string, history []ReactMessage) (ReactResult, error) {
	// 将当前代理标识写入上下文，便于链路追踪、日志和工具调用时识别身份。
	ctx = WithAgentID(ctx, a.name)
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
	for i := 0; a.maxIter <= 0 || i < a.maxIter; i++ {
		// 在每次调用 LLM 之前，通过记忆流水线组装上下文消息。
		// 这可能会压缩历史、注入相关记忆或做其他上下文管理。
		messages := a.memory.Assemble(a.role, a.name, history)

		// 滑动窗口裁剪：防止长任务历史无限增长导致 token 爆炸与上下文窗口溢出。
		// 仅影响本次请求，不修改 history 本身（完整历史仍用于持久化与续跑）。
		messages = windowMessages(messages, a.historyMaxMessages)

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

		// 非空响应才追加到历史中。
		history = append(history, assistant)

		// 在判断本轮是否结束之前，先轮询邮箱并注入任何新的异步消息。
		// mailbox 由服务装配层注入共享邮箱（用于接收异步子代理摘要）；未注入时跳过。
		if a.mailbox != nil {
			// Drain 取出所有以当前代理为收件人的未读消息。
			for _, m := range a.mailbox.Drain(a.name) {
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
		}

		// 如果助手消息中没有任何工具调用，说明本轮已产生最终答案。
		if len(assistant.ToolCalls) == 0 {
			// 父会话终结保护：若仍有未决子 Agent（call_sub_agent 派发后尚未回传结果），
			// 阻塞等待其完成而非立即终结，防止迟到 mailbox 消息随会话销毁丢失。
			// 多 Agent 协作验证闭环（code<->test 互问互答）的关键正确性保障。
			if a.pendingChecker != nil && a.pendingChecker.PendingChildren(a.name) > 0 {
				// 阻塞等待任一子 Agent 完成或超时；超时后继续循环由 maxIter 兜底。
				// 收到信号后 continue，下一轮迭代会 Drain mailbox 取到子 Agent 结果摘要，
				// 模型基于新信息重新生成答复（可能再次给出终答，此时若仍有未决则继续等待）。
				a.pendingChecker.WaitForAnyChild(a.name, 30*time.Second)
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

			// 分发执行单个工具调用；出错时构造包含错误信息的 ToolResult。
			result, err := a.tools.Dispatch(ctx, tc)
			if err != nil {
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
	}

	// 达到最大迭代次数上限（仅 maxIter>0 时可能触发）：
	// 不视为错误——返回 LimitReached 标记与完整历史，
	// 由上层将会话置为暂停并提示用户发送消息续跑，而不是判定任务失败。
	return ReactResult{History: history, LimitReached: true}, nil
}

// generate 包装一次 LLM 调用：带单次超时与指数退避重试。
// 重试次数为 retryCount+1 次尝试；会话被取消（ctx.Err() 非空）时不重试，直接返回。
func (a *ReActAgent) generate(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	attempts := a.retryCount + 1
	if attempts < 1 {
		attempts = 1
	}
	backoff := a.retryBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		// 首次之后的尝试先按指数退避等待。
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		// 单次调用超时从会话 ctx 派生，不影响会话整体的取消语义。
		callCtx := ctx
		cancel := context.CancelFunc(func() {})
		if a.llmTimeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, a.llmTimeout)
		}
		resp, err := a.generateOnce(callCtx, req)
		cancel()
		if err != nil {
			lastErr = err
			// 会话本身被取消/超时，属用户或上层主动行为，不再重试。
			if ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		// 响应为空属模型异常，不重试（重试大概率同样为空），直接报错。
		if resp == nil || resp.Message == nil {
			return nil, errors.New("empty model response")
		}
		return resp, nil
	}
	return nil, lastErr
}

// streamingModelProvider 是 blades.ModelProvider 的可选流式接口子集。
// 具体 provider（如 contrib/openai）通常同时实现 Generate 与 NewStreaming；
// 仅实现 Generate 的 provider（含测试 mock）自动回退到一次性调用。
type streamingModelProvider interface {
	// NewStreaming 执行请求并返回一个逐块产出响应的生成器；
	// 最后一个产出值是完整累积响应（contrib/openai 由 accumulator 保证）。
	NewStreaming(context.Context, *blades.ModelRequest) blades.Generator[*blades.ModelResponse, error]
}

// generateOnce 执行单次 LLM 调用：provider 支持流式时走流式并推送 llm_delta 实时事件，
// 否则回退到一次性 Generate。
func (a *ReActAgent) generateOnce(ctx context.Context, req *blades.ModelRequest) (*blades.ModelResponse, error) {
	if sp, ok := a.llm.(streamingModelProvider); ok {
		return a.generateStreaming(ctx, req, sp)
	}
	return a.llm.Generate(ctx, req)
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

// windowMessages 把发送给 LLM 的消息裁剪到最多 max 条（滑动窗口）：
// 保留开头的 system 消息（记忆注入）与最近的对话，并在保留段的最早 user 消息
// 边界下刀，保证 assistant 的 tool_calls 与后续 tool 结果成对完整（部分 provider
// 校验不成对会报错）。被省略的条数以一条说明消息占位。max<=0 或未超限时原样返回。
func windowMessages(messages []ReactMessage, max int) []ReactMessage {
	if max <= 0 || len(messages) <= max {
		return messages
	}
	// 保留开头的 system 消息（记忆流水线注入的"近期事件"上下文）。
	keep := 0
	for keep < len(messages) && messages[keep].Role == "system" {
		keep++
	}
	// 预算：system 前缀 + 省略说明各占 1 条，其余留给最近消息。
	budget := max - keep - 1
	if budget < 1 {
		budget = 1
	}
	start := len(messages) - budget
	if start < keep {
		start = keep
	}
	// 向前移动 start 到最近的 user 边界，保证 tool 调用链完整。
	for start < len(messages) && start > keep && messages[start].Role != "user" {
		start++
	}
	omitted := start - keep
	if omitted <= 0 {
		return messages
	}
	out := make([]ReactMessage, 0, len(messages)-omitted+1)
	out = append(out, messages[:keep]...)
	out = append(out, ReactMessage{
		Role:    "user",
		Content: fmt.Sprintf("（上下文省略：此处之前还有 %d 条早期对话，已从本次请求中裁剪，关键结论见上方近期事件）", omitted),
	})
	out = append(out, messages[start:]...)
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
// 基础提示来自角色配置；末尾追加一段统一的执行纪律，用于减少常见反模式
// （无目的工具调用、未验证就声称完成、忘记 mailbox 消息语义等）。
func (a *ReActAgent) systemPrompt() string {
	// 取角色配置中的系统提示作为基础。
	base := a.role.SystemPrompt

	// 如果角色未配置系统提示，则使用默认兜底文案。
	if base == "" {
		base = "You are a helpful assistant."
	}

	// 在基础提示后追加执行纪律块，与角色提示同语言（中文），覆盖：
	// 工具使用节制、修改后验证、完成即停、mailbox 消息语义。
	return base + "\n\n" +
		"【执行纪律】\n" +
		"1. 只在必要时调用工具；先用 SearchInFiles/ListDir 定位，再按需 ReadFile；不重复读取已读过的文件。\n" +
		"2. 修改代码或文件后，用 RunCommand 验证（构建/测试/检查），没有验证证据不得声称完成。\n" +
		"3. 任务完成立即停止调用工具，输出最终答复；答复必须自包含：做了什么、结果如何、关键文件路径。\n" +
		"4. 形如 [mailbox from <agent_id>] 的消息是异步子 Agent 回传的结果摘要，阅读后整合进当前结论；若摘要表明失败，决定重试、自己接手或在答复中说明。\n"
}

// mailboxMessageToReact 把异步 mailbox 消息转换为模型可见的 ReactMessage。
// 使用 user role 并在内容前加 [mailbox] 前缀，兼容 Anthropic Messages API
// （该 API 不允许在会话中途插入 system 消息）。
func mailboxMessageToReact(m *mailbox.Message) ReactMessage {
	// 主题作为消息正文的基础部分。
	body := m.Subject

	// 如果邮件有正文，则追加到主题之后。
	if m.Body != "" {
		body += "\n" + m.Body
	}

	// 如果邮件携带结构化载荷，则序列化为 JSON 字符串并追加，方便模型读取。
	if len(m.Payload) > 0 {
		b, _ := json.Marshal(m.Payload)
		body += "\n" + string(b)
	}

	// 组合成带发送者标记的 user 消息返回。
	return ReactMessage{Role: "user", Content: fmt.Sprintf("[mailbox from %s] %s", m.From, body)}
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
