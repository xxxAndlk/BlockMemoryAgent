import type { SessionEvent } from '@/types'

/** 一次工具调用：把 tool_call (intent) + tool_result/tool_exec (结果) 配对 */
export interface ToolCallGroup {
  id: string
  tool: string
  agent: string
  call?: SessionEvent     // kind === 'tool_call'
  result?: SessionEvent   // type === 'tool_exec' 或 kind === 'tool_result'
  success: boolean
  pending: boolean
}

/** 一个对话回合：用户消息 → 助手处理过程 → 最终答案 */
export interface Turn {
  id: string
  userMessage?: SessionEvent
  thinkChain: SessionEvent[]
  toolCalls: ToolCallGroup[]
  errors: SessionEvent[]
  clarifyQuestion?: SessionEvent
  finalAnswer?: SessionEvent
  status: 'running' | 'completed' | 'error' | 'awaiting_clarify'
  startedAt: string
  endedAt?: string
  tokens: { in: number; out: number }
  agents: string[]
}

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

const TOOL_CALL_KINDS = new Set(['tool_call'])
const TOOL_RESULT_KINDS = new Set(['tool_result'])

function isCompletion(ev: SessionEvent): boolean {
  return ev.type === 'system' && ev.agent === 'MetaAgent' && (ev.message?.startsWith('会话完成') || ev.message?.startsWith('执行失败'))
}

function isError(ev: SessionEvent): boolean {
  return ev.type === 'error' || (ev.kind === 'error') || ev.success === false
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
 * - 回合内事件按 Kind 路由：think/intend/llm/wait/prompt/agent_created/token_usage/graph_step → thinkChain；
 *   tool_call → toolCalls[].call；tool_result + tool_exec → toolCalls[].result（按 tool 名最近匹配）；
 *   error → errors；system "会话完成"/"执行失败" → finalAnswer + 改变 status。
 */
export function groupEventsToTurns(events: SessionEvent[]): Turn[] {
  const turns: Turn[] = []
  let current: Turn | null = null

  const openTurn = (ev?: SessionEvent) => {
    current = {
      id: ev?.timestamp || `turn-${turns.length}`,
      userMessage: ev,
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
    // 启动 / 用户消息 → 新回合
    if (ev.type === 'user_message') {
      openTurn(ev)
      continue
    }
    if (ev.type === 'system' && ev.agent === 'MetaAgent' && ev.message?.startsWith('会话启动')) {
      // 用启动事件作为首个回合的"目标"占位（如果用户已经显式发过 user_message 就别覆盖）
      if (!current) openTurn(ev)
      continue
    }
    if (ev.type === 'system' && ev.message?.startsWith('继续会话')) {
      // 接续上一个回合（用户已 push 过 user_message），忽略
      continue
    }

    if (!current) openTurn()

    // 收集参与的 agent
    if (ev.agent && current!.agents.indexOf(ev.agent) === -1 && ev.agent !== 'System') {
      current!.agents.push(ev.agent)
    }

    // Token 累计
    if (ev.kind === 'token_usage') {
      current!.tokens.in += tokenIn(ev)
      current!.tokens.out += tokenOut(ev)
    }

    // 完成事件
    if (isCompletion(ev)) {
      current!.finalAnswer = ev
      current!.status = ev.message?.startsWith('执行失败') ? 'error' : 'completed'
      current!.endedAt = ev.timestamp
      continue
    }

    // 待澄清：Agent 请求用户澄清，挂起会话；区别于错误，单独标记
    if (ev.type === 'clarify' || ev.kind === 'clarify') {
      current!.clarifyQuestion = ev
      current!.status = 'awaiting_clarify'
      current!.endedAt = ev.timestamp
      continue
    }

    // 错误
    if (isError(ev)) {
      current!.errors.push(ev)
      current!.status = 'error'
      continue
    }

    // 工具调用
    if (TOOL_CALL_KINDS.has(ev.kind || '')) {
      current!.toolCalls.push({
        id: ev.timestamp,
        tool: ev.tool || extractToolName(ev.message) || 'unknown',
        agent: ev.agent,
        call: ev,
        success: false,
        pending: true,
      })
      continue
    }

    // 工具结果（kind=tool_result 或 type=tool_exec）
    if (TOOL_RESULT_KINDS.has(ev.kind || '') || ev.type === 'tool_exec') {
      const toolName = ev.tool || extractToolName(ev.message) || 'unknown'
      // 反向找到最近一个同名、未匹配的 call
      const matched = [...current!.toolCalls].reverse().find(g => g.tool === toolName && g.pending)
      if (matched) {
        matched.result = ev
        matched.success = ev.success !== false
        matched.pending = false
      } else {
        // 没有配对的 call，作为孤儿结果新建一个组
        current!.toolCalls.push({
          id: ev.timestamp,
          tool: toolName,
          agent: ev.agent,
          result: ev,
          success: ev.success !== false,
          pending: false,
        })
      }
      continue
    }

    // 思考链相关 Kind
    if (THINK_KINDS.has(ev.kind || '') || ev.type === 'progress') {
      current!.thinkChain.push(ev)
      continue
    }

    // agent_done / stats / 其它系统事件：也归到思考链
    current!.thinkChain.push(ev)
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

/** 简洁模式过滤：剔除偏调试用的事件 */
export function filterTurnForConcise(turn: Turn): Turn {
  return {
    ...turn,
    thinkChain: turn.thinkChain.filter(ev => {
      const k = ev.kind || ev.type
      return k === 'think' || k === 'intend' || k === 'llm' || k === 'wait'
    }),
  }
}
