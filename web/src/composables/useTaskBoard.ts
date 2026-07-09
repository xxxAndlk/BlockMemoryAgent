import { computed, unref } from 'vue'
import type { MaybeRef } from 'vue'
import type { SubTask, TaskBoardData } from '@/types'

export interface TaskItem {
  title?: string
  name?: string
  assignee?: string
  status: string
}

export function useTaskBoard(board: MaybeRef<TaskBoardData | null>) {
  const tasks = computed<TaskItem[]>(() => {
    const b = unref(board)
    if (b?.tasks?.length) {
      return b.tasks.map((t: SubTask) => ({
        title: t.title,
        assignee: t.assignee,
        status: t.status,
      }))
    }
    return []
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
