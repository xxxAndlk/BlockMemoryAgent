package verifyloop

// verifiers.go 提供 Verifier/Fixer/Reporter 接口的默认实现（Agent 驱动）。
//
// 设计意图：编排器引擎（Orchestrator）只消费接口，具体验证方式可插拔。
// 默认实现覆盖当前"派发测试 Agent + 派发编码 Agent + 邮箱上报"主路径。
// 扩展验证器（ComputerUse/CLI/MCP）待对应基础设施就绪后按 Verifier 接口契约填入。

import (
	"context"
	"fmt"

	"github.com/blockmemory/agent/backend/internal/mailbox"
)

// ---- 默认实现：Agent 驱动 ----

// AgentVerifier 通过派发测试 Agent 执行验证，解析 [VERIFY:PASS/FAIL] 标记归一为 Verdict。
// testRole 为测试角色 ID（如 test_assistant）；passMarker/failMarker 通常为包级常量
// verifyMarkerPass/verifyMarkerFail，留字段便于自定义协议。
type AgentVerifier struct {
	runner     Runner
	testRole   string
	passMarker string
	failMarker string
}

// NewAgentVerifier 创建默认的 Agent 验证器。testRole 为测试角色 ID。
func NewAgentVerifier(runner Runner, testRole, passMarker, failMarker string) *AgentVerifier {
	return &AgentVerifier{runner: runner, testRole: testRole, passMarker: passMarker, failMarker: failMarker}
}

// SelfTest 派发测试 Agent 对产出做单元/功能级验证。
func (v *AgentVerifier) SelfTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	task := fmt.Sprintf("【验证任务】对以下产出进行自测（单元/功能级），判断是否符合原始任务要求。\n\n"+
		"【原始任务】\n%s\n\n"+
		"【产出】\n%s\n\n"+
		"请编写并运行测试，覆盖主路径与边界条件。"+
		"最终答复末尾必须单独一行输出 %s（通过）或 %s（未通过，并附失败原因）。",
		req.InitialTask, produced, v.passMarker, v.failMarker)
	report, err := v.execute(ctx, req.ParentID, task)
	if err != nil {
		return Verdict{}, err
	}
	return ParseAgentVerdict(report), nil
}

// UnifiedTest 派发测试 Agent 做模块/集成级验证。
func (v *AgentVerifier) UnifiedTest(ctx context.Context, req Request, produced string) (Verdict, error) {
	task := fmt.Sprintf("【上级统一测试】对以下产出做模块级/集成级验证，关注模块边界、与现有系统的兼容性、回归风险。\n\n"+
		"【原始任务】\n%s\n\n"+
		"【产出】\n%s\n\n"+
		"请运行集成级测试（构建、跨模块调用、关键路径回归）。"+
		"最终答复末尾必须单独一行输出 %s（通过）或 %s（未通过，并附失败原因与受影响范围）。",
		req.InitialTask, produced, v.passMarker, v.failMarker)
	report, err := v.execute(ctx, req.ParentID, task)
	if err != nil {
		return Verdict{}, err
	}
	return ParseAgentVerdict(report), nil
}

// PlanConfirm 实现 PlanConfirmVerifier 可选接口：派发测试 Agent 列出测试方案并自评是否覆盖产出方逻辑。
// 单轮调用合并"列方案+自评确认"，减少脚本消耗：测试 Agent 在同一轮先列方案，再自评覆盖度，输出 PASS/FAIL。
// 未通过则引擎用 Fixer 修正产出后再次确认。
// 返回的 Verdict.Detail 含完整方案与自评，供 Fixer 引用。
//
// 实现语义对应 TODO 第七项"不明确具体测试方向时需要先把方案列出，给开发处方案的代码Agent
// 是否符合代码Agent的逻辑，不符合代码Agent纠正，符合测试Agent测试Agent进行测试"。
//
// 注意：本实现把"产出方确认"合并进测试 Agent 的自评（AgentVerifier 仅持 testRole runner）。
// 若需严格"产出方（codeRole）确认"，调用方应使用 NewWith 注入自定义 Verifier 实现。
func (v *AgentVerifier) PlanConfirm(ctx context.Context, req Request, produced string) (Verdict, error) {
	task := fmt.Sprintf("【列测试方案并自评】基于以下原始任务与产出：\n"+
		"1. 列出你打算执行的测试方案（覆盖哪些路径、边界、预期）；\n"+
		"2. 自评方案是否覆盖产出方的核心逻辑与边界。\n\n"+
		"【原始任务】\n%s\n\n"+
		"【产出】\n%s\n\n"+
		"只列方案与自评，不执行测试。"+
		"若方案覆盖产出方逻辑、无遗漏关键路径，末尾单独一行输出 %s；否则输出 %s 并附补充建议。",
		req.InitialTask, produced, v.passMarker, v.failMarker)
	report, err := v.execute(ctx, req.ParentID, task)
	if err != nil {
		return Verdict{}, err
	}
	v2 := ParseAgentVerdict(report)
	v2.Detail = report
	return v2, nil
}

// execute 包装 Runner.ExecuteChild：附加超时。
func (v *AgentVerifier) execute(ctx context.Context, parentID, task string) (string, error) {
	childCtx, cancel := context.WithTimeout(ctx, defaultChildTimeout)
	defer cancel()
	return v.runner.ExecuteChild(childCtx, parentID, v.testRole, task)
}

// AgentReviewer 通过派发 code_reviewer 角色做静态代码审查，解析 [VERIFY:PASS/FAIL] 标记归一为 Verdict。
// reviewRole 为审查角色 ID（如 code_reviewer）；passMarker/failMarker 与 AgentVerifier 共用包级常量。
// 实现 Reviewer 接口，供 Orchestrator 在 PlanConfirm 通过后、SelfTest 前调用。
type AgentReviewer struct {
	runner     Runner
	reviewRole string
	passMarker string
	failMarker string
}

// NewAgentReviewer 创建默认的 Agent 审查器。reviewRole 为审查角色 ID（如 code_reviewer）。
func NewAgentReviewer(r Runner, reviewRole, passMarker, failMarker string) *AgentReviewer {
	return &AgentReviewer{runner: r, reviewRole: reviewRole, passMarker: passMarker, failMarker: failMarker}
}

// Review 派发 code_reviewer 角色对产出做静态审查（bug/安全/风格/边界/错误处理）。
// 不执行代码，只读 ReadFile/SearchInFiles/GitDiff 产出 findings。
// 最终答复末尾必须单独一行输出 [VERIFY:PASS] 或 [VERIFY:FAIL] + 原因。
func (v *AgentReviewer) Review(ctx context.Context, req Request, produced string) (Verdict, error) {
	task := fmt.Sprintf("【代码审查】对以下产出做静态审查，判断是否可发布。\n\n"+
		"【原始任务】\n%s\n\n"+
		"【产出】\n%s\n\n"+
		"审查维度：bug/逻辑错误、安全漏洞、风格一致性、边界遗漏、错误处理缺失。\n"+
		"用 ReadFile/SearchInFiles/GitDiff 审查，不执行代码、不重写实现。\n"+
		"只输出 findings 列表（按严重度排序）。"+
		"最终答复末尾必须单独一行输出 %s（无阻断性问题）或 %s（附阻断原因与受影响范围）。",
		req.InitialTask, produced, v.passMarker, v.failMarker)
	childCtx, cancel := context.WithTimeout(ctx, defaultChildTimeout)
	defer cancel()
	report, err := v.runner.ExecuteChild(childCtx, req.ParentID, v.reviewRole, task)
	if err != nil {
		return Verdict{}, err
	}
	return ParseAgentVerdict(report), nil
}

// AgentFixer 通过派发编码 Agent 修正产出。
type AgentFixer struct {
	runner   Runner
	codeRole string
}

// NewAgentFixer 创建默认的 Agent 修正器。codeRole 为编码角色 ID。
func NewAgentFixer(runner Runner, codeRole string) *AgentFixer {
	return &AgentFixer{runner: runner, codeRole: codeRole}
}

// Fix 派发编码 Agent 基于验证反馈修正产出，返回新的产出文本。
func (f *AgentFixer) Fix(ctx context.Context, req Request, produced, feedback string) (string, error) {
	task := fmt.Sprintf("【修正任务】以下产出未通过验证，请修正。\n\n"+
		"【原始任务】\n%s\n\n"+
		"【当前产出】\n%s\n\n"+
		"【验证反馈】\n%s\n\n"+
		"请针对反馈修正，返回修正后的完整产出。不要重复无关内容。",
		req.InitialTask, produced, feedback)
	childCtx, cancel := context.WithTimeout(ctx, defaultChildTimeout)
	defer cancel()
	return f.runner.ExecuteChild(childCtx, req.ParentID, f.codeRole, task)
}

// MailboxReporter 把验证闭环结果投递到父 Agent 邮箱。
// 通过发 MsgInfo"验证通过"；未通过发 MsgEscalate"验证未通过"附失败原因。
type MailboxReporter struct {
	mb *mailbox.Mailbox
}

// NewMailboxReporter 创建邮箱上报器。mb 为 nil 时 Report 为空操作（测试可传 nil）。
func NewMailboxReporter(mb *mailbox.Mailbox) *MailboxReporter {
	return &MailboxReporter{mb: mb}
}

// Report 实现 Reporter 接口，把结果投递到父 Agent 邮箱。
func (r *MailboxReporter) Report(req Request, result Result) {
	if r.mb == nil {
		return
	}
	from := "verifyloop/" + req.ProducerID
	if result.Passed {
		r.mb.Send(&mailbox.Message{
			From:    from,
			To:      req.ParentID,
			Type:    mailbox.MsgInfo,
			Subject: "验证闭环通过: " + req.ProducerID,
			Body: fmt.Sprintf("经过 %d 轮自测+上级统一测试，产出已通过验证。\n\n【最终产出】\n%s",
				result.Rounds, result.FinalProduced),
		})
		return
	}
	r.mb.Send(&mailbox.Message{
		From:    from,
		To:      req.ParentID,
		Type:    mailbox.MsgEscalate,
		Subject: "验证闭环未通过: " + req.ProducerID,
		Body: fmt.Sprintf("经过 %d 轮仍未通过验证。\n\n【失败原因】\n%s\n\n【最终产出】\n%s\n\n【自测报告】\n%s",
			result.Rounds, result.FailReason, result.FinalProduced, result.SelfTestVerdict.Detail),
	})
}

// 编译期断言：默认实现满足各接口契约。
var (
	_ Verifier = (*AgentVerifier)(nil)
	_ Fixer    = (*AgentFixer)(nil)
	_ Reporter = (*MailboxReporter)(nil)
	_ Reviewer = (*AgentReviewer)(nil)
)
