// agentTurns.ts 编排页适配器：把单 Agent 消息流（GET /sessions/:id/agents/:aid/messages
// 的 AgentMessageItem[]）适配成对话页的回合结构 Turn[]，以便复用 chat 的
// UserBubble / PeerBubble / AssistantTurn / ThinkChain / ToolActivity 组件渲染。
// 合成事件仅供展示，不回写任何状态（设计 §编排页改版）。
// 发送者口径（user_message 事件的 agent 字段）：mailbox 注入消息取 `[mailbox from X]`
// 的 X（user / dispatcher / 子 Agent inst_id / verifyloop/*）；无前缀的首条任务消息
// 由上级派发——meta 的来自人类取 'user'，子 Agent 的取 parent_id（兜底 'dispatcher'）。
import type { AgentMessageItem } from '@/api/session'
import type { AgentNode, SessionEvent } from '@/types'
import type { ToolCallGroup, Turn, TurnStep } from '../../chat/utils/turns'

const MAILBOX_PREFIX = '[mailbox from '

/** mailbox 注入的 user 消息：解析发送方（后端格式 `[mailbox from X] 主题\n正文`）。 */
export function mailboxFrom(content: string): string | null {
  if (!content.startsWith(MAILBOX_PREFIX)) return null
  const end = content.indexOf(']')
  return end > MAILBOX_PREFIX.length ? content.slice(MAILBOX_PREFIX.length, end) : null
}

/** 剥离 `[mailbox from X]` 前缀后的正文。 */
function stripMailboxPrefix(content: string): string {
  return content.replace(/^\[mailbox from [^\]]*\]\s*/, '')
}

/** 合成事件时间戳：热层条目带 at；PG 回退路径无 at 时用空串（fmtTime 容错）。 */
function atOf(m: AgentMessageItem): string {
  return m.at || ''
}

/** 合成事件 ID：seq 在 Agent 内单调唯一，直接用作稳定 key。 */
function evId(m: AgentMessageItem, suffix?: string): string {
  return `seq-${m.seq}${suffix ? '-' + suffix : ''}`
}

/**
 * 把单 Agent 的 role 消息流分组成对话页 Turn[]。
 *
 * 规则：
 * - `role='user'` 开新回合：合成 user_message 事件（agent = mailbox 发送方；
 *   无前缀的首条任务/复活种子 = 上级派发，meta 取 'user'，子 Agent 取 parent_id），
 *   message = 剥离 `[mailbox from X]` 前缀的正文）。
 * - `assistant.reasoning` → think 事件（type='progress'，kind='think'）入 steps/thinkChain。
 * - `assistant.tool_calls[i]` → tool_call 事件；后续 `role='tool'` 按 tool_call_id
 *   配对成 tool_exec 事件（tool_output=content，success 默认 true），组 ToolCallGroup。
 * - 回合最后一条有正文的 assistant 消息 → finalAnswer（message 直接给正文；
 *   AssistantTurn 的 finalText 正则会剥"会话完成："前缀，故正文不要带此前缀）。
 * - 回合 status：agent 运行中且为最后一回合 → 'running'，其余 'completed'。
 */
export function groupAgentMessagesToTurns(messages: AgentMessageItem[], agent: AgentNode | null): Turn[] {
  const turns: Turn[] = []
  let current: Turn | null = null
  // tool_call_id → 待配对工具组（role='tool' 结果回填）
  const pendingTools = new Map<string, ToolCallGroup>()
  const agentName = agent?.name || agent?.inst_id || 'Agent'
  const agentRunning = !!agent && (agent.status === 'running' || agent.status === 'active')
  // 无前缀 user 消息（首条派发任务/复活种子）的真实发送者：meta 的任务来自人类，
  // 子 Agent 的任务来自上级（parent_id 缺失时兜底 'dispatcher'）。
  const isMeta = agent?.type === 'meta' || agent?.inst_id === 'meta'
  const taskSender = isMeta ? 'user' : agent?.parent_id || 'dispatcher'

  const openTurn = (ev: SessionEvent) => {
    // 新输入接替仍在运行的上一回合：标 completed，避免出现多个"处理中"假转圈。
    if (current && current.status === 'running') {
      current.status = 'completed'
      current.endedAt = ev.timestamp
    }
    current = {
      id: `turn-${turns.length}`,
      userMessage: ev,
      steps: [],
      thinkChain: [],
      toolCalls: [],
      errors: [],
      subAgents: [], // 单 Agent 对话面板不展示子 Agent 列表（该 Agent 自身即对话主体）
      clarifyDetails: [],
      narrations: [], // 单 Agent 面板的流式正文走 live 渲染（消息流里没有 assistant_text 事件）
      status: 'running',
      startedAt: ev.timestamp,
      tokens: { in: 0, out: 0 },
      agents: [agentName],
    }
    turns.push(current)
    pendingTools.clear()
  }

  const ensureTurn = (m: AgentMessageItem): Turn => {
    if (!current) {
      // 兜底：消息流不以 user 开头（PG 回退窗口截断等），开一个无用户消息的回合。
      openTurn({
        type: 'user_message',
        agent: 'user',
        message: '',
        timestamp: atOf(m),
      })
      current!.userMessage = undefined
      current!.id = evId(m)
      current!.startedAt = atOf(m)
    }
    return current!
  }

  for (const m of messages) {
    if (m.role === 'user') {
      const from = mailboxFrom(m.content)
      const ev: SessionEvent = {
        type: 'user_message',
        agent: from || taskSender,
        message: from ? stripMailboxPrefix(m.content) : m.content,
        timestamp: atOf(m),
      }
      openTurn(ev)
      continue
    }

    const turn = ensureTurn(m)

    if (m.role === 'assistant') {
      // 思考过程 → think 事件（progress/think 会命中对话页的 think 分类）
      if (m.reasoning) {
        const ev: SessionEvent = {
          type: 'progress',
          kind: 'think',
          agent: agentName,
          message: m.reasoning,
          timestamp: atOf(m),
        }
        turn.thinkChain.push(ev)
        turn.steps.push({ kind: 'think', event: ev })
      }
      // 工具调用意图 → tool_call 事件 + 待配对组
      for (const tc of m.tool_calls || []) {
        const callEv: SessionEvent = {
          type: 'tool_call',
          kind: 'tool_call',
          agent: agentName,
          message: `调用工具 ${tc.name}`,
          tool: tc.name,
          tool_args: stringifyArgs(tc.input),
          timestamp: atOf(m),
        }
        const group: ToolCallGroup = {
          id: evId(m, tc.id),
          tool: tc.name,
          agent: agentName,
          call: callEv,
          success: false,
          pending: true,
        }
        turn.toolCalls.push(group)
        const step: TurnStep = { kind: 'tool', group }
        turn.steps.push(step)
        if (tc.id) pendingTools.set(tc.id, group)
      }
      // 正文：先记着，回合结束/被接替时取最后一条非空正文作 finalAnswer
      if (m.content) {
        const ev: SessionEvent = {
          type: 'agent_reply',
          agent: agentName,
          message: m.content,
          timestamp: atOf(m),
        }
        turn.finalAnswer = ev
        turn.endedAt = atOf(m)
      }
      continue
    }

    if (m.role === 'tool') {
      // 工具结果按 tool_call_id 配对；配对不到时建自包含组（历史截断/孤儿结果）。
      const group = (m.tool_call_id && pendingTools.get(m.tool_call_id)) || undefined
      const execEv: SessionEvent = {
        type: 'tool_exec',
        agent: agentName,
        message: '',
        tool: group?.tool || '',
        tool_output: m.content,
        success: true,
        timestamp: atOf(m),
      }
      if (group) {
        group.result = execEv
        group.success = true
        group.pending = false
        pendingTools.delete(m.tool_call_id!)
      } else {
        const orphan: ToolCallGroup = {
          id: evId(m),
          tool: 'unknown',
          agent: agentName,
          result: execEv,
          success: true,
          pending: false,
        }
        turn.toolCalls.push(orphan)
        turn.steps.push({ kind: 'tool', group: orphan })
      }
      continue
    }

    // 其他 role（system 等）：归入思考链兜底展示，避免静默丢失
    if (m.content) {
      const ev: SessionEvent = {
        type: 'progress',
        kind: 'notify',
        agent: agentName,
        message: m.content,
        timestamp: atOf(m),
      }
      turn.thinkChain.push(ev)
      turn.steps.push({ kind: 'think', event: ev })
    }
  }

  // 收尾：agent 运行中且最后一回合仍在运行 → 保持 'running'（展示"正在生成"）；
  // 其余回合一律 'completed'（消息流是历史快照，无终结事件可判 error/cancelled）。
  for (let i = 0; i < turns.length; i++) {
    const t = turns[i]
    if (t.status !== 'running') continue
    if (i === turns.length - 1 && agentRunning) {
      // 保持 running
    } else {
      t.status = 'completed'
    }
  }
  return turns
}

/** 工具入参序列化为字符串（tool_args 口径同对话页：JSON 文本）。 */
function stringifyArgs(input: unknown): string {
  if (input == null) return ''
  if (typeof input === 'string') return input
  try {
    return JSON.stringify(input)
  } catch {
    return ''
  }
}
