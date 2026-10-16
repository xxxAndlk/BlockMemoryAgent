<template>
  <div>
    <el-table :data="runs" v-loading="loading" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
      <el-table-column label="运行 ID" min-width="150">
        <template #default="{ row }">
          <router-link :to="`/workflow/runs/${row.id}`" class="text-primary hover:underline font-mono text-xs">
            {{ row.id }}
          </router-link>
        </template>
      </el-table-column>
      <el-table-column label="配方" min-width="150">
        <template #default="{ row }">
          <span class="text-sm">{{ row.recipe_id }}</span>
          <span class="text-xs text-ink-2 ml-1">v{{ row.recipe_version }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="130">
        <template #default="{ row }">
          <el-tag size="small" :type="runStatusType(row.status)" effect="plain">{{ runStatusText(row.status) }}</el-tag>
          <el-tooltip v-if="row.pause_reason" :content="pauseReasonText(row.pause_reason)">
            <el-icon class="ml-1 text-ink-2 align-middle"><WarningFilled /></el-icon>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="进度" min-width="200">
        <template #default="{ row }">
          <div class="flex items-center gap-2">
            <el-progress :percentage="progressOf(row)" :stroke-width="8" class="flex-1 min-w-[80px]" />
            <span class="text-xs text-ink-2 whitespace-nowrap">{{ doneOf(row) }}/{{ totalOf(row) }}</span>
          </div>
          <div v-if="reviewOf(row)" class="text-xs text-orange-400 mt-0.5">待审 {{ reviewOf(row) }}</div>
        </template>
      </el-table-column>
      <el-table-column label="预算" width="110">
        <template #default="{ row }">
          <span class="text-xs" :class="row.spent_estimate >= row.budget_cap ? 'text-red-400' : 'text-ink-2'">
            {{ row.spent_estimate }}/{{ row.budget_cap }}
          </span>
        </template>
      </el-table-column>
      <el-table-column label="发起会话" width="120">
        <template #default="{ row }">
          <span class="font-mono text-xs text-ink-2">{{ row.session_id }}</span>
        </template>
      </el-table-column>
      <el-table-column label="创建时间" width="150">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ fmtDateTime(row.created_at) }}</span>
        </template>
      </el-table-column>
      <template #empty>
        <div class="text-sm text-ink-2 py-10">
          还没有量产运行——在对话里对 Agent 说「按配方 X 量产这批件」（workflow_run），或在工作流详情页发起。
        </div>
      </template>
    </el-table>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import { listRuns, type WorkflowRun } from '@/api/workflow'
import { fmtDateTime } from '@/utils/date'

const props = defineProps<{ refreshKey: number }>()

const runs = ref<WorkflowRun[]>([])
const loading = ref(false)
let timer: ReturnType<typeof setInterval> | null = null

async function load() {
  loading.value = true
  try {
    const res = await listRuns({ limit: 100 })
    runs.value = res.runs || []
  } catch (e) {
    ElMessage.error('读取运行列表失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

function totalOf(row: WorkflowRun): number {
  return Object.values(row.counts || {}).reduce((a, b) => a + b, 0)
}
function doneOf(row: WorkflowRun): number {
  const c = row.counts || {}
  return (c.done || 0) + (c.skipped || 0)
}
function reviewOf(row: WorkflowRun): number {
  return (row.counts || {}).needs_review || 0
}
function progressOf(row: WorkflowRun): number {
  const total = totalOf(row)
  return total === 0 ? 0 : Math.round((doneOf(row) / total) * 100)
}

/** run 状态徽标口径（对齐 dag 页 statusType/OrchMiniCanvas）。 */
function runStatusType(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'warning'
    case 'paused': return 'warning'
    case 'done': return 'success'
    case 'failed': return 'danger'
    case 'aborted': return 'info'
    default: return 'info' // pending
  }
}
function runStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队中', running: '运行中', paused: '已暂停', done: '已完成', failed: '失败', aborted: '已中止',
  }
  return map[s] || s
}
function pauseReasonText(r: string): string {
  const map: Record<string, string> = {
    budget_exceeded: '预算超限暂停（续跑需提额）', user: '用户暂停', circuit_breaker: '早期熔断（配方疑似有问题）',
  }
  return map[r] || r
}

watch(() => props.refreshKey, load)
onMounted(() => {
  load()
  // 列表里有未终态 run 时 3s 轮询进度。
  timer = setInterval(() => {
    if (runs.value.some((r) => r.status === 'running' || r.status === 'pending' || r.status === 'paused')) load()
  }, 3000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>
