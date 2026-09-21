/**
 * plan.ts 计划确认工具（submit_plan / review_plan）的展示解析。
 *
 * 这两类调用是里程碑而不是普通工具步骤：下级提交计划后**阻塞等上级审批**，结论决定
 * 后续要不要动手。此前它们和 ReadFile/RunCommand 一样被折进「已执行 N 次工具」，用户
 * 在对话栏里根本看不到（2026-09-17 用户实证），故单独成卡（计划正文 + 审批结论）。
 *
 * 数据来源两种形状都要吃：
 * - 主对话（chat/turns.ts）：tool_call 事件的 detail_json/tool_args + tool_exec 的 tool_output；
 * - 子 Agent 面板（orch/agentTurns.ts）：assistant.tool_calls 的 input + role='tool' 消息，
 *   后者整条是 ToolResultJSON 信封 {"tool","success","output"}，要剥壳。
 */

/** 计划确认类工具名（其余工具仍走 ToolActivity 折叠）。 */
export const PLAN_TOOLS = ['submit_plan', 'review_plan']

export function isPlanTool(name?: string | null): boolean {
  return !!name && PLAN_TOOLS.includes(name)
}

/** 一次计划提交/审批的展示数据。 */
export interface PlanStep {
  id: string
  tool: string
  agent: string
  /** submit_plan：任务简介（一句话） */
  taskSummary: string
  /** submit_plan：计划正文（markdown）。reject 后重新提交时是新版本 */
  planText: string
  /** 审批状态：pending=还在等上级答复；approved/rejected/unknown 来自结果文案 */
  verdict: 'pending' | 'approved' | 'rejected' | 'unknown'
  /** 驳回时的修改意见（从结果文案剥出，独立展示） */
  feedback: string
  /** fail-open（2026-09-20）：审批等待超时/通道不可用，未经批准按计划自动放行——
   *  界面上必须与普通"已返回"区分，否则用户看不出计划其实没被审批 */
  timedOut: boolean
  /** 结果原文（兜底折叠展示） */
  resultText: string
  success: boolean
  ts: string
}

/** 结果文案是否 fail-open（超时/通道不可用自动放行，文案见 plan_confirm.go）。 */
export function isFailOpenText(resultText: string): boolean {
  return /fail-open|审批等待超时|未及时答复|按计划继续执行/.test(resultText)
}

/** 工具入参取字段：入参是 JSON 文本（detail_json / tool_args / stringifyArgs 的产物）。 */
export function planArgsOf(raw?: string): Record<string, unknown> {
  if (!raw) return {}
  try {
    const v = JSON.parse(raw)
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {}
  } catch {
    return {} // 非 JSON（旧事件/截断）：当空处理，正文走 resultText 兜底
  }
}

/** 剥 ToolResultJSON 信封：`{"tool":..,"success":..,"output":".."}` → output；非信封原样返回。 */
export function unwrapOutput(text?: string): string {
  const t = (text || '').trim()
  if (!t.startsWith('{')) return text || ''
  try {
    const v = JSON.parse(t)
    if (v && typeof v === 'object' && typeof (v as Record<string, unknown>).output === 'string') {
      return (v as Record<string, unknown>).output as string
    }
  } catch {
    // 正文本身以 { 开头但不是 JSON（如 JSON 片段输出）：原样返回
  }
  return text || ''
}

/**
 * 从结果文案判结论（后端文案见 plan_confirm.go / review_plan 工具）：
 * - submit_plan 侧：【计划已批准】… / 【计划被驳回】… / 【计划驳回达上限】…
 * - review_plan 侧（审批人自己那笔调用）："已回传审批结果: 批准。" / "…: 驳回（…）。"
 */
export function verdictOf(resultText: string): 'approved' | 'rejected' | 'unknown' {
  if (/计划已批准|审批结果[:：]\s*批准/.test(resultText)) return 'approved'
  if (/计划被驳回|计划驳回达上限|审批结果[:：]\s*驳回/.test(resultText)) return 'rejected'
  return 'unknown'
}

/** 驳回意见：结果首行"【计划被驳回】上级修改意见: xxx"，剥到"请按意见修订…"为止。 */
export function feedbackOf(resultText: string): string {
  const m = resultText.match(/【计划(?:被驳回|驳回达上限)】[^\n]*/)
  if (!m) return ''
  return m[0]
    .replace(/^【计划(?:被驳回|驳回达上限)】/, '')
    .replace(/^[^:：]*[:：]?\s*/, '')
    .replace(/请(?:按意见)?修订后重新调用.*$/s, '')
    .trim()
}

/** 组装一条计划步骤（两处数据源共用）。 */
export function buildPlanStep(input: {
  id: string
  tool: string
  agent: string
  rawArgs?: string
  resultText?: string
  success?: boolean
  ts: string
}): PlanStep {
  const args = planArgsOf(input.rawArgs)
  const resultText = unwrapOutput(input.resultText)
  const hasResult = !!input.resultText
  return {
    id: input.id,
    tool: input.tool,
    agent: input.agent,
    taskSummary: typeof args.task_summary === 'string' ? args.task_summary : '',
    planText: typeof args.plan === 'string' ? args.plan : '',
    verdict: hasResult ? verdictOf(resultText) : 'pending',
    feedback: hasResult ? feedbackOf(resultText) : '',
    timedOut: hasResult && isFailOpenText(resultText),
    resultText,
    success: input.success !== false,
    ts: input.ts,
  }
}

/** 结果到达后就地补充（提交时先渲染"审批中"，结论回填同一张卡）。 */
export function fillPlanResult(step: PlanStep, resultText?: string, success?: boolean) {
  const text = unwrapOutput(resultText)
  step.resultText = text
  step.success = success !== false
  step.verdict = verdictOf(text)
  step.feedback = feedbackOf(text)
  step.timedOut = isFailOpenText(text)
}
