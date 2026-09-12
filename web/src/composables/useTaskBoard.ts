import { computed, unref } from 'vue'
import type { MaybeRef } from 'vue'
import type { AgentNode, SubTask, TaskBoardData } from '@/types'

export interface TaskItem {
  title?: string
  name?: string
  assignee?: string
  status: string
}

/** 任务标题截断（goal 取首行，避免长目标撑爆任务行）。 */
const TITLE_CLIP = 60
function clipTitle(text: string): string {
  const first = (text || '').split('\n')[0].trim()
  return first.length > TITLE_CLIP ? first.slice(0, TITLE_CLIP) + '…' : first
}

/** Agent 状态 → 任务行状态口径（对齐 TaskBoardPanel 的状态图标分支）。 */
function toTaskStatus(agentStatus: string): string {
  switch (agentStatus) {
    case 'done':
      return 'done'
    case 'running':
    case 'active':
      return 'running'
    case 'failed':
      return 'failed'
    case 'delivered-unverified':
      return 'delivered-unverified'
    // paused/cancelled/idle 等一律归入 blocked（等待/阻塞图例行）。
    default:
      return 'blocked'
  }
}

export function useTaskBoard(board: MaybeRef<TaskBoardData | null>, agents?: MaybeRef<AgentNode[]>) {
  const tasks = computed<TaskItem[]>(() => {
    const b = unref(board)
    if (b?.tasks?.length) {
      return b.tasks.map((t: SubTask) => ({
        title: t.title,
        assignee: t.assignee,
        status: t.status,
      }))
    }
    // 看板无任务快照时从 Agent 列表合成：每个非 meta Agent 即一条实际任务
    // （派发即任务，避免面板长期停在"暂无子任务"）。
    const list = unref(agents) || []
    return list
      .filter((a) => a.type !== 'meta' && a.inst_id !== 'meta')
      .map((a) => ({
        title: clipTitle(a.goal || '') || a.name,
        assignee: a.name,
        status: toTaskStatus(a.status),
      }))
  })

  const constraints = computed<[string, string][]>(() => {
    const b = unref(board)
    if (b?.constraints) return Object.entries(b.constraints)
    return []
  })

  const taskProgress = computed(() => {
    const done = tasks.value.filter((t) => t.status === 'done').length
    const total = tasks.value.length
    return total ? Math.round((done / total) * 100) : 0
  })

  return { tasks, constraints, taskProgress }
}
