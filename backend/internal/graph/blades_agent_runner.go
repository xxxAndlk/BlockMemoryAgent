package graph

// 本文件承载 blades 工具循环的核心执行逻辑：executeWithTools（ReAct 循环）+
// executeAssistantWithTools（对外入口）+ prompt 构造 / mock 退化 / 用量记录等内部辅助。
// 从 llm_tools.go 拆出（P0-3），工具列表构造见 blades_tools_build.go，完成门控见 blades_completion.go。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/model"
	"github.com/blockmemory/agent/backend/pkg/types"
	"github.com/go-kratos/blades"
)

// maxConsecutiveFailures 同一工具连续失败多少次后强制放弃重试。
//
// 设计意图：防止 LLM 在某个工具上陷入"失败-重试-再失败"的死循环，
//
//	达到阈值后通过 SetAction(ActionLoopExit) 强制跳出 blades Agent 循环。
const maxConsecutiveFailures = 3

// emitFunc 推送无 detail 的进度事件（executeWithTools 内闭包的类型别名）。
type emitFunc func(ctx context.Context, kind, msg string)

// loopExitError 表示 blades 工具循环被智能循环检测主动终止，不是执行错误。
// 触发时 executeWithTools 会基于已收集的工具结果返回总结，而非返回 Error。
type loopExitError struct {
	reason string
}

func (e *loopExitError) Error() string { return e.reason }

// toolCallFingerprint 工具调用指纹，用于检测重复调用。
type toolCallFingerprint struct {
	name    string
	request string
}

// loopDetector 在 blades Agent 迭代过程中检测死循环/空转，并在适当时机请求优雅退出。
//
// 检测策略：
//   - 重复调用：最近 windowSize 次工具调用中，同一 (name, request) 出现超过 maxRepeat 次。
//   - 连续空转：连续 maxEmptyStreak 轮 Assistant 消息只有工具调用请求、没有任何文本输出。
//   - 硬上限兜底：当观察到的工具调用次数达到 maxRounds 时主动退出（与 blades.WithMaxIterations 对齐）。
type loopDetector struct {
	maxRounds      int
	windowSize     int
	maxRepeat      int
	maxEmptyStreak int

	rounds      int
	toolHistory []toolCallFingerprint
	emptyStreak int
}

// newLoopDetector 创建循环检测器。maxRounds 通常等于传给 blades.WithMaxIterations 的值。
func newLoopDetector(maxRounds int) *loopDetector {
	if maxRounds <= 0 {
		maxRounds = 12
	}
	return &loopDetector{
		maxRounds:      maxRounds,
		windowSize:     20,
		maxRepeat:      1, // 同一工具+参数在最近 20 次中出现 2 次即判定循环（塔防事故中 game_td.js 被读 3 次才触发，过宽）
		maxEmptyStreak: 4, // 连续 4 轮只有工具调用无文本输出即判定空转
		toolHistory:    make([]toolCallFingerprint, 0, 20),
	}
}

// observe 观察一轮 blades 消息，返回是否需要主动退出及原因。
// 对 RoleAssistant 消息中的 ToolPart 计数；对空文本的工具调用消息累计空转次数。
func (d *loopDetector) observe(m *blades.Message) (stop bool, reason string) {
	if m == nil {
		return false, ""
	}

	isAssistant := m.Role == blades.RoleAssistant
	hasToolCall := false
	hasText := m.Text() != ""

	for _, part := range m.Parts {
		tp, ok := part.(blades.ToolPart)
		if !ok {
			continue
		}
		hasToolCall = true
		d.rounds++

		// 硬上限兜底：达到最大轮数时退出。
		if d.rounds >= d.maxRounds {
			return true, fmt.Sprintf("达到最大工具调用轮数 %d", d.maxRounds)
		}

		fp := toolCallFingerprint{name: tp.Name, request: tp.Request}
		d.toolHistory = append(d.toolHistory, fp)
		if len(d.toolHistory) > d.windowSize {
			d.toolHistory = d.toolHistory[len(d.toolHistory)-d.windowSize:]
		}

		// 重复调用检测。
		repeats := 0
		for _, old := range d.toolHistory {
			if old == fp {
				repeats++
			}
		}
		if repeats > d.maxRepeat {
			return true, fmt.Sprintf("工具 %s 以相同参数重复调用 %d 次，判定为循环", tp.Name, repeats)
		}
	}

	// 空转检测：Assistant 只有 tool call 且没有文字说明时，认为无进展。
	if isAssistant && hasToolCall && !hasText {
		d.emptyStreak++
		if d.emptyStreak >= d.maxEmptyStreak {
			return true, fmt.Sprintf("连续 %d 轮只有工具调用无文字输出，判定为空转", d.emptyStreak)
		}
	} else {
		d.emptyStreak = 0
	}

	return false, ""
}

// executeWithTools 使用 blades.Agent + 原生 function-calling 执行任务。
//
// LLM 通过 function calling 决定调用哪些工具，blades.Agent 内部跑 ReAct 循环
// （model.Generate → tool.Handle → 回灌 → 直到无 tool call 或达 maxIterations）。
// provider 为 nil 时（mock 路径，无 API Key）退化为单次 llm.Generate。
//
// 参数：见各字段；llmTracker 非 nil 时记录 token 用量（P0-4）。
// 返回：结构化 AgentResult（SummaryForUser/MemoryForMeta）与工具结果列表。
func executeWithTools(
	ctx context.Context,
	provider blades.ModelProvider,
	llm model.LLMClient,
	executor *ToolExecutor,
	roleDef *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
	skillBrief string,
	progress ProgressCallback,
	agentName string,
	maxIters int,
	llmTracker llmCallTracker,
) (*types.AgentResult, []*ToolResult) {
	var allResults []*ToolResult
	sessionID := sessionIDFromState(state)
	emit, emitDetail := newEmitters(progress, sessionID, agentName)
	emit(ctx, "think", "助手开始执行任务: "+task)

	systemPrompt, userMsg := buildAssistantPrompts(roleDef, skillBrief, state, task)

	// mock 路径：无 blades provider，退化为单次 LLM 调用
	if provider == nil {
		res, _ := executeMockAssistant(ctx, llm, systemPrompt, userMsg, agentName, llmTracker, emit, emitDetail)
		return res, nil
	}

	// 构造内置工具集，结果写回 allResults
	tools := buildBladesTools(executor, progress, sessionID, agentName, &allResults)
	maxItersResolved := maxIters
	if maxItersResolved <= 0 {
		maxItersResolved = 50
	}
	agent, err := blades.NewAgent(
		agentName,
		blades.WithModel(provider),
		blades.WithInstruction(systemPrompt),
		blades.WithTools(tools...),
		blades.WithMaxIterations(maxItersResolved),
	)
	if err != nil {
		emit(ctx, "error", fmt.Sprintf("创建 blades agent 失败: %v", err))
		return &types.AgentResult{
			SummaryForUser: "",
			MemoryForMeta:  fmt.Sprintf("创建 blades agent 失败: %v", err),
			Error:          err.Error(),
		}, nil
	}

	emit(ctx, "llm", fmt.Sprintf("启动 blades agent 执行（最多 %d 轮 function calling）", maxItersResolved))
	start := time.Now()
	invocation := &blades.Invocation{
		ID:      blades.NewInvocationID(),
		Session: blades.NewSession(),
		Message: blades.UserMessage(userMsg),
	}
	// 手动迭代 agent.Run：每轮 yield 一个 *Message，借此 hook 每轮 LLM 文本输出推给前端思考链
	lastMessage, totalUsage, loopErr := runBladesAgentLoop(ctx, agent, invocation, agentName, maxItersResolved, emit)
	dur := time.Since(start)

	finalText := ""
	if lastMessage != nil {
		finalText = lastMessage.Text()
	}

	var inTok, outTok int
	if loopErr != nil {
		var loopExit *loopExitError
		if errors.As(loopErr, &loopExit) {
			// 智能循环检测触发：不是执行错误，基于已收集结果返回总结。
			resultText := finalText
			if resultText == "" && len(allResults) > 0 {
				resultText = summarizeToolResults(allResults)
			}
			if resultText == "" {
				resultText = fmt.Sprintf("工具执行已收敛（%s），但未产生最终文字输出。", loopExit.reason)
			}
			resultText = fmt.Sprintf("%s [循环保护: %s, 已完成%d步工具操作]", resultText, loopExit.reason, len(allResults))
			inTok, outTok = recordBladesCall(llmTracker, ctx, dur, nil, agentName, systemPrompt, userMsg, resultText,
				int(totalUsage.InputTokens), int(totalUsage.OutputTokens))
			emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, dur.Round(time.Millisecond)), "")
			emit(ctx, "think", fmt.Sprintf("基于已收集结果返回（%s）", loopExit.reason))
			return &types.AgentResult{
				SummaryForUser: resultText,
				MemoryForMeta:  resultText,
			}, allResults
		}

		// P0-4：循环错误也记录一次（带 err），便于失败可见
		inTok, outTok = recordBladesCall(llmTracker, ctx, dur, loopErr, agentName, systemPrompt, userMsg, "",
			int(totalUsage.InputTokens), int(totalUsage.OutputTokens))
		emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, dur.Round(time.Millisecond)), "")
		emit(ctx, "error", fmt.Sprintf("blades agent 执行失败: %v", loopErr))
		resultText := ""
		if len(allResults) > 0 {
			resultText = fmt.Sprintf("agent 执行失败(已完成%d步工具操作): %v", len(allResults), loopErr)
		}
		return &types.AgentResult{
			SummaryForUser: resultText,
			MemoryForMeta:  fmt.Sprintf("blades agent 执行失败: %v", loopErr),
			Error:          loopErr.Error(),
		}, allResults
	}

	// P0-4：成功完成时记录一次 LLM 调用（含累计 token 用量）
	inTok, outTok = recordBladesCall(llmTracker, ctx, dur, nil, agentName, systemPrompt, userMsg, finalText,
		int(totalUsage.InputTokens), int(totalUsage.OutputTokens))
	emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, dur.Round(time.Millisecond)), "")

	// 完成门控：LLM 声称已写文件但无成功 WriteFile 记录 → 返回 Error 触发上层重试
	if !hasWriteFileResult(allResults) && outputClaimsWriteFile(finalText) {
		emit(ctx, "error", "输出声称已写文件但未调用 WriteFile 落盘")
		return &types.AgentResult{
			SummaryForUser: finalText,
			MemoryForMeta:  "输出声称已写文件但未调用 WriteFile 落盘",
			Error:          "missing WriteFile result",
		}, allResults
	}
	return &types.AgentResult{
		SummaryForUser: finalText,
		MemoryForMeta:  finalText,
	}, allResults
}

// newEmitters 构造 emit / emitDetail 两个进度事件推送闭包。
func newEmitters(progress ProgressCallback, sessionID, agentName string) (emitFunc, func(ctx context.Context, kind, msg, detail string)) {
	emit := func(ctx context.Context, kind, msg string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg})
		}
	}
	emitDetail := func(ctx context.Context, kind, msg, detail string) {
		if progress != nil {
			progress(ctx, ProgressEvent{SessionID: sessionID, Kind: kind, Agent: agentName, Message: msg, Detail: detail})
		}
	}
	return emit, emitDetail
}

// buildAssistantPrompts 拼装 system prompt（角色 + 工具列表 + 环境 + 硬性规则）与用户消息。
func buildAssistantPrompts(roleDef *types.RoleDefinition, skillBrief string, state *types.ThreeLayerState, task string) (systemPrompt, userMsg string) {
	skillSection := mergeToolList(skillBrief, "")
	envSection := fmtEnvSection()
	systemPrompt = fmt.Sprintf(`%s

你可以使用以下工具（通过 function calling 调用）完成任务。

%s

%s

【硬性规则】
1. 任务与工具必须匹配，禁止"为用工具而用工具"：
   - 信息查询/搜索/新闻/行情/资讯类目标 → 优先调用 HTTPGet 抓取公开 URL（如新闻站、API），
     禁止"写一个 Python 脚本去搜索/抓取"再 RunCommand 运行——这是反模式。
   - 只有目标明确要求"写代码/生成文件/运行程序"时，才用 WriteFile / RunCommand。
2. 凡任务涉及"创建/写入/生成/实现/编写"文件或代码，必须调用 WriteFile 工具真正落盘，
   禁止只用文字描述代码内容当作完成。代码必须完整、可直接运行，禁止用 pass/占位符/省略号代替实际逻辑。
3. 凡任务涉及"运行/执行/启动"程序，必须调用 RunCommand 工具实际执行，禁止只描述如何运行。
   但【未明确要求运行时禁止直接启动程序】：如果用户只是让"写/实现/开发"某个程序，没有明确说"运行它""启动它""执行它"，
   你只能用 RunCommand 做编译/语法检查（如 'python -m py_compile xxx.py'），禁止直接运行会打开 GUI、进入主循环或长时间占用终端的程序。
4. 工具未成功执行前，不得宣称任务完成。
5. 工具失败时，输出会包含 stderr/stdout。请阅读失败原因后再决定下一步，
   不要盲目重试同一命令的不同变种。连续两次失败后必须换一种完全不同的方法
   （例如换工具、换路径、放弃当前思路），而不是继续试错。
6. 只有当所有要求的文件已落盘、命令已执行，且无需再调用工具时，才输出最终文字总结。
7. 循环收敛：若连续多次工具调用未获得新信息、同一命令重复失败、或已无明显进展，
   必须立即停止继续试错，基于已掌握的信息给出当前结论，而不是无限循环。
8. 文件读取预算：
   - 先用 SearchInFiles/ListDir 定位，再用 ReadFile 精确读取。
   - 同一文件禁止重复读取；需要确认时依靠已返回内容。
   - 单次 ReadFile 不超过 300 行；每个任务累计不超过 5 次。
   - 禁止写临时脚本再次打印已读过的文件内容。
   - 连续两次工具调用无新信息，或累计 input tokens 超过 80K，立即停止探索并返回结论。`,
		roleDef.SystemPrompt, skillSection, envSection)

	contextInfo := ""
	if state != nil && state.CurrentDomain != "" {
		contextInfo = fmt.Sprintf("\n当前领域: %s\n领域目标: %s", state.CurrentDomain, state.DomainGoal)
	}
	userMsg = fmt.Sprintf("任务: %s%s\n\n请分析任务并执行。如果需要查看文件或执行命令，请调用工具。", task, contextInfo)
	return systemPrompt, userMsg
}

// executeMockAssistant mock 路径：无 blades provider 时单次 llm.Generate，并记录用量（P0-4）。
func executeMockAssistant(
	ctx context.Context, llm model.LLMClient, systemPrompt, userMsg, agentName string,
	llmTracker llmCallTracker, emit emitFunc,
	emitDetail func(ctx context.Context, kind, msg, detail string),
) (*types.AgentResult, []*ToolResult) {
	emit(ctx, "llm", "无 blades provider（mock 模式），单次 LLM 调用")
	mockPrompt := systemPrompt + "\n\n" + userMsg
	mockStart := time.Now()
	// 优先用 UsageAware 拿真实用量，否则回退估算
	var resp string
	var usage blades.TokenUsage
	var err error
	if bc, ok := llm.(*model.BladesClient); ok {
		resp, usage, err = bc.GenerateWithUsage(ctx, mockPrompt)
	} else {
		resp, err = llm.Generate(ctx, mockPrompt)
	}
	mockDur := time.Since(mockStart)
	var inTok, outTok int
	if err != nil {
		inTok, outTok = recordBladesCall(llmTracker, ctx, mockDur, err, agentName, systemPrompt, userMsg, "", 0, 0)
		emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, mockDur.Round(time.Millisecond)), "")
		emit(ctx, "error", fmt.Sprintf("LLM 调用失败: %v", err))
		return &types.AgentResult{
			SummaryForUser: "",
			MemoryForMeta:  fmt.Sprintf("LLM 调用失败: %v", err),
			Error:          err.Error(),
		}, nil
	}
	inTok, outTok = recordBladesCall(llmTracker, ctx, mockDur, nil, agentName, systemPrompt, userMsg, resp,
		int(usage.InputTokens), int(usage.OutputTokens))
	emitDetail(ctx, "token_usage", fmt.Sprintf("[%s] Token 消耗: in=%d out=%d dur=%v", agentName, inTok, outTok, mockDur.Round(time.Millisecond)), "")
	return &types.AgentResult{
		SummaryForUser: resp,
		MemoryForMeta:  resp,
	}, nil
}

// runBladesAgentLoop 迭代 blades agent.Run 生成器，累加每轮 token 用量（P0-4），
// 并在循环检测器触发时主动退出，避免无限循环或成本失控。
// 返回最后一个 message、累计用量、循环错误（nil 表示正常结束）。
// 注意：假设 provider 非流式返回每轮独立用量；若未来切流式需复查累加逻辑。
//
// 上下文爆炸保护：分两级阈值。
//   - 软阈值 50K：单轮 input_tokens 超 50K 时 emit 警告，提示 LLM 收敛。
//   - 硬阈值 80K：单轮 input_tokens 超 80K 时主动退出，返回已收集结果。
//
// 参见塔防 demo 事故 v2：MetaAgent[临时助手] 累积 850K input tokens（152s 调用），
// 因反复读 game.js/game_core.js + 工具结果全量回灌 LLM。blades agent 内部无上下文裁剪，
// 需在外层 loop 拦截。软阈值给 LLM 一次"收敛机会"，硬阈值兜底防爆炸。
func runBladesAgentLoop(ctx context.Context, agent blades.Agent, invocation *blades.Invocation, agentName string, maxIters int, emit emitFunc) (*blades.Message, blades.TokenUsage, error) {
	var lastMessage *blades.Message
	var totalUsage blades.TokenUsage
	detector := newLoopDetector(maxIters)
	const maxInputTokensBudget = 80000 // 单轮输入 token 硬上限，超此视为上下文爆炸
	const softWarnThreshold = 50000    // 软阈值：超此 emit 警告但不退出
	softWarned := false                // 软阈值只警告一次，避免刷屏
	for m, err := range agent.Run(ctx, invocation) {
		if err != nil {
			// 单 assistant wall-clock 超时：返回已收集结果而非裸 error，避免上层当作失败丢弃半成品
			if errors.Is(err, context.DeadlineExceeded) {
				reason := fmt.Sprintf("单助手 wall-clock 超时（3min），基于已收集结果返回（已完成 %d 步工具操作）", detector.rounds)
				emit(ctx, "wait", reason)
				return lastMessage, totalUsage, &loopExitError{reason: reason}
			}
			return lastMessage, totalUsage, err
		}
		if m == nil {
			continue
		}
		lastMessage = m
		totalUsage.InputTokens += m.TokenUsage.InputTokens
		totalUsage.OutputTokens += m.TokenUsage.OutputTokens
		totalUsage.TotalTokens += m.TokenUsage.TotalTokens
		// 软阈值警告：单轮 input_tokens 超 50K 但未达 80K，提示 LLM 收敛
		if !softWarned && m.TokenUsage.InputTokens >= softWarnThreshold && m.TokenUsage.InputTokens < maxInputTokensBudget {
			emit(ctx, "wait", fmt.Sprintf("上下文接近爆炸（单轮 input_tokens=%d），请减少 ReadFile 次数并基于已读内容推进任务", m.TokenUsage.InputTokens))
			softWarned = true
		}
		// 上下文爆炸检测：单轮 input_tokens 超预算则退出
		if m.TokenUsage.InputTokens > maxInputTokensBudget {
			reason := fmt.Sprintf("上下文爆炸保护：单轮 input_tokens=%d 超预算 %d，可能因工具结果累积过多。建议减少 ReadFile 次数或缩短工具输出",
				m.TokenUsage.InputTokens, maxInputTokensBudget)
			emit(ctx, "error", reason)
			return lastMessage, totalUsage, &loopExitError{reason: reason}
		}
		// 暴露每轮 LLM 文本输出；工具调用轮次 (RoleTool) Text 通常为空，不会重复 emit
		if txt := m.Text(); txt != "" {
			emit(ctx, "llm_result", "LLM 输出: "+truncateStr(txt, 400))
		}
		// 智能循环检测：重复调用 / 空转 / 硬上限兜底。
		if stop, reason := detector.observe(m); stop {
			emit(ctx, "wait", fmt.Sprintf("触发循环保护：%s，基于已收集结果返回", reason))
			return lastMessage, totalUsage, &loopExitError{reason: reason}
		}
	}
	return lastMessage, totalUsage, nil
}

// recordBladesCall 记录一次 blades 路径 LLM 调用；real 用量为 0 时回退 EstimateTokens（P0-4）。
// 返回最终记录/展示用的 input/output token 数（tracker 为 nil 时回退估算值）。
// 兜底：input/output 均保证至少为 1，避免 token_usage 事件出现全 0 导致面板统计为空。
func recordBladesCall(tracker llmCallTracker, ctx context.Context, dur time.Duration, err error,
	agentName, systemPrompt, userMsg, response string, inReal, outReal int) (inTok, outTok int) {
	prompt := systemPrompt + "\n\n" + userMsg
	inTok = inReal
	if inTok == 0 {
		inTok = model.EstimateTokens(prompt)
	}
	if inTok == 0 && prompt != "" {
		inTok = 1
	}
	outTok = outReal
	if outTok == 0 {
		// response 为空时，用错误信息或占位符兜底估算，确保 output token 不为 0
		fallback := response
		if fallback == "" && err != nil {
			fallback = err.Error()
		}
		if fallback == "" {
			fallback = "[no response]"
		}
		outTok = model.EstimateTokens(fallback)
	}
	if outTok == 0 {
		outTok = 1
	}
	if tracker != nil {
		tracker.RecordCall(ctx, dur, err, agentName, model.SummarizePrompt(prompt, 500), prompt, response, inTok, outTok, false)
	}
	return
}

// executeAssistantWithTools 助手使用 blades.Agent + 工具执行任务的对外入口。
// 职责：从 ModelFactory 取模型与 blades provider，设置 5 分钟超时，转调 executeWithTools。
// 返回：结构化 AgentResult 与工具结果列表；modelFactory 为 nil 或取模型失败时返回空。
func executeAssistantWithTools(
	ctx context.Context,
	modelFactory *model.ModelFactory,
	executor *ToolExecutor,
	roleDef *types.RoleDefinition,
	task string,
	state *types.ThreeLayerState,
	skillBrief string,
	progress ProgressCallback,
	agentName string,
	maxIters int,
	llmTracker llmCallTracker,
) (*types.AgentResult, []*ToolResult) {
	if modelFactory == nil {
		return nil, nil
	}
	llm, err := modelFactory.GetModel(ctx, roleDef.ID)
	if err != nil {
		return nil, nil // 取模型失败，静默返回
	}
	// 取 blades provider；mock 路径（无 API Key）返回错误，传入 nil 触发退化
	provider, _ := modelFactory.GetBladesProvider(ctx, roleDef.ID)
	// 带超时执行：单个 assistant 最多 3 分钟（每轮 LLM 可达 40s+，3min 够 4-5 轮 ReAct）
	// 塔防事故：assistant_17 在 4m25s 内烧 1.5M input tokens，5min 上限太松
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	return executeWithTools(ctx, provider, llm, executor, roleDef, task, state, skillBrief, progress, agentName, maxIters, llmTracker)
}

// summarizeToolResults 把已收集的工具结果拼成一段简短总结，用于循环保护触发时
// 向用户返回已有进展，而不是空白。
func summarizeToolResults(results []*ToolResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("已执行工具操作摘要：")
	for _, r := range results {
		sb.WriteString("\n- ")
		sb.WriteString(r.Tool)
		if r.Path != "" {
			sb.WriteString(" ")
			sb.WriteString(r.Path)
		}
		if r.Success {
			out := truncateStr(r.Output, 120)
			if out != "" {
				sb.WriteString(": ")
				sb.WriteString(out)
			} else {
				sb.WriteString(": 成功")
			}
		} else if r.Error != "" {
			sb.WriteString(" 失败: ")
			sb.WriteString(truncateStr(r.Error, 120))
		}
	}
	return sb.String()
}
