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
	"errors"
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

// PlanStepStatus 是 plan_execute 单步的执行状态（PlanProgress 回调用）。
type PlanStepStatus string

const (
	// PlanStepInProgress 步骤开始执行。
	PlanStepInProgress PlanStepStatus = "in_progress"
	// PlanStepDone 步骤成功完成。
	PlanStepDone PlanStepStatus = "done"
	// PlanStepFailed 步骤执行出错或触限收口。
	PlanStepFailed PlanStepStatus = "failed"
)

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
	// PlanProgress 回传单步执行状态（看板任务状态迁移，TUI 计划面板实时化）；
	// stepIdx 为 1 起步步骤序号，与 PlanSink 落板顺序一致。nil 时跳过（零行为变化）。
	PlanProgress func(stepIdx int, status PlanStepStatus, summary string)
}

// ---- ReflectEngine ----

// ReflectEngine 包装 ReAct 循环：产出后对照任务验收标准自检，不达标带反馈重试。
// 适用正确性敏感任务（算法/迁移/重构/验收修复）。每轮成本 ≈ +1 次辅助 LLM 调用，
// 轮数硬上限兜底（MaxReflectionRounds）。
//
// fail-closed 语义（TODO #43）：judge LLM 缺失/报错/返回非法 JSON 时 result.Unverified=true，
// 绝不静默放行——旧 fail-open 使自检形同虚设（2026-08-13 实证全天 reflection 静默失效）。
// 校验不可用时上抛父 Agent 自决，而非把未验证结论当成功交付。
type ReflectEngine struct {
	agent  *ReActAgent
	llm    LLMComplete
	rounds int
}

// ErrJudgeUnavailable 是 judge LLM 不可用（报错/坏 JSON）的哨兵，
// 供测试断言 fail-closed 路径；对外表现为 result.Unverified=true 而非 error。
var ErrJudgeUnavailable = errors.New("judge LLM unavailable")

// rubricCheck 是 judge 对单条验收条目的判定。
type rubricCheck struct {
	Item     string `json:"item"`     // 验收条目文本
	Pass     bool   `json:"pass"`     // 该条目是否达标
	Evidence string `json:"evidence"` // 依据（引用执行结果/验证证据的具体内容）
}

// verdict 是 judge 的 rubric 分项判定结果。
type verdict struct {
	Pass     bool          `json:"pass"`     // 整体是否达标（所有条目 pass）
	Feedback string        `json:"feedback"` // 不达标问题清单（达标为空串）
	Checks   []rubricCheck `json:"checks"`   // 逐条判定
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
	if err != nil || result.LimitReached {
		return result, err
	}
	if e.llm == nil {
		// fail-closed：无 judge 即无法判定，标记未验证而非静默通过。
		result.Unverified = true
		result.VerifyNote = "judge LLM 未配置"
		return result, nil
	}
	history := result.History
	for round := 0; round < e.rounds; round++ {
		// 每轮重算证据：重试轮的文件/验证输出随 history 更新。
		files := FilesModifiedFromHistory(result.History)
		evidence := RecentVerificationOutputs(result.History, 3)
		v, err := e.reflect(ctx, input, result.Text, files, evidence)
		if err != nil {
			// fail-closed：judge 不可用（报错/坏 JSON）时未验证上抛，绝不静默 pass。
			// TODO #54 降级：judge 重试后仍不可用，但 L0 可执行证据存在
			//（node --check 等验证命令 Success）时降级 pass-with-warning，
			// 不再 unverified-FAIL 触发重派（2026-08-17 塔防事故：23 分钟产出
			// 仅因 judge 围栏 JSON 被误杀，重派再烧 13 分钟同文件重复劳动）。
			// 无客观证据时维持 fail-closed（#43"绝不静默放行"边界 = 无证据场景）。
			if HasExecutableVerification(result.History) {
				result.VerifyNote = "L0 通过，L2 judge 不可用（降级放行）"
				return result, nil
			}
			result.Unverified = true
			result.VerifyNote = err.Error()
			return result, nil
		}
		if v.Pass {
			result.VerifyNote = "L2 rubric"
			return result, nil
		}
		result, err = e.agent.RunWithHistory(ctx, reflectionRetryMessage(v.Feedback), history)
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

// reflect 对照任务验收标准，rubric 分项自检执行结果（TODO #43 改造）：
// judge 被要求从任务提取验收条目逐条判定，并对照【修改文件】与【验证证据】客观段
// 给出依据（堵幻觉 pass）。judge 报错/坏 JSON 时重试 1 次（更严"只输出 JSON"措辞），
// 重试仍败才返回 ErrJudgeUnavailable（fail-closed；TODO #54 解析健壮化）。
func (e *ReflectEngine) reflect(ctx context.Context, task, answer string, files, evidence []string) (verdict, error) {
	prompt := "【质量自检】你是独立评审（与被评审者不同模型）。对照任务验收标准，逐条检查执行结果是否达标。\n\n【任务】\n" + task +
		"\n\n【执行结果】\n" + answer +
		"\n\n【修改文件】\n" + renderListOrNone(files, 10) +
		"\n\n【验证证据】\n" + renderListOrNone(evidence, 0) +
		"\n\n【验收标准】从【任务】中提取验收条目（无显式条目则推导 2-5 条），逐条判定。" +
		"\n\n严格输出 JSON（不要其他文字）：{\"pass\": true 或 false, \"feedback\": \"不达标时的具体问题清单（达标则为空串）\", \"checks\": [{\"item\": \"验收条目\", \"pass\": true 或 false, \"evidence\": \"依据【执行结果】或【验证证据】的具体内容\"}]}"
	if v, err := e.judgeOnce(ctx, prompt); err == nil {
		return v, nil
	}
	// TODO #54：解析失败/LLM 报错重试 1 次，换更严"只输出 JSON"措辞
	//（judge 模型返回围栏/夹带文字时首答大概率可救，2026-08-17 塔防事故根因）。
	return e.judgeOnce(ctx, prompt+
		"\n\n【重试】上一次输出无法解析。只输出一个合法 JSON 对象：以 { 开头、以 } 结尾，禁止 markdown 代码围栏、禁止任何解释文字。")
}

// judgeOnce 执行一次 judge 调用并解析 verdict；报错/坏 JSON 返回 ErrJudgeUnavailable。
func (e *ReflectEngine) judgeOnce(ctx context.Context, prompt string) (verdict, error) {
	resp, err := e.llm(ctx, prompt)
	if err != nil {
		return verdict{}, fmt.Errorf("%w: %v", ErrJudgeUnavailable, err)
	}
	var v verdict
	if json.Unmarshal([]byte(extractJSON(resp)), &v) != nil {
		return verdict{}, fmt.Errorf("%w: invalid JSON response", ErrJudgeUnavailable)
	}
	v.Feedback = truncateRunes(strings.TrimSpace(v.Feedback), 2000)
	return v, nil
}

// extractJSON 从 judge 原始响应提取 JSON 对象：剥 markdown 代码围栏，
// 再提取首个花括号平衡的 {...} 块（容忍前后夹带解释文字）。无平衡块返回空串。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
		s = strings.TrimSpace(s)
	}
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		switch {
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case !inStr && c == '{':
			depth++
		case !inStr && c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// renderListOrNone 把字符串列表渲染为编号行；空列表渲染为"无"。
// maxLines>0 时截断到最近 maxLines 条。
func renderListOrNone(items []string, maxLines int) string {
	if len(items) == 0 {
		return "无"
	}
	if maxLines > 0 && len(items) > maxLines {
		items = items[len(items)-maxLines:]
	}
	var sb strings.Builder
	for i, it := range items {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, it)
	}
	return strings.TrimRight(sb.String(), "\n")
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
	progress func(stepIdx int, status PlanStepStatus, summary string)
}

// NewPlanExecuteEngine 构造 PlanExecuteEngine；maxSteps <=0 按默认 8。
func NewPlanExecuteEngine(a *ReActAgent, opts EngineOptions) *PlanExecuteEngine {
	maxSteps := opts.PlanMaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}
	return &PlanExecuteEngine{agent: a, llm: opts.LLM, maxSteps: maxSteps, planSink: opts.PlanSink, progress: opts.PlanProgress}
}

// reportStep 回传单步状态（看板迁移）；progress 未注入时静默跳过。
func (e *PlanExecuteEngine) reportStep(stepIdx int, status PlanStepStatus, summary string) {
	if e.progress != nil {
		e.progress(stepIdx, status, summary)
	}
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
			// 超步数上限截断：未执行步骤落 failed，避免计划面板残留 Waiting。
			e.failRemaining(steps, i, "超最大执行步数未执行")
			break
		}
		e.reportStep(i+1, PlanStepInProgress, "")
		result, err := e.agent.RunWithHistory(ctx, planStepMessage(st, i+1, len(steps)), history)
		if err != nil {
			e.reportStep(i+1, PlanStepFailed, err.Error())
			e.failRemaining(steps, i+1, "前序步骤失败未执行")
			return result, err
		}
		if result.LimitReached {
			e.reportStep(i+1, PlanStepFailed, "上下文 token 上限收口")
			e.failRemaining(steps, i+1, "前序步骤触限收口未执行")
			return result, nil
		}
		e.reportStep(i+1, PlanStepDone, st.Title)
		history = result.History
	}
	// 收口终答：各步骤产出已在 history 中，要求模型汇总输出最终交付答复。
	return e.agent.RunWithHistory(ctx, planExecuteFinalizeMessage, history)
}

// failRemaining 把从 from（0 起步索引）起的未执行步骤统一落 failed。
func (e *PlanExecuteEngine) failRemaining(steps []PlanStep, from int, reason string) {
	for j := from; j < len(steps); j++ {
		e.reportStep(j+1, PlanStepFailed, reason)
	}
}

// plan 用辅助 LLM 把任务拆解为步骤计划。返回 ok=false 表示应降级纯 ReAct。
func (e *PlanExecuteEngine) plan(ctx context.Context, input string) ([]PlanStep, bool) {
	if e.llm == nil {
		return nil, false
	}
	prompt := "把以下任务拆解为 2-8 个可独立执行的步骤。每步指令必须自包含" +
		"（目标、涉及文件路径、验收标准——验收标准须逐条可判 PASS/FAIL，步骤报告将逐条判定）：" +
		"执行 Agent 只看得到该步指令与前序历史，看不到整体计划。\n\n【任务】\n" +
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

// planStepReportFormat 是步骤完成报告的三段式瘦身模板（TODO #50）。
// 背景：步骤报告是 plan_execute 模式第二大输出开销——实证每步强制输出 2000+ token 的
// 交付报告（改动清单表 + 验收证据表 + 备注，整段复述改动内容），6 步累计上万 token。
// 父 Agent 只需要结论、验收结果、关键位置（文件+行号），细节可自行 ReadFile。
// 模板只约束报告形态，不删验收纪律：验收标准逐条判 PASS/FAIL 的要求保留。
const planStepReportFormat = "【步骤完成报告（必须遵守，禁止整段复述 diff 与代码原文；父 Agent 需要细节会自行 ReadFile）】\n" +
	"① 结论：通过 / 未通过（一行）\n" +
	"② 关键改动位置：函数名 + 行号清单（每处一行：文件:行号 函数/位置），不复述代码\n" +
	"③ 验收证据：步骤指令中的每条验收标准逐条判定，一行一条：验收标准 → PASS/FAIL → 证据引用（文件+行号或命令输出摘要）\n" +
	"本步骤所有验收标准未全部 PASS 时，结论必须为未通过并列出失败项。"

// planStepMessage 构造单步执行指令（带步骤序号，提示上下文位置，末尾附瘦身报告格式约束）。
func planStepMessage(st PlanStep, idx, total int) string {
	return fmt.Sprintf("【执行计划 步骤 %d/%d】%s\n\n%s\n\n%s", idx, total, st.Title, st.Instruction, planStepReportFormat)
}

// planExecuteFinalizeMessage 全部步骤完成后收口终答的指令。
const planExecuteFinalizeMessage = "所有计划步骤已执行完毕。请汇总各步骤产出，输出最终交付答复（自包含：做了什么、结果如何、关键文件路径）。"

// ---- 装配辅助 ----

// engineAssistantSystem 是引擎辅助调用（自检/规划）的系统提示。
const engineAssistantSystem = "你是执行质量评审与任务规划助手。严格只输出要求格式的 JSON，不要解释。"

// NewEngineLLM 把 ModelProvider 适配为 LLMComplete（引擎辅助调用）。
// 每次调用构造"只给系统提示 + 单条用户消息"的极简请求，返回响应文本。
// 流式优先：ark /api/coding 对 thinking 模型拒绝非流式长任务请求
// （"streaming is required for operations that may take longer than 10 minutes"），
// 2026-08-13 实证 Generate 路径全天 400、reflection 自检全部 fail-open 形同虚设。
// provider 取不到或调用失败由调用方处理（引擎 fail-open 降级）。
func NewEngineLLM(p ModelProvider) LLMComplete {
	return func(ctx context.Context, prompt string) (string, error) {
		req := &blades.ModelRequest{
			Instruction: blades.SystemMessage(engineAssistantSystem),
			Messages:    []*blades.Message{blades.UserMessage(prompt)},
		}
		if sp, ok := p.(streamingModelProvider); ok {
			return engineLLMStream(ctx, req, sp)
		}
		resp, err := p.Generate(ctx, req)
		if err != nil {
			return "", err
		}
		return bladesText(resp.Message), nil
	}
}

// engineLLMStream 消费流式响应并返回最后一次有效产出的完整文本。
func engineLLMStream(ctx context.Context, req *blades.ModelRequest, sp streamingModelProvider) (string, error) {
	var final *blades.ModelResponse
	for resp, err := range sp.NewStreaming(ctx, req) {
		if err != nil {
			return "", err
		}
		if resp != nil && resp.Message != nil {
			final = resp
		}
	}
	if final == nil {
		return "", errors.New("engine llm: empty streaming response")
	}
	return bladesText(final.Message), nil
}
