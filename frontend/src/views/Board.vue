<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import type { Session, TaskBoardData, SubTask } from '../types'

const sessions = ref<Session[]>([])
const activeSessionId = ref<string>('')
const board = ref<TaskBoardData | null>(null)
const loading = ref(false)

onMounted(load)

async function load() {
  try {
    const r = await fetch('/api/sessions')
    const list: Session[] = await r.json()
    sessions.value = list
    const running = list.find(s => s.status === 'running')
    const target = running || list[list.length - 1]
    if (target) {
      activeSessionId.value = target.id
      await loadBoard(target.id)
    }
  } catch {}
}

async function loadBoard(id: string) {
  activeSessionId.value = id
  loading.value = true
  try {
    const r = await fetch(`/api/sessions/${id}/board`)
    const d = await r.json()
    board.value = d.board || null
  } catch {
    board.value = null
  } finally {
    loading.value = false
  }
}

const cols = [
  { key: 'pending', label: '待处理' },
  { key: 'in_progress', label: '进行中' },
  { key: 'blocked', label: '阻塞' },
  { key: 'done', label: '已完成' },
  { key: 'failed', label: '失败' },
]

const tasksByCol = computed(() => {
  const map: Record<string, SubTask[]> = { pending: [], in_progress: [], blocked: [], done: [], failed: [] }
  if (board.value) {
    for (const t of board.value.tasks) {
      if (map[t.status]) map[t.status].push(t)
    }
  }
  return map
})

function statusLabel(s: string) {
  return ({ NEW: '新建', IN_PROGRESS: '进行中', DONE: '已完成', FAILED: '失败' } as Record<string, string>)[s] || s
}
</script>

<template>
  <div class="board">
    <div class="header">
      <div class="title-row">
        <div class="title">任务看板</div>
        <select v-model="activeSessionId" @change="loadBoard(activeSessionId)" class="sess-select">
          <option v-for="s in sessions" :key="s.id" :value="s.id">
            {{ s.goal.slice(0, 24) }}{{ s.goal.length > 24 ? '…' : '' }} · {{ s.status }}
          </option>
        </select>
      </div>
      <div v-if="board" class="meta-row">
        <span class="board-goal">目标: {{ board.goal }}</span>
        <span :class="'badge board-status-'+board.status">{{ statusLabel(board.status) }}</span>
        <span class="count">共 {{ board.tasks.length }} 个子任务</span>
      </div>
      <div v-if="board && board.constraints && Object.keys(board.constraints).length" class="constraints">
        <span class="ckey" v-for="(v, k) in board.constraints" :key="k">{{ k }}: {{ v }}</span>
      </div>
    </div>
    <div v-if="loading" class="empty">加载中...</div>
    <div v-else-if="!board" class="empty">该会话尚无任务看板</div>
    <div v-else class="kanban">
      <div v-for="c in cols" :key="c.key" class="col">
        <div class="col-header">
          {{ c.label }}
          <span class="col-count">{{ tasksByCol[c.key].length }}</span>
        </div>
        <div class="col-items">
          <div v-for="t in tasksByCol[c.key]" :key="t.id" class="task-card" :class="'task-'+t.status">
            <div class="task-title">{{ t.title }}</div>
            <div v-if="t.assignee" class="task-assignee">执行: {{ t.assignee }}</div>
            <div v-if="t.result" class="task-result">{{ t.result }}</div>
            <div class="task-id">{{ t.id }}</div>
          </div>
          <div v-if="!tasksByCol[c.key].length" class="col-empty">空</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.board { display: flex; flex-direction: column; height: 100%; gap: 12px; }
.header { background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 12px 14px; }
.title-row { display: flex; align-items: center; gap: 12px; margin-bottom: 8px; }
.title { font-size: 16px; font-weight: 700; }
.sess-select { background: #1a2332; border: 1px solid #243447; color: #e2e8f0; border-radius: 4px; padding: 4px 8px; font-size: 11px; max-width: 240px; margin-left: auto; }
.meta-row { display: flex; align-items: center; gap: 12px; font-size: 12px; color: #94a3b8; flex-wrap: wrap; }
.board-goal { color: #e2e8f0; }
.count { color: #64748b; }
.constraints { margin-top: 8px; display: flex; gap: 8px; flex-wrap: wrap; }
.ckey { font-size: 10px; padding: 2px 8px; background: rgba(234,179,8,0.1); color: #eab308; border-radius: 3px; }
.badge { font-size: 10px; padding: 2px 8px; border-radius: 10px; font-weight: 700; }
.board-status-NEW { background: rgba(100,116,139,0.15); color: #64748b; }
.board-status-IN_PROGRESS { background: rgba(59,130,246,0.15); color: #3b82f6; }
.board-status-DONE { background: rgba(34,197,94,0.15); color: #22c55e; }
.board-status-FAILED { background: rgba(239,68,68,0.15); color: #ef4444; }
.kanban { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; flex: 1; min-height: 0; }
.col { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; min-height: 0; }
.col-header { padding: 10px 12px; font-size: 12px; font-weight: 700; color: #94a3b8; border-bottom: 1px solid #243447; display: flex; justify-content: space-between; align-items: center; }
.col-count { font-size: 10px; padding: 1px 6px; background: #1a2332; border-radius: 10px; color: #64748b; }
.col-items { flex: 1; overflow-y: auto; padding: 8px; display: flex; flex-direction: column; gap: 6px; }
.col-empty { padding: 20px 0; text-align: center; font-size: 11px; color: #64748b; }
.task-card { background: #1a2332; border: 1px solid #243447; border-left: 3px solid #64748b; border-radius: 4px; padding: 8px 10px; font-size: 12px; }
.task-pending { border-left-color: #64748b; }
.task-in_progress { border-left-color: #3b82f6; }
.task-blocked { border-left-color: #eab308; }
.task-done { border-left-color: #22c55e; }
.task-failed { border-left-color: #ef4444; }
.task-title { color: #e2e8f0; margin-bottom: 4px; }
.task-assignee { font-size: 10px; color: #a855f7; }
.task-result { font-size: 10px; color: #94a3b8; margin-top: 4px; max-height: 80px; overflow-y: auto; white-space: pre-wrap; word-break: break-all; background: #0a0e17; padding: 4px 6px; border-radius: 3px; }
.task-id { font-size: 9px; color: #64748b; font-family: 'JetBrains Mono', monospace; margin-top: 4px; }
.empty { padding: 60px 20px; text-align: center; color: #64748b; font-size: 12px; }
</style>
