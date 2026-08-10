package agent

// engine.go 实现派发执行模式（TODO #29）：Engine 接口 + ReflectEngine + PlanExecuteEngine。
//
// 背景：所有被派发 Agent 不分任务形态跑同一个裸 ReAct 循环（ReActAgent.RunWithHistory），
// 简单任务白烧反思开销、复杂任务无规划直接上手翻车。三引擎共享同一 Agent 骨架
// （LLM 调用/工具派发/历史/记忆全部复用 ReActAgent），差异只在"循环策略"一层，
// 省略 mode = 零行为变化（默认 ReAct 引擎即既有主循环）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-kratos/blades"
)

// 派发执行模式（call_sub_agent 的 mode 参数取值）。
const (
	// ModeReact 默认模式：裸 ReAct 循环（零行为变化）。
	ModeReact = "react"
	// ModeReflection 正确性敏感任务：产出后对照验收标准自检，不达标带反馈重试。
	ModeReflection = "reflection"
	// ModePlanExecute 多步骤长任务：先出步骤计划（落 board，TUI 可见）再逐步执行。
	ModePlanExecute = "plan_execute"
)

// Engine 是派发执行模式的可插拔执行入口（TODO #29）。
// ReActAgent.Run 即默认 ReAct 引擎；ReflectEngine / PlanExecuteEngine 包装它，
// 挂载点与语义（ReactResult/error/LimitReached）与裸 ReAct 完全一致，上层零感知。
type Engine interface {
	// Run 从空历史执行任务。
	Run(ctx context.Context, input string) (ReactResult, error)
}

// LLMComplete 是一次非流式文本补全调用（自检/规划等辅助决策用），
// 由装配方（Dispatcher）经 NewEngineLLM 从 ModelProvider 适配；nil 时引擎降级纯 ReAct。
type LLMComplete func(ctx context.Context, prompt string) (string, error)

// PlanStep 是 plan_execute 引擎的一步执行指令。
// Instruction 必须自包含（目标、文件路径、验收标准）：执行 Agent 只看得到
// 该步指令 + 前序步骤历史，看不到整体计划。
type PlanStep struct {
	Title       string `json:"title"`       // 步骤标题
	Instruction string `json:"instruction"` // 自包含执行指令
}

// EngineOptions 是包装引擎的运行时参数。
type EngineOptions struct {
	// LLM 辅助补全（reflection 自检 / plan_execute 规划）；nil 时降级纯 ReAct。
	LLM LLMComplete
	// MaxReflectionRounds reflection 自检不达标后的重试轮数上限；<=0 按默认 2。
	MaxReflectionRounds int
	// PlanMaxSteps plan_execute 最大执行步数；<=0 按默认 8。超限强制收口终答。
	PlanMaxSteps int
	// PlanSink 把执行计划写入看板（TUI 可见）；nil 时跳过（零行为变化）。
	PlanSink func(goal string, steps []PlanStep) error
}

// ---- ReflectEngine ----

// ReflectEngine 包装 ReAct 循环：产出后对照任务验收标准自检，不达标带反馈重试。
// 适用正确性敏感任务（算法/迁移/重构/验收修复）。每轮成本 ≈ +1 次辅助 LLM 调用，
// 轮数硬上限兜底（MaxReflectionRounds）。
//
// fail-open 语义：辅助 LLM 缺失/报错/返回非法 JSON 时按"通过"处理，不阻塞交付——
// 自检是增值层，宁可放行也不让 Agent 卡在无法判定的循环里。
type ReflectEngine struct {
	agent  *ReActAgent
	llm    LLMComplete
	rounds int
}

// NewReflectEngine 构造 ReflectEngine；rounds <=0 按默认 2。
func NewReflectEngine(a *ReActAgent, opts EngineOptions) *ReflectEngine {
	rounds := opts.MaxReflectionRounds
	if rounds <= 0 {
		rounds = 2
	}
	return &ReflectEngine{agent: a, llm: opts.LLM, rounds: rounds}
}

// Run 执行 ReAct 循环并自检重试。
func (e *ReflectEngine) Run(ctx context.Context, input string) (ReactResult, error) {
	result, err := e.agent.Run(ctx, input)
	if err != nil || result.LimitReached || e.llm == nil {
		return result, err
	}
	history := result.History
	for round := 0; round < e.rounds; round++ {
		pass, feedback := e.reflect(ctx, input, result.Text)
		if pass {
			return result, nil
		}
		result, err = e.agent.RunWithHistory(ctx, reflectionRetryMessage(feedback), history)
		if err != nil {
			return result, err
		}
		if result.LimitReached {
			return result, nil
		}
		history = result.History
	}
	return result, nil
}

// reflect 对照任务（含验收标准）自检执行结果。
// 返回 pass=true 表示达标（或无法判定，fail-open）；pass=false 表示不达标，附反馈。
func (e *ReflectEngine) reflect(ctx context.Context, task, answer string) (bool, string) {
	prompt := "【质量自检】对照任务与验收标准，检查执行结果是否达标。\n\n【任务】\n" + task +
		"\n\n【执行结果】\n" + answer +
		"\n\n严格输出 JSON（不要其他文字）：{\"pass\": true 或 false, \"feedback\": \"不达标时的具体问题清单（达标则为空串）\"}"
	resp, err := e.llm(ctx, prompt)
	if err != nil {
		return true, "" // fail-open：自检 LLM 失败不阻塞交付
	}
	var v struct {
		Pass     bool   `json:"pass"`
		Feedback string `json:"feedback"`
	}
	if json.Unmarshal([]byte(resp), &v) != nil {
		return true, "" // fail-open：非法 JSON 按通过处理
	}
	return v.Pass, truncateRunes(strings.TrimSpace(v.Feedback), 2000)
}

// reflectionRetryMessage 构造自检未通过后的带反馈重试指令。
func reflectionRetryMessage(feedback string) string {
	return "【自检未通过】对照任务验收标准，你的上一轮产出未达标。请针对性修正后重新产出最终答复。\n自检反馈：\n" + feedback
}

// ---- PlanExecuteEngine ----

// PlanExecuteEngine 包装 ReAct 循环：先出步骤计划（落 board，TUI 可见），
// 再逐步执行（每步带前序历史续跑），全部步骤完成后终答汇总。
// 适用多步骤长任务（多文件/多阶段/复杂装配）。
//
// 降级语义：规划 LLM 缺失/报错/返回非法 JSON/空步骤时整体降级为裸 ReAct（零行为变化）；
// 计划落板失败仅日志，不阻塞执行。
//
// 已知语义弱化：token 预算按步重置（每步 RunWithHistory 独立计 budget），
// 总成本仍受 sub_agent_timeout 墙钟与 LLM 轮数上限兜底；接受该偏差换取实现简单。
type PlanExecuteEngine struct {
	agent    *ReActAgent
	llm      LLMComplete
	maxSteps int
	planSink func(goal string, steps []PlanStep) error
}

// NewPlanExecuteEngine 构造 PlanExecuteEngine；maxSteps <=0 按默认 8。
func NewPlanExecuteEngine(a *ReActAgent, opts EngineOptions) *PlanExecuteEngine {
	maxSteps := opts.PlanMaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}
	return &PlanExecuteEngine{agent: a, llm: opts.LLM, maxSteps: maxSteps, planSink: opts.PlanSink}
}

// Run 规划并逐步执行任务。
func (e *PlanExecuteEngine) Run(ctx context.Context, input string) (ReactResult, error) {
	steps, ok := e.plan(ctx, input)
	if !ok {
		return e.agent.Run(ctx, input) // 规划失败降级纯 ReAct
	}
	if e.planSink != nil {
		_ = e.planSink(input, steps)
	}
	var history []ReactMessage
	for i, st := range steps {
		if i >= e.maxSteps {
			break
		}
		result, err := e.agent.RunWithHistory(ctx, planStepMessage(st, i+1, len(steps)), history)
		if err != nil {
			return result, err
		}
		if result.LimitReached {
			return result, nil
		}
		history = result.History
	}
	// 收口终答：各步骤产出已在 history 中，要求模型汇总输出最终交付答复。
	return e.agent.RunWithHistory(ctx, planExecuteFinalizeMessage, history)
}

// plan 用辅助 LLM 把任务拆解为步骤计划。返回 ok=false 表示应降级纯 ReAct。
func (e *PlanExecuteEngine) plan(ctx context.Context, input string) ([]PlanStep, bool) {
	if e.llm == nil {
		return nil, false
	}
	prompt := "把以下任务拆解为 2-8 个可独立执行的步骤。每步指令必须自包含" +
		"（目标、涉及文件路径、验收标准）：执行 Agent 只看得到该步指令与前序历史，看不到整体计划。\n\n【任务】\n" +
		input +
		"\n\n严格输出 JSON 数组（不要其他文字）：[{\"title\": \"步骤名\", \"instruction\": \"自包含执行指令\"}]"
	resp, err := e.llm(ctx, prompt)
	if err != nil {
		return nil, false
	}
	var steps []PlanStep
	if json.Unmarshal([]byte(resp), &steps) != nil || len(steps) == 0 {
		return nil, false
	}
	for _, s := range steps {
		if strings.TrimSpace(s.Instruction) == "" {
			return nil, false
		}
	}
	return steps, true
}

// planStepMessage 构造单步执行指令（带步骤序号，提示上下文位置）。
func planStepMessage(st PlanStep, idx, total int) string {
	return fmt.Sprintf("【执行计划 步骤 %d/%d】%s\n\n%s", idx, total, st.Title, st.Instruction)
}

// planExecuteFinalizeMessage 全部步骤完成后收口终答的指令。
const planExecuteFinalizeMessage = "所有计划步骤已执行完毕。请汇总各步骤产出，输出最终交付答复（自包含：做了什么、结果如何、关键文件路径）。"

// ---- 装配辅助 ----

// engineAssistantSystem 是引擎辅助调用（自检/规划）的系统提示。
const engineAssistantSystem = "你是执行质量评审与任务规划助手。严格只输出要求格式的 JSON，不要解释。"

// NewEngineLLM 把 ModelProvider 适配为 LLMComplete（引擎辅助调用）。
// 每次调用构造"只给系统提示 + 单条用户消息"的极简请求，返回响应文本。
// provider 取不到或调用失败由调用方处理（引擎 fail-open 降级）。
func NewEngineLLM(p ModelProvider) LLMComplete {
	return func(ctx context.Context, prompt string) (string, error) {
		resp, err := p.Generate(ctx, &blades.ModelRequest{
			Instruction: blades.SystemMessage(engineAssistantSystem),
			Messages:    []*blades.Message{blades.UserMessage(prompt)},
		})
		if err != nil {
			return "", err
		}
		return bladesText(resp.Message), nil
	}
}
