<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { SessionSummary } from '@/types'
import { listSessions } from '@/api/session'
import { statusTagType, statusText } from '@/utils/sessionStatus'

import { normDir } from '@/utils/dir'
const route = useRoute()
const router = useRouter()

const sessions = ref<SessionSummary[]>([])
const loading = ref(false)
const search = ref('')
const statusFilter = ref('')
// 工作目录页「查看会话」跳入：?work_dir= 预过滤
const workDirFilter = ref((route.query.work_dir as string) || '')

const statusOptions = [
  { value: '', label: '全部状态' },
  { value: 'running', label: '运行中' },
  { value: 'completed', label: '已完成' },
  { value: 'error', label: '失败' },
  { value: 'awaiting_clarify', label: '待澄清' },
  { value: 'paused_on_child', label: '子Agent暂停' },
  { value: 'awaiting_child', label: '挂起等子' },
]

const workDirOptions = computed(() => {
  // 归一化去重：同一目录的不同写法（尾斜杠/盘符大小写）合成一个选项。
  const seen = new Map<string, string>()
  for (const s of sessions.value) {
    const d = s.work_dir || ''
    const k = normDir(d)
    if (!seen.has(k)) seen.set(k, d)
  }
  const dirs = [...seen.values()].sort()
  return [{ value: '', label: '全部目录' }, ...dirs.map((d) => ({ value: d, label: d || '默认目录' }))]
})

const rows = computed(() =>
  sessions.value.filter((s) => {
    if (statusFilter.value && s.status !== statusFilter.value) return false
    // 目录过滤按规范化比较：从工作目录页带 ?work_dir= 跳入时，写法差异不该查成空列表。
    if (workDirFilter.value && normDir(s.work_dir) !== normDir(workDirFilter.value)) return false
    const q = search.value.trim().toLowerCase()
    if (q && !s.id.toLowerCase().includes(q) && !(s.goal || '').toLowerCase().includes(q)) return false
    return true
  })
)

onMounted(async () => {
  loading.value = true
  try {
    sessions.value = await listSessions()
  } catch (e) {
    ElMessage.error('会话历史加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
})

function open(s: SessionSummary) {
  router.push({ path: '/session', query: { id: s.id, view: 'monitor' } })
}

function fmt(t?: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : '—'
}

function duration(s: SessionSummary) {
  if (!s.started_at || !s.ended_at) return '—'
  const ms = new Date(s.ended_at).getTime() - new Date(s.started_at).getTime()
  if (ms < 0) return '—'
  const mm = Math.floor(ms / 60000)
  const ss = Math.floor((ms % 60000) / 1000)
  return mm ? `${mm}m ${ss}s` : `${ss}s`
}
</script>

<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-ink">
    <div class="bg-card border border-line rounded-card p-4 shrink-0 flex items-center gap-3 flex-wrap">
      <div class="font-bold text-sm mr-auto">会话历史</div>
      <el-input v-model="search" size="small" placeholder="搜索目标 / ID…" class="w-56">
        <template #prefix><el-icon class="text-ink-3"><Search /></el-icon></template>
      </el-input>
      <el-select v-model="statusFilter" size="small" class="w-32">
        <el-option v-for="o in statusOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
      <el-select v-model="workDirFilter" size="small" class="w-56">
        <el-option v-for="o in workDirOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
    </div>

    <div class="flex-1 min-h-0 bg-card border border-line rounded-card overflow-hidden">
      <!-- 桌面（≥768px）：表格原样 -->
      <div class="hidden md:block h-full">
        <el-table v-loading="loading" :data="rows" class="w-full" height="100%">
          <el-table-column label="目标" min-width="280">
            <template #default="{ row }">
              <div class="font-bold text-sm truncate">{{ row.goal || '(无目标)' }}</div>
              <div class="text-[11px] text-ink-3 font-mono">{{ row.id }}</div>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="statusTagType(row.status)" effect="plain">{{ statusText(row.status) }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="工作目录" min-width="220">
            <template #default="{ row }">
              <span class="font-mono text-xs text-ink-2">{{ row.work_dir || '默认目录' }}</span>
            </template>
          </el-table-column>
          <el-table-column label="开始时间" width="160">
            <template #default="{ row }"><span class="text-xs text-ink-2">{{ fmt(row.started_at) }}</span></template>
          </el-table-column>
          <el-table-column label="耗时" width="100">
            <template #default="{ row }"><span class="text-xs text-ink-2">{{ duration(row) }}</span></template>
          </el-table-column>
          <el-table-column width="80" align="right">
            <template #default="{ row }">
              <el-button size="small" link type="primary" @click="open(row)">打开</el-button>
            </template>
          </el-table-column>
          <template #empty>
            <div class="text-sm text-ink-3 py-10">暂无会话历史</div>
          </template>
        </el-table>
      </div>

      <!-- 小屏（<768px，T33）：卡片化兜底——六列表格在手机上只剩横向滚动，
           切成单列卡片（目标/状态/目录/时间堆叠），整卡可点打开 -->
      <div v-loading="loading" class="md:hidden h-full overflow-y-auto">
        <button
          v-for="row in rows"
          :key="row.id"
          class="w-full text-left px-4 py-3 border-b border-line last:border-b-0 hover:bg-page transition-colors"
          @click="open(row)"
        >
          <div class="flex items-start gap-2">
            <div class="flex-1 min-w-0">
              <div class="font-bold text-sm leading-5 line-clamp-2">{{ row.goal || '(无目标)' }}</div>
              <div class="text-[11px] text-ink-3 font-mono truncate mt-0.5">{{ row.id }}</div>
            </div>
            <el-tag size="small" :type="statusTagType(row.status)" effect="plain" class="shrink-0 mt-0.5">
              {{ statusText(row.status) }}
            </el-tag>
          </div>
          <div class="flex items-center gap-2 mt-1.5 text-xs text-ink-3">
            <span class="font-mono truncate min-w-0" :title="row.work_dir || ''">{{ row.work_dir || '默认目录' }}</span>
            <span class="ml-auto shrink-0">{{ fmt(row.started_at) }}</span>
            <span v-if="duration(row) !== '—'" class="shrink-0">· {{ duration(row) }}</span>
          </div>
        </button>
        <div v-if="!loading && rows.length === 0" class="text-sm text-ink-3 py-10 text-center">暂无会话历史</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 小屏卡片目标两行截断（项目 Tailwind 版本未含 line-clamp，各视图自定义） */
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
