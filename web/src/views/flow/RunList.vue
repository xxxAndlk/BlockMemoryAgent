<template>
  <div>
    <div class="flex items-center gap-2 mb-3">
      <el-input v-model="flowIdFilter" size="small" placeholder="按 flow_id 过滤（留空全部）" clearable class="!w-[240px]"
                @change="load" />
      <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="load">
        <el-icon class="mr-1"><Refresh /></el-icon>刷新
      </el-button>
    </div>

    <el-table :data="runs" v-loading="loading" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
      <el-table-column label="运行 ID" min-width="150">
        <template #default="{ row }">
          <span class="font-mono text-xs text-primary">{{ row.id }}</span>
        </template>
      </el-table-column>
      <el-table-column label="流程" min-width="150">
        <template #default="{ row }">
          <span class="text-sm">{{ row.flow_name || row.flow_id }}</span>
          <span class="text-xs text-ink-2 ml-1">v{{ row.flow_version }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="120">
        <template #default="{ row }">
          <el-tag size="small" :type="statusType(row.status)" effect="plain">{{ statusText(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="错误" min-width="180">
        <template #default="{ row }">
          <span v-if="row.error" class="text-xs text-red-400">{{ row.error }}</span>
          <span v-else class="text-xs text-ink-3">—</span>
        </template>
      </el-table-column>
      <el-table-column label="创建时间" width="130">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ fmtDateTime(row.created_at) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="耗时" width="100">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ durationOf(row) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="140" fixed="right">
        <template #default="{ row }">
          <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="openDetail(row)">
            查看
          </el-button>
          <el-button v-if="isActive(row.status)" size="small" plain type="danger" class="!bg-transparent" @click="abort(row)">
            中止
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <div class="text-sm text-ink-2 py-10">还没有流程运行——到流程编辑器里点「试运行」发起一个。</div>
      </template>
    </el-table>

    <!-- 运行详情抽屉：run.outputs + 节点运行表 -->
    <el-drawer v-model="drawerOpen" :title="`运行 ${detail?.run.id || ''}`" size="560px">
      <div v-loading="detailLoading">
        <template v-if="detail">
          <div class="flex items-center gap-2 mb-3">
            <el-tag size="small" :type="statusType(detail.run.status)" effect="plain">{{ statusText(detail.run.status) }}</el-tag>
            <span class="text-xs text-ink-2">{{ detail.run.flow_name }} v{{ detail.run.flow_version }}</span>
          </div>
          <div v-if="detail.run.error" class="text-xs text-red-400 mb-3">错误：{{ detail.run.error }}</div>
          <div class="mb-2 text-sm font-bold text-ink">运行产出</div>
          <pre v-if="detail.run.outputs && Object.keys(detail.run.outputs).length"
               class="text-xs bg-page border border-line rounded p-2 overflow-auto max-h-48 mb-4">{{ JSON.stringify(detail.run.outputs, null, 2) }}</pre>
          <div v-else class="text-xs text-ink-3 mb-4">—</div>
          <div class="mb-2 text-sm font-bold text-ink">节点执行</div>
          <el-table :data="detail.node_runs" size="small" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
            <el-table-column label="节点" min-width="110">
              <template #default="{ row }">
                <span class="font-mono text-xs">{{ row.node_id }}</span>
                <div class="text-[10px] text-ink-3">{{ row.node_type }}</div>
              </template>
            </el-table-column>
            <el-table-column label="状态" width="90">
              <template #default="{ row }">
                <el-tag size="small" :type="nodeStatusType(row.status)" effect="plain">{{ nodeStatusText(row.status) }}</el-tag>
              </template>
            </el-table-column>
            <el-table-column label="产出/错误" min-width="200">
              <template #default="{ row }">
                <span v-if="row.error" class="text-xs text-red-400">{{ row.error }}</span>
                <span v-else-if="row.outputs" class="text-xs text-ink-2 break-all">{{ JSON.stringify(row.outputs) }}</span>
                <span v-else class="text-xs text-ink-3">—</span>
              </template>
            </el-table-column>
          </el-table>
        </template>
      </div>
    </el-drawer>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { listFlowRuns, getFlowRun, abortFlowRun, type FlowRun, type FlowNodeRun } from '@/api/flow'
import { fmtDateTime } from '@/utils/date'

const props = defineProps<{ refreshKey: number }>()

const runs = ref<FlowRun[]>([])
const loading = ref(false)
const flowIdFilter = ref('')
const drawerOpen = ref(false)
const detailLoading = ref(false)
const detail = ref<{ run: FlowRun; node_runs: FlowNodeRun[] } | null>(null)
let timer: ReturnType<typeof setInterval> | null = null

async function load() {
  loading.value = true
  try {
    const res = await listFlowRuns({ flow_id: flowIdFilter.value, limit: 100 })
    runs.value = res.runs || []
  } catch (e) {
    ElMessage.error('读取运行列表失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

function isActive(s: string): boolean {
  return s === 'pending' || s === 'running' || s === 'waiting_approval'
}

function durationOf(row: FlowRun): string {
  if (!row.created_at) return '—'
  const end = row.finished_at || row.updated_at || row.created_at
  try {
    const sec = Math.max(0, Math.round((new Date(end).getTime() - new Date(row.created_at).getTime()) / 1000))
    return sec < 60 ? `${sec}s` : `${Math.floor(sec / 60)}m${sec % 60}s`
  } catch {
    return '—'
  }
}

function statusType(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'primary'
    case 'waiting_approval': return 'warning'
    case 'done': return 'success'
    case 'failed': return 'danger'
    case 'aborted': return 'info'
    default: return 'info' // pending
  }
}
function statusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队中', running: '运行中', waiting_approval: '等待审批', done: '已完成', failed: '失败', aborted: '已中止',
  }
  return map[s] || s
}
function nodeStatusType(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'primary'
    case 'waiting': return 'warning'
    case 'success': return 'success'
    case 'failed': return 'danger'
    default: return 'info' // pending|skipped
  }
}
function nodeStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '等待', running: '运行中', success: '成功', failed: '失败', waiting: '等待审批', skipped: '跳过',
  }
  return map[s] || s
}

async function openDetail(row: FlowRun) {
  drawerOpen.value = true
  detailLoading.value = true
  try {
    detail.value = await getFlowRun(row.id)
  } catch (e) {
    ElMessage.error('读取运行详情失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    detailLoading.value = false
  }
}

async function abort(row: FlowRun) {
  try {
    await ElMessageBox.confirm(`确定中止运行 ${row.id}？`, '中止运行', { type: 'warning' })
  } catch {
    return
  }
  try {
    await abortFlowRun(row.id)
    ElMessage.success('已中止')
    await load()
  } catch (e) {
    ElMessage.error('中止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

watch(() => props.refreshKey, load)
onMounted(() => {
  load()
  // 列表里有未终态 run 时 3s 轮询进度。
  timer = setInterval(() => {
    if (runs.value.some((r) => isActive(r.status))) load()
    // 抽屉开着且 run 未终态时也顺带刷新详情
    if (drawerOpen.value && detail.value && isActive(detail.value.run.status)) refreshDetail()
  }, 3000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})

async function refreshDetail() {
  if (!detail.value) return
  try {
    detail.value = await getFlowRun(detail.value.run.id)
  } catch {
    // 轮询失败静默，下一轮再试
  }
}
</script>
