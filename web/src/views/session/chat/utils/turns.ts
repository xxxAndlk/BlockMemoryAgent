import type { SessionEvent } from '@/types'
import {
  isUserMessageEvent,
  isToolCallEvent,
  isToolExecEvent,
  isTokenUsageEvent,
  isErrorEvent,
} from '@/types'

/** 一次工具调用：把 tool_call (intent) + tool_exec (结果) 配对 */
export interface ToolCallGroup {
  id: string
  tool: string
  agent: string
  call?: SessionEvent // kind === 'tool_call'
  result?: SessionEvent // type === 'tool_exec'
  success: boolean
  pending: boolean
}

/** 回合内一个按时间顺序的步骤：要么是一段思考，要么是一次工具调用 */
export interface TurnStep {
  kind: 'think' | 'tool'
  event?: SessionEvent // think 步骤对应的事件
  group?: ToolCallGroup // tool 步骤对应的工具调用组
}

/** 一个对话回合：用户消息 → 助手处理过程 → 最终答案 */
export interface Turn {
  id: string
  userMessage?: SessionEvent
  /** 按时间交错的步骤序列，保留 ReAct 时序（思考↔工具↔结果↔下一轮思考） */
  steps: TurnStep[]
  thinkChain: SessionEvent[] // 兼容字段：所有思考事件扁平集合
  toolCalls: ToolCallGroup[] // 兼容字段：所有工具调用组
  errors: SessionEvent[]
  clarifyQuestion?: SessionEvent
  finalAnswer?: SessionEvent
  status: 'running' | 'completed' | 'error' | 'awaiting_clarify'
  startedAt: string
  endedAt?: string
  tokens: { in: number; out: number }
  agents: string[]
}

type EventCategory =
  | 'user_message'
  | 'system_start'
  | 'system_resume'
  | 'tool_call'
  | 'tool_exec'
  | 'tool_result_legacy'
  | 'completion'
  | 'clarify'
  | 'error'
  | 'think'
  | 'other'

const THINK_KINDS = new Set([
  'think',
  'intend',
  'llm',
  'llm_result',
  'llm_response',
  'wait',
  'prompt',
  'agent_created',
  'token_usage',
  'graph_step',
  'notify',
])

/** 将事件归类到单一语义类别，作为后续路由的依据 */
export function classifyEvent(ev: SessionEvent): EventCategory {
  if (isUserMessageEvent(ev)) return 'user_message'
  // 用户对澄清/审批的答复事件（"提问答复: …"/"审批答复: …"，type=clarify + agent=User）
  // 本质是用户发言：归入 user_message 开新回合。否则会命中下方 clarify 分支——把已答复
  // 的问题卡覆盖成答复文本、把回合重新标回 awaiting_clarify，且问答流卡在"待澄清"态直到
  // 完成事件到来（2026-09-08 web 端 ask_user 答复显示修复）。
  if ((ev.type === 'clarify' || ev.kind === 'clarify') && ev.agent === 'User') return 'user_message'
  if (ev.type === 'system' && ev.agent === 'MetaAgent' && ev.message?.startsWith('会话启动')) {
    return 'system_start'
  }
  if (ev.type === 'system' && ev.message?.startsWith('继续会话')) return 'system_resume'
  if (isCompletion(ev)) return 'completion'
  if (ev.type === 'clarify' || ev.kind === 'clarify') return 'clarify'
  if (isToolCallEvent(ev)) return 'tool_call'
  if (isToolExecEvent(ev)) return 'tool_exec'
  if (ev.kind === 'tool_result') return 'tool_result_legacy'
  if (isErrorEvent(ev)) return 'error'
  if (isLLMThinkEvent(ev)) return 'think'
  return 'other'
}

function isLLMThinkEvent(ev: SessionEvent): boolean {
  return THINK_KINDS.has(ev.kind || '') || ev.type === 'progress'
}

export function isCompletion(ev: SessionEvent): boolean {
  return (
    ev.type === 'system' &&
    ev.agent === 'MetaAgent' &&
    (ev.message?.startsWith('会话完成') || ev.message?.startsWith('执行失败'))
  )
}

export function isError(ev: SessionEvent): boolean {
  return isErrorEvent(ev)
}

export function createToolGroup(callEv: SessionEvent): ToolCallGroup {
  return {
    id: callEv.timestamp,
    tool: callEv.tool || extractToolName(callEv.message) || 'unknown',
    agent: callEv.agent,
    call: callEv,
    success: false,
    pending: true,
  }
}

export function finalizeTurn(turn: Turn, finalEvent?: SessionEvent): Turn {
  if (!finalEvent) return turn
  const status: Turn['status'] = finalEvent.message?.startsWith('执行失败') ? 'error' : 'completed'
  return {
    ...turn,
    finalAnswer: finalEvent,
    status,
    endedAt: finalEvent.timestamp,
  }
}

function tokenIn(ev: SessionEvent): number {
  return ev.input_tokens || 0
}

function tokenOut(ev: SessionEvent): number {
  return ev.output_tokens || 0
}

/**
 * 把扁平的 SessionEvent[] 分组成 Turn[]。
 *
 * 规则：
 * - 每条 type='user_message' 开启新回合；或会话初始的 system "会话启动" 事件视为第一个回合的"用户消息"。
 * - 回合内事件按时间顺序路由到 steps[]，保留 ReAct 时序：
 *   think/intend/llm/llm_result/wait/... → think 步骤；
 *   tool_call → 新建工具组 + tool 步骤；tool_exec → 匹配同名 pending 组填 result（不新增步骤）；
 *   error → errors；system "会话完成"/"执行失败" → finalAnswer + 改变 status。
 * - 工具类事件判断优先于 error，避免失败的工具调用被误归 errors 而永久 pending。
 */
export function groupEventsToTurns(events: SessionEvent[]): Turn[] {
  const turns: Turn[] = []
  let current: Turn | null = null

  const openTurn = (ev?: SessionEvent) => {
    current = {
      id: ev?.timestamp || `turn-${turns.length}`,
      userMessage: ev,
      steps: [],
      thinkChain: [],
      toolCalls: [],
      errors: [],
      status: 'running',
      startedAt: ev?.timestamp || new Date().toISOString(),
      tokens: { in: 0, out: 0 },
      agents: [],
    }
    turns.push(current)
  }

  for (const ev of events) {
    const category = classifyEvent(ev)

    // 启动 / 用户消息 → 新回合
    if (category === 'user_message') {
      openTurn(ev)
      continue
    }
    if (category === 'system_start') {
      // 用启动事件作为首个回合的"目标"占位（如果用户已经显式发过 user_message 就别覆盖）
      if (!current) openTurn(ev)
      continue
    }
    if (category === 'system_resume') {
      // 接续上一个回合（用户已 push 过 user_message），忽略
      continue
    }

    if (!current) openTurn()

    // 收集参与的 agent
    if (ev.agent && current!.agents.indexOf(ev.agent) === -1 && ev.agent !== 'System') {
      current!.agents.push(ev.agent)
    }

    // Token 累计
    if (isTokenUsageEvent(ev)) {
      current!.tokens.in += tokenIn(ev)
      current!.tokens.out += tokenOut(ev)
    }

    // 完成事件
    if (category === 'completion') {
      current = finalizeTurn(current!, ev)
      continue
    }

    // 待澄清：Agent 请求用户澄清，挂起会话；区别于错误，单独标记
    if (category === 'clarify') {
      current!.clarifyQuestion = ev
      current!.status = 'awaiting_clarify'
      current!.endedAt = ev.timestamp
      continue
    }

    // 工具调用意图（pending）：新建组并占一个 tool 步骤
    if (category === 'tool_call') {
      const group = createToolGroup(ev)
      current!.toolCalls.push(group)
      current!.steps.push({ kind: 'tool', group })
      continue
    }

    // 工具执行结果（type=tool_exec）：配对到同名 pending 组填 result，不新增步骤
    if (category === 'tool_exec') {
      const toolName = ev.tool || extractToolName(ev.message) || 'unknown'
      const matched = [...current!.toolCalls].reverse().find((g) => g.tool === toolName && g.pending)
      if (matched) {
        matched.result = ev
        matched.success = ev.success !== false
        matched.pending = false
      } else {
        // 没有配对的 call（历史回放/孤儿），作为自包含组新建一个 tool 步骤
        const group: ToolCallGroup = {
          id: ev.timestamp,
          tool: toolName,
          agent: ev.agent,
          result: ev,
          success: ev.success !== false,
          pending: false,
        }
        current!.toolCalls.push(group)
        current!.steps.push({ kind: 'tool', group })
      }
      continue
    }

    // 旧版后端可能仍发 tool_result kind（已废弃），忽略避免重复卡片
    if (category === 'tool_result_legacy') {
      continue
    }

    // 错误（工具失败已由 tool_exec.success=false 体现，此处只收真正的错误事件）
    if (category === 'error') {
      current!.errors.push(ev)
      current!.status = 'error'
      continue
    }

    // 思考链相关 Kind → think 步骤（同时累积到 thinkChain 兼容字段）
    if (category === 'think' || category === 'other') {
      current!.thinkChain.push(ev)
      current!.steps.push({ kind: 'think', event: ev })
      continue
    }
  }

  return turns
}

/** 从消息文本里推断工具名（如 "调用工具 ReadFile" / "执行工具: ReadFile" / "工具 ReadFile 执行成功"） */
function extractToolName(msg?: string): string | undefined {
  if (!msg) return undefined
  const patterns = [
    /调用工具\s+([A-Za-z_][\w]*)/,
    /执行工具[:：]\s*([A-Za-z_][\w]*)/,
    /工具\s+([A-Za-z_][\w]*)\s+执行/,
  ]
  for (const re of patterns) {
    const m = msg.match(re)
    if (m) return m[1]
  }
  return undefined
}

/** 从调用参数 JSON 中提取一行关键入参摘要（读了哪个文件 / 跑了什么命令 / 请求哪个 URL） */
export function toolHeadline(group: ToolCallGroup): string {
  const raw = group.call?.detail_json || group.call?.tool_args || ''
  let args: Record<string, unknown> = {}
  if (raw) {
    try {
      args = JSON.parse(raw)
    } catch {
      args = {}
    }
  }
  switch (group.tool) {
    case 'ReadFile':
    case 'WriteFile':
    case 'EditFile':
    case 'ListDir':
      return String(args.path || group.result?.tool_path || '')
    case 'RunCommand':
      return String(args.command || '')
    case 'HTTPGet':
    case 'HTTPPost':
      return String(args.url || '')
    case 'SearchInFiles':
      return String(args.pattern || '')
    default:
      return group.result?.tool_path || ''
  }
}

/** 简洁模式过滤：剔除偏调试用的事件 */
export function filterTurnForConcise(turn: Turn): Turn {
  return {
    ...turn,
    thinkChain: turn.thinkChain.filter((ev) => {
      const k = ev.kind || ev.type
      return k === 'think' || k === 'intend' || k === 'llm' || k === 'wait'
    }),
  }
}
