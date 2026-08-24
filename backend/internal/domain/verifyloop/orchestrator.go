// Package verifyloop 实现多 Agent 协作验证闭环的原生状态机编排器引擎。
//
// 【现状：未接线保留】2026-08-08 A/B 实证（test/benchmark/runs/multi-on vs multi-off）：
// 自动验证闭环开启后通过率 16/16 -> 5/16、token 翻倍（verifier 累计 input 计费烧穿预算、
// 零件级通过≠整品可用、失败摘要干扰父 Agent 决策）。dispatcher/bootstrap 接线与
// self_test 配置已整体移除，活跃机制改为分层自检（叶子自检 / 领域收尾验收 / meta 纸面交付对照+返工，
// 见 config/roles.yaml）。本包作为业务验收测试工作流的原型保留，复活方案见
// doc/扩展设计_Agent工作流平台.md §12。
//
// 设计意图：把"产出 -> 验证 -> 不通过打回修正 -> 通过后上级统一测试"这条业务流程
// 从主 Agent 提示词驱动转为编排器原生驱动，避免依赖 LLM 自觉。
//
// 抽象分层（支持多场景复用，不只代码自测）：
//   - Engine（Orchestrator）：驱动"自测 -> 修正 -> 上级统一测试"状态机循环，
//     不绑定具体验证方式，只消费 Verifier/Fixer/Reporter 三个接口。
//   - Verifier：执行一次验证并返回结构化结论 Verdict。默认 AgentVerifier 派发测试 Agent；
//     后续可替换为 ComputerUse（模拟点击/截图对比）、CLI（跑命令断言）、
//     MCP（调用 MCP 服务器测试工具）等实现，按 Verifier 接口契约填入。
//   - Fixer：基于验证失败反馈修正产出。默认 AgentFixer 派发编码 Agent；
//     后续可替换为 patch 工具、LLM 局部重写等。
//   - Reporter：把最终结果通知上游。默认 MailboxReporter 投递父 Agent 邮箱；
//     后续可替换为 webhook、SSE、DAG 任务回调等。
//
// 状态机：
//  1. 入口：产出方已完成初始任务，产出 initialResult。
//  2. 自测：Verifier.SelfTest(initialResult) -> Verdict。
//  3. 自测通过 -> 进入上级统一测试 Verifier.UnifiedTest。
//  4. 自测失败 -> Fixer.Fix -> 新产出 -> 回到步骤 2。
//  5. 上级统一测试通过 -> 整体通过，Reporter 上报。
//  6. 上级统一测试失败 -> 打回修正 -> 回到步骤 2。
//  7. 往返次数超过 maxRounds -> 未通过，Reporter 上报失败原因。
//
// 引擎通过 Runner 接口同步执行子 Agent（默认 DispatcherRunner），不经过 call_sub_agent 工具，
// 因此不会触发 onSubAgentDone 钩子，避免编排器内部的修正轮递归触发自测。
package verifyloop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// Runner 抽象编排器执行子 Agent 的能力，由 *subagent.Dispatcher 实现（ExecuteChild 方法）。
// 用窄接口避免 verifyloop 反向依赖 subagent 包。
// 后续可替换为 MockRunner（测试）、MCPRunner（调用 MCP 服务器）等。
type Runner interface {
	// ExecuteChild 同步执行一个子 Agent 并返回其最终答复文本。
	ExecuteChild(ctx context.Context, parentID, roleID, task string) (string, error)
}

// Verdict 是验证器返回的结构化结论，替代裸字符串的 [VERIFY:PASS/FAIL] 标记解析。
// 验证器实现负责把自身媒介（Agent 答复/CLI 退出码/截图对比/MCP 响应）归一为 Verdict。
type Verdict struct {
	Passed bool   // 是否通过
	Reason string // 未通过时的失败原因（通过时可留空）
	Detail string // 验证过程的完整报告（日志/输出/截图描述等），供 Fixer 与 Reporter 引用
}

// Verifier 执行验证动作。接口分 SelfTest（单元/功能级）与 UnifiedTest（模块/集成级）两层，
// 对应 TODO 第七项"先自测，返回成功后上级按更大范围模块统一测试"的分层语义。
// 实现方决定如何驱动验证：派发 Agent、跑 CLI、调 MCP、模拟 computer use 均可。
type Verifier interface {
	// SelfTest 对产出做单元/功能级验证。
	// ctx 用于取消/超时；req 携带原始任务与上级 Agent 上下文；produced 为待验证的当前产出。
	SelfTest(ctx context.Context, req Request, produced string) (Verdict, error)
	// UnifiedTest 对产出做模块/集成级验证（自测通过后调用）。
	UnifiedTest(ctx context.Context, req Request, produced string) (Verdict, error)
}

// PlanConfirmVerifier 是 Verifier 的可选扩展接口，实现方支持"方案确认前置"步骤：
// 在 SelfTest 前，先让测试方列出测试方案，交产出方确认是否符合其逻辑；
// 不符合则产出方纠正（通过 Fixer），符合后才进 SelfTest。
// 对应 TODO 第七项"不明确具体测试方向时需要先把方案列出，给开发处方案的代码Agent是否符合代码Agent的逻辑"。
//
// 引擎 Run 通过 type assert 检查 Verifier 是否实现本接口：
//   - 实现且 PlanConfirm 返回 Verdict.Passed=true 才进 SelfTest；
//   - 实现且返回 false 则 Fixer.Fix 后再确认（受 maxRounds 约束）；
//   - 未实现则跳过，直接进 SelfTest（向后兼容）。
//
// PlanConfirm 强制开关：planSkipEnabled=true 时跳过本阶段（向后兼容无 PlanConfirm 实现的自定义 Verifier）；
// 默认 false（强制执行），AgentVerifier 已实现 PlanConfirm。
type PlanConfirmVerifier interface {
	// PlanConfirm 让测试方列方案并交产出方确认。
	// produced 为当前产出；返回 Verdict.Passed=true 表示方案确认通过，可进 SelfTest。
	PlanConfirm(ctx context.Context, req Request, produced string) (Verdict, error)
}

// Reviewer 是可选的静态审查接口：PlanConfirm 通过后、SelfTest 前对产出做代码审查。
// 实现方决定审查方式：派发 code_reviewer Agent、调 lint CLI、跑静态分析工具等均可。
// 引擎 Run 通过 reviewer 字段是否为 nil 决定是否执行 Review 阶段（向后兼容）。
//
// Review 与 SelfTest/UnifiedTest 的区别：
//   - Review 关注静态质量（bug/安全/风格/边界/错误处理），不执行代码；
//   - SelfTest 关注单元/功能级动态验证（跑测试）；
//   - UnifiedTest 关注模块/集成级动态验证（构建+跨模块回归）。
type Reviewer interface {
	// Review 对产出做静态审查，返回 Verdict。
	// 未通过时引擎用 Fixer 修正产出后再次审查（受 maxRounds 约束）。
	Review(ctx context.Context, req Request, produced string) (Verdict, error)
}

// Fixer 基于验证失败反馈修正产出，返回新的产出文本。
// 实现方决定修正方式：派发编码 Agent、调 patch 工具、LLM 局部重写等。
type Fixer interface {
	// Fix 基于 feedback 修正 produced，返回修正后的新产出。
	Fix(ctx context.Context, req Request, produced, feedback string) (string, error)
}

// Reporter 上报验证闭环最终结果。实现方决定上报渠道：
// 默认 MailboxReporter 投递父 Agent 邮箱；后续可替换为 webhook、SSE、DAG 回调等。
type Reporter interface {
	// Report 上报一次验证闭环的最终结果。
	Report(req Request, result Result)
}

// defaultMaxRounds 是未显式配置 maxRounds 时的默认往返上限。
const defaultMaxRounds = 5

// defaultChildTimeout 是单次子 Agent 执行的超时，防止挂起拖死整个闭环。
// 实证：思考型模型（glm-5.2）单 LLM 调用 1-3 分钟，验证轮需读全产出+跑测试多轮往返，
// 10 分钟在塔防级任务精确卡死（VERIFY FAIL: self-test failed: context deadline exceeded，
// DONE->FAIL 间隔 9m59s/10m39s），放宽至 20 分钟。
const defaultChildTimeout = 20 * time.Minute

// Request 描述一次验证闭环的输入。字段与具体验证方式解耦，
// 各 Verifier 实现按需读取。
type Request struct {
	ParentID    string // 上级 Agent ID（通常为 domain 或 meta），Runner 以此为 caller
	ProducerID  string // 产出方 Agent ID（如 session-1/code_assistant-1），用于 Reporter 标识来源
	InitialTask string // 原始任务文本（编码任务/文档任务/任何待验证任务）
	Produced    string // 产出方的初始产出，作为验证闭环起点
}

// Result 描述一次验证闭环的最终结果。
type Result struct {
	Passed             bool    // 整体是否通过（自测 + 上级统一测试均通过）
	Rounds             int     // 实际往返轮数（自测 + 修正循环次数）
	FinalProduced      string  // 最终产出（最后一轮修正后的产出）
	PlanConfirmVerdict Verdict // 最后一轮方案确认结论（仅 Verifier 实现 PlanConfirmVerifier 且未跳过时有值）
	ReviewVerdict      Verdict // 最后一轮静态审查结论（仅 reviewer != nil 时有值）
	SelfTestVerdict    Verdict // 最后一轮自测结论
	UnifiedVerdict     Verdict // 最后一轮上级统一测试结论（仅通过自测后才有）
	FailReason         string  // 未通过时的失败原因
}

// Orchestrator 是验证闭环状态机引擎。零值不可用，须通过 New 构造。
// 引擎只消费 Verifier/Fixer/Reporter 接口，不绑定具体验证方式。
type Orchestrator struct {
	verifier        Verifier
	reviewer        Reviewer // 可选静态审查器，nil 跳过 Review 阶段
	fixer           Fixer
	reporter        Reporter
	maxRounds       int
	planSkipEnabled bool // true 跳过 PlanConfirm（向后兼容无实现的自定义 Verifier）
}

// New 创建编排器，使用默认的 AgentVerifier + AgentFixer + MailboxReporter 装配。
// runner 提供 ExecuteChild 能力（通常 *subagent.Dispatcher）；mb 用于默认 Reporter；
// codeRole/testRole 为 Agent 验证器派发的角色 ID；maxRounds<=0 时回退 defaultMaxRounds。
// 这是向后兼容的快捷构造入口；需要自定义验证器/修正器/上报器时用 NewWith。
// 默认不启用 Review 阶段（reviewer=nil），需要 code_reviewer 静态审查时用 NewWithReviewer。
func New(runner Runner, mb *mailbox.Mailbox, maxRounds int, codeRole, testRole string) *Orchestrator {
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}
	return &Orchestrator{
		verifier:  NewAgentVerifier(runner, testRole, verifyMarkerPass, verifyMarkerFail),
		fixer:     NewAgentFixer(runner, codeRole),
		reporter:  NewMailboxReporter(mb),
		maxRounds: maxRounds,
	}
}

// NewWithReviewer 在 New 基础上启用 Review 阶段：派发 reviewRole 做 code_reviewer 静态审查。
// reviewRole 为空时退化为 New（不启用 Review）。
// planSkipEnabled 为 true 时跳过 PlanConfirm（向后兼容）；默认 false 强制执行。
// 这是 Spec->Plan->Code->Review->Test 标准流程的默认装配入口。
func NewWithReviewer(runner Runner, mb *mailbox.Mailbox, maxRounds int, codeRole, testRole, reviewRole string, planSkipEnabled bool) *Orchestrator {
	o := New(runner, mb, maxRounds, codeRole, testRole)
	o.planSkipEnabled = planSkipEnabled
	if reviewRole != "" {
		o.reviewer = NewAgentReviewer(runner, reviewRole, verifyMarkerPass, verifyMarkerFail)
	}
	return o
}

// NewWith 用自定义 Verifier/Fixer/Reporter 装配编排器，支持 ComputerUse/CLI/MCP 等非 Agent 验证场景。
// maxRounds<=0 时回退 defaultMaxRounds。任一接口为 nil 时引擎在对应步骤 panic（调用方应确保装配完整）。
// 默认不启用 Review 阶段；PlanConfirm 按 type-assert 触发（Verifier 实现 PlanConfirmVerifier 即执行，
// 否则跳过）。planSkipEnabled=true 可显式跳过 PlanConfirm。
func NewWith(verifier Verifier, fixer Fixer, reporter Reporter, maxRounds int) *Orchestrator {
	if maxRounds <= 0 {
		maxRounds = defaultMaxRounds
	}
	return &Orchestrator{verifier: verifier, fixer: fixer, reporter: reporter, maxRounds: maxRounds}
}

// Run 驱动验证闭环状态机，阻塞至通过、失败或往返上限。
// ctx 取消时立即返回当前轮次的未通过结果。成功或失败后调用 Reporter.Report 上报。
func (o *Orchestrator) Run(ctx context.Context, req Request) Result {
	result := o.runEngine(ctx, req)
	if o.reporter != nil {
		o.reporter.Report(req, result)
	}
	return result
}

// runEngine 是纯状态机循环，不调用 Reporter，便于测试与复用。
func (o *Orchestrator) runEngine(ctx context.Context, req Request) Result {
	var result Result
	produced := req.Produced
	result.FinalProduced = produced

	for round := 1; round <= o.maxRounds; round++ {
		result.Rounds = round

		if err := ctx.Err(); err != nil {
			result.FailReason = fmt.Sprintf("round %d cancelled: %v", round, err)
			return result
		}

		// 步骤 0：方案确认前置（type-assert 触发，planSkipEnabled=true 显式跳过）。
		// Verifier 实现 PlanConfirmVerifier 且 planSkipEnabled=false 时执行：
		// 先列测试方案交产出方确认，未通过则 Fixer 修正后再次确认（受 maxRounds 约束）。
		// 默认装配（NewWithReviewer）传 AgentVerifier，已实现 PlanConfirm，默认执行。
		if !o.planSkipEnabled {
			if pc, ok := o.verifier.(PlanConfirmVerifier); ok {
				planVerdict, err := pc.PlanConfirm(ctx, req, produced)
				if err != nil {
					if cerr := ctx.Err(); cerr != nil {
						result.FailReason = fmt.Sprintf("round %d cancelled: %v", round, cerr)
						return result
					}
					result.FailReason = fmt.Sprintf("round %d plan-confirm failed: %v", round, err)
					return result
				}
				result.PlanConfirmVerdict = planVerdict
				if !planVerdict.Passed {
					fixed, ferr := o.fixer.Fix(ctx, req, produced, failFeedback(planVerdict))
					if ferr != nil {
						result.FailReason = fmt.Sprintf("round %d fix after plan-reject: %v", round, ferr)
						return result
					}
					produced = fixed
					result.FinalProduced = produced
					continue
				}
			}
		}

		// 步骤 0.5（可选）：静态审查（code_reviewer）。
		// reviewer == nil 时跳过（非 Agent 验证器场景）。
		// PlanConfirm 通过后、SelfTest 前对产出做 bug/安全/风格/边界审查，
		// 未通过则 Fixer 修正后再次审查（受 maxRounds 约束）。
		if o.reviewer != nil {
			reviewVerdict, err := o.reviewer.Review(ctx, req, produced)
			if err != nil {
				if cerr := ctx.Err(); cerr != nil {
					result.FailReason = fmt.Sprintf("round %d cancelled: %v", round, cerr)
					return result
				}
				result.FailReason = fmt.Sprintf("round %d review failed: %v", round, err)
				return result
			}
			result.ReviewVerdict = reviewVerdict
			if !reviewVerdict.Passed {
				fixed, ferr := o.fixer.Fix(ctx, req, produced, failFeedback(reviewVerdict))
				if ferr != nil {
					result.FailReason = fmt.Sprintf("round %d fix after review-reject: %v", round, ferr)
					return result
				}
				produced = fixed
				result.FinalProduced = produced
				continue
			}
		}

		// 步骤 1：自测。
		selfVerdict, err := o.verifier.SelfTest(ctx, req, produced)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				result.FailReason = fmt.Sprintf("round %d cancelled: %v", round, cerr)
				return result
			}
			result.FailReason = fmt.Sprintf("round %d self-test failed: %v", round, err)
			return result
		}
		result.SelfTestVerdict = selfVerdict

		// 自测未通过 -> 修正 -> 下一轮自测。
		if !selfVerdict.Passed {
			fixed, ferr := o.fixer.Fix(ctx, req, produced, failFeedback(selfVerdict))
			if ferr != nil {
				result.FailReason = fmt.Sprintf("round %d fix failed: %v", round, ferr)
				return result
			}
			produced = fixed
			result.FinalProduced = produced
			continue
		}

		// 步骤 2：自测通过，进入上级统一测试。
		unifiedVerdict, err := o.verifier.UnifiedTest(ctx, req, produced)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				result.FailReason = fmt.Sprintf("round %d cancelled: %v", round, cerr)
				return result
			}
			result.FailReason = fmt.Sprintf("round %d unified-test failed: %v", round, err)
			return result
		}
		result.UnifiedVerdict = unifiedVerdict

		// 上级统一测试通过 -> 整体通过。
		if unifiedVerdict.Passed {
			result.Passed = true
			return result
		}

		// 上级统一测试未通过 -> 打回修正 -> 下一轮自测。
		fixed, ferr := o.fixer.Fix(ctx, req, produced, failFeedback(unifiedVerdict))
		if ferr != nil {
			result.FailReason = fmt.Sprintf("round %d fix after unified fail: %v", round, ferr)
			return result
		}
		produced = fixed
		result.FinalProduced = produced
	}

	result.FailReason = fmt.Sprintf("max rounds (%d) exceeded", o.maxRounds)
	return result
}

// failFeedback 把未通过的 Verdict 转为 Fixer 可消费的反馈文本。
func failFeedback(v Verdict) string {
	if v.Reason == "" {
		return v.Detail
	}
	if v.Detail == "" {
		return v.Reason
	}
	return v.Reason + "\n" + v.Detail
}

// VerifyMarkers 是 Agent 验证器使用的文本标记协议，供 AgentVerifier 解析 Verdict。
// 验证 Agent 须在答复中输出 PASS/FAIL 标记其一，AgentVerifier 据此归一为 Verdict。
const (
	verifyMarkerPass = "[VERIFY:PASS]"
	verifyMarkerFail = "[VERIFY:FAIL]"
)

// ParseAgentVerdict 从 Agent 答复文本解析 Verdict。
// 优先匹配 FAIL（防止 PASS 出现在失败原因文本中误判），再匹配 PASS；无标记判失败。
// 供 AgentVerifier 使用，不解析文本的 Verifier 实现无需调用。
func ParseAgentVerdict(report string) Verdict {
	if strings.Contains(report, verifyMarkerFail) {
		return Verdict{Passed: false, Reason: extractReason(report), Detail: report}
	}
	if strings.Contains(report, verifyMarkerPass) {
		return Verdict{Passed: true, Detail: report}
	}
	return Verdict{Passed: false, Reason: "missing verify marker in agent report", Detail: report}
}

// extractReason 从含 FAIL 标记的答复中提取失败原因（标记之后的文本）。
func extractReason(report string) string {
	idx := strings.Index(report, verifyMarkerFail)
	if idx < 0 {
		return ""
	}
	tail := strings.TrimSpace(report[idx+len(verifyMarkerFail):])
	return tail
}
