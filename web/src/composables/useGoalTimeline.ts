import { computed, unref } from 'vue'
import type { ComputedRef, MaybeRef } from 'vue'
import type { AgentNode, SessionEvent, TaskBoardData } from '@/types'
import { isErrorEvent, isToolCallEvent, isUserMessageEvent } from '@/types'

/** Milestone 是任务目标时间线上的一个里程碑点（设计 §8：纯前端聚合，零后端改动）。 */
export interface Milestone {
  at: string
  kind: 'user' | 'dispatch' | 'done' | 'failed'
  text: string
}

/** 时间线条数上限：超出保留最近 N 条（长会话事件流可达数千条）。 */
const MAX_MILESTONES = 50
/** 里程碑文本截断长度（首行摘要，避免长文撑爆面板）。 */
const TEXT_CLIP = 80

/** 派发类工具名（子 Agent 派发）：单派与同构批量两种入口都算"派发"里程碑。 */
const DISPATCH_TOOLS = ['call_sub_agent', 'map_sub_agents']

function clip(text: string): string {
  const first = (text || '').split('\n')[0].trim()
  return first.length > TEXT_CLIP ? first.slice(0, TEXT_CLIP) + '…' : first
}

/** 从工具入参里尽力取领域名（domain 参数优先），取不到回落到工具名本身。 */
function dispatchLabel(ev: SessionEvent): string {
  const args = ev.tool_args || ''
  if (args) {
    try {
      const parsed = JSON.parse(args) as Record<string, unknown>
      const domain = parsed.domain || parsed.role_id || parsed.task
      if (typeof domain === 'string' && domain.trim()) return clip(domain)
    } catch {
      // 入参非 JSON（截断/非结构化）时回落工具名，不因解析失败丢里程碑。
    }
  }
  return ev.tool || '子 Agent'
}

/**
 * useGoalTimeline 把会话事件流 + 看板快照聚合成任务目标时间线（看板面板用）。
 * 提取规则与兜底见实现内注释；events 为空或提不出任何里程碑时从 board.tasks 合成，
 * 保证面板永不空白（设计 §8）。
 * agents 用于把 sub_agent_done 事件 message 里的子 Agent 实例 ID 映射成显示名。
 */
export function useGoalTimeline(
  events: MaybeRef<SessionEvent[]>,
  board: MaybeRef<TaskBoardData | null>,
  agents?: MaybeRef<AgentNode[]>,
): { milestones: ComputedRef<Milestone[]> } {
  const milestones = computed<Milestone[]>(() => {
    const agentList = unref(agents) || []
    const out: Milestone[] = []
    for (const ev of unref(events) || []) {
      if (isUserMessageEvent(ev)) {
        out.push({ at: ev.timestamp, kind: 'user', text: clip(ev.message) })
        continue
      }
      if (isToolCallEvent(ev) && DISPATCH_TOOLS.some((t) => (ev.tool || '').includes(t))) {
        out.push({ at: ev.timestamp, kind: 'dispatch', text: '派发 ' + dispatchLabel(ev) })
        continue
      }
      // 完成里程碑：后端会话完成发 agent_done；子 Agent 完成经 mailbox 事件落为 sub_agent_done。
      // sub_agent_done 的 message 携带子 Agent 实例 ID（形如 session-1/domain-1），
      // 经 agents 映射为显示名；ev.agent 常为笼统的 "SubAgent"，不直接展示。
      if (ev.kind === 'agent_done' || ev.kind === 'sub_agent_done') {
        const instId = (ev.message || '').trim()
        const name = agentList.find((a) => a.inst_id === instId)?.name || ''
        const fallback = ev.agent && ev.agent !== 'MetaAgent' && ev.agent !== 'SubAgent' ? ev.agent : ''
        const who = name || fallback
        out.push({ at: ev.timestamp, kind: 'done', text: who ? `${who} 完成` : '完成' })
        continue
      }
      if (isErrorEvent(ev)) {
        out.push({ at: ev.timestamp, kind: 'failed', text: clip(ev.message) })
      }
    }

    // 兜底：事件流为空/未提不出里程碑时，用看板任务合成（每任务一条，状态即语义）。
    if (!out.length) {
      const tasks = unref(board)?.tasks || []
      for (const t of tasks) {
        const done = t.status === 'done'
        const failed = t.status === 'failed' || t.status === 'delivered-unverified'
        out.push({
          at: '',
          kind: done ? 'done' : failed ? 'failed' : 'dispatch',
          text: (done ? '完成 ' : failed ? '未过 ' : '待办 ') + clip(t.title || ''),
        })
      }
    }

    return out.length > MAX_MILESTONES ? out.slice(out.length - MAX_MILESTONES) : out
  })

  return { milestones }
}
