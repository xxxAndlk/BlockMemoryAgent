<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { approveTeamPlan, listTeamAttempts, reconcileTeam, type DispatchAttempt } from '@/api/team'
import { fmtDateTime } from '@/utils/date'

const sessionId = ref('')
const attempts = ref<DispatchAttempt[]>([])
const loading = ref(false)
const reconciling = ref(false)
let timer: ReturnType<typeof setInterval> | null = null

async function load() {
  if (!sessionId.value.trim()) {
    attempts.value = []
    return
  }
  loading.value = true
  try {
    attempts.value = await listTeamAttempts(sessionId.value.trim())
  } catch (e) {
    ElMessage.error('自动调度 attempts 加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function approve() {
  if (!sessionId.value.trim()) {
    ElMessage.warning('先填会话 ID')
    return
  }
  try {
    const res = await approveTeamPlan(sessionId.value.trim())
    ElMessage.success(`已批准计划 v${res.plan_version}`)
  } catch (e) {
    ElMessage.error('批准失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function reconcile() {
  if (!sessionId.value.trim()) {
    ElMessage.warning('先填会话 ID')
    return
  }
  reconciling.value = true
  try {
    const done = await reconcileTeam(sessionId.value.trim())
    ElMessage.success(done.length ? `已触发调度：${done.join(', ')}` : '无可调度会话')
    await load()
  } catch (e) {
    ElMessage.error('触发调度失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    reconciling.value = false
  }
}

function statusTag(s: string): 'success' | 'warning' | 'danger' | 'info' {
  switch (s) {
    case 'done': return 'success'
    case 'running': return 'info'
    case 'reserved': return 'warning'
    case 'failed': return 'danger'
    default: return 'info'
  }
}

function startPolling() {
  stopPolling()
  timer = setInterval(() => {
    if (document.visibilityState === 'visible') load()
  }, 3000)
}
function stopPolling() {
  if (timer) { clearInterval(timer); timer = null }
}

onMounted(() => {
  startPolling()
})
onBeforeUnmount(stopPolling)
</script>

<template>
  <div class="page">
    <div class="head">
      <h2>自动调度运行</h2>
      <div class="controls">
        <el-input v-model="sessionId" placeholder="会话 ID，如 session-12" class="sid" @keyup.enter="load" />
        <el-button :loading="loading" @click="load">刷新</el-button>
        <el-button @click="approve">批准计划</el-button>
        <el-button type="primary" :loading="reconciling" @click="reconcile">立即调度</el-button>
      </div>
    </div>

    <div v-if="!sessionId.trim()" class="hint">填入会话 ID 查看该会话的自动派发 attempt。</div>
    <el-table v-else :data="attempts" v-loading="loading" size="small" empty-text="暂无自动派发 attempt">
      <el-table-column prop="task_id" label="任务" min-width="130" />
      <el-table-column prop="attempt_no" label="Attempt" width="80" />
      <el-table-column label="状态" width="110">
        <template #default="{ row }">
          <el-tag :type="statusTag(row.status)" size="small">{{ row.status }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="plan_version" label="Plan v" width="80" />
      <el-table-column prop="attempt_id" label="Attempt ID" min-width="150" show-overflow-tooltip />
      <el-table-column prop="agent_id" label="Agent" min-width="150" show-overflow-tooltip />
      <el-table-column prop="last_error" label="错误" min-width="180" show-overflow-tooltip />
      <el-table-column label="预留时间" min-width="150">
        <template #default="{ row }">{{ fmtDateTime(row.reserved_at) }}</template>
      </el-table-column>
      <el-table-column label="结束时间" min-width="150">
        <template #default="{ row }">{{ row.finished_at ? fmtDateTime(row.finished_at) : '-' }}</template>
      </el-table-column>
    </el-table>
  </div>
</template>

<style scoped>
.page { padding: 20px; }
.head { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 16px; flex-wrap: wrap; }
.head h2 { margin: 0; font-size: 18px; }
.controls { display: flex; gap: 8px; align-items: center; }
.sid { width: 260px; }
.hint { color: var(--el-text-color-secondary); font-size: 13px; padding: 18px 0; }
</style>
