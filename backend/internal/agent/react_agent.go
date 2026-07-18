// Package agent 实现单代理的 ReAct（推理-行动）循环：
// LLM 生成 -> 工具调用 -> 工具结果 -> 重复，直到产生最终答案或达到迭代上限。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Run 针对给定的用户输入执行 ReAct 循环。
// 参数 ctx 用于取消/超时控制；input 为用户输入文本。
// 返回值 ReactResult 包含助手最终回复与完整会话历史；error 表示执行过程中的错误。
func (a *ReActAgent) Run(ctx context.Context, input string) (ReactResult, error) {
	// 委托给 RunWithHistory，从空历史开始新的会话。
	return a.RunWithHistory(ctx, input, nil)
}

// RunWithHistory 从已有历史开始执行 ReAct 循环。
// 参数 ctx 用于取消/超时控制；input 为新的用户输入；history 为已有会话历史。
// 新输入会被追加到传入的历史中，便于会话续跑并保留之前的轮次。
// 返回值 ReactResult 包含最终回复与完整历史；error 表示执行错误。
func (a *ReActAgent) RunWithHistory(ctx context.Context, input string, history []ReactMessage) (ReactResult, error) {
	// 将当前代理标识写入上下文，便于链路追踪、日志和工具调用时识别身份。
	ctx = WithAgentID(ctx, a.name)

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

		// 将 blades 返回的消息转换为内部 Assistant 消息并追加到历史中。
		assistant := AssistantMessageFromBlades(resp.Message)
		history = append(history, assistant)

		// 在判断本轮是否结束之前，先轮询邮箱并注入任何新的异步消息。
		// mailbox 由服务装配层注入共享邮箱（用于接收异步子代理摘要）；未注入时跳过。
		if a.mailbox != nil {
			// Drain 取出所有以当前代理为收件人的未读消息。
			for _, m := range a.mailbox.Drain(a.name) {
				// 将 mailbox 消息转为模型可见的 user 角色消息并加入历史。
				history = append(history, mailboxMessageToReact(m))

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
			// 分发执行单个工具调用；出错时构造包含错误信息的 ToolResult。
			result, err := a.tools.Dispatch(ctx, tc)
			if err != nil {
				result = ToolResult{Tool: tc.Name, Error: err.Error()}
			}

			// 截断工具输出后再写入历史，避免单次大输出（如整文件/长日志）
			// 在历史中无限累积，导致后续每轮请求 token 爆炸。
			if a.toolOutputMaxRunes > 0 {
				result.Output = truncateRunes(result.Output, a.toolOutputMaxRunes)
			}

			// 将工具执行结果以 tool 角色消息追加到历史中，供下一次 LLM 调用使用。
			history = append(history, ReactMessage{
				Role:    "tool",
				Content: ToolResultJSON(result),
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
		resp, err := a.llm.Generate(callCtx, req)
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
