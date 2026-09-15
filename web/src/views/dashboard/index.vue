<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { SessionStatus, SessionSummary } from '@/types'
import { listSessions, createSession, deleteSession, deleteSessions } from '@/api/session'
import { getTimeline, getActivity, type TimelinePoint, type ActivityItem } from '@/api/metrics'
import { statusDotClass, statusText } from '@/utils/sessionStatus'
import { fmtDate } from '@/utils/date'
import { kindIcon, kindTagType } from '@/views/session/chat/utils/eventStyles'
import { useWorkDir } from '@/composables/useWorkDir'
import WorkDirPicker from '@/components/WorkDirPicker.vue'

const router = useRouter()
const goal = ref('')
const { workDir, setWorkDir } = useWorkDir()
const searchSession = ref('')
const sessions = ref<SessionSummary[]>([])
const loading = ref(false)
const filter = ref('all')
const usingMock = ref(false)

// 列表条数与后端上限（listSessionsMaxLimit=1000）对齐：首页的计数/分页/搜索都是
// 客户端口径，只取默认 200 条会让删掉的会话被更旧的行顶上来——总数看着纹丝不动，
// 用户以为"删除没生效"（2026-09-15 实证）。
const SESSION_LIST_LIMIT = 1000
const listCapped = ref(false)

const timeline = ref<TimelinePoint[]>([])
const activities = ref<ActivityItem[]>([])

onMounted(load)

async function load() {
  loading.value = true
  try {
    const [sessionsRes, timelineRes, activityRes] = await Promise.all([
      listSessions(SESSION_LIST_LIMIT).catch(() => null),
      getTimeline(12).catch(() => null),
      getActivity(8).catch(() => null),
    ])
    if (sessionsRes) {
      // 成功后必须清 mock 标记：否则后端恢复可用仍走"示例数据"分支（删除只改本地、不发请求）。
      sessions.value = sessionsRes
      usingMock.value = false
      listCapped.value = sessionsRes.length >= SESSION_LIST_LIMIT
    } else {
      usingMock.value = true
      sessions.value = mockSessions()
    }
    timeline.value = timelineRes?.points || []
    activities.value = activityRes?.activities || []
  } finally {
    loading.value = false
  }
}

function mockSessions(): SessionSummary[] {
  return [
    { id: 's-1', goal: '生成项目架构设计文档', status: 'running', started_at: new Date(Date.now() - 3600000).toISOString() },
    { id: 's-2', goal: '排查 Redis 连接池告警', status: 'completed', started_at: new Date(Date.now() - 1800000).toISOString() },
    { id: 's-3', goal: '编写 v3 接口测试用例', status: 'completed', started_at: new Date(Date.now() - 7200000).toISOString() },
    { id: 's-4', goal: '重构 Skill 注册逻辑', status: 'error', started_at: new Date(Date.now() - 10800000).toISOString() },
    { id: 's-5', goal: '演练 Chaos 自动回滚', status: 'completed', started_at: new Date(Date.now() - 14400000).toISOString() },
    { id: 's-6', goal: '分析日志异常模式', status: 'running', started_at: new Date(Date.now() - 15000000).toISOString() },
  ]
}

async function runSession() {
  if (!goal.value.trim()) return
  loading.value = true
  try {
    const s = await createSession(goal.value.trim(), undefined, workDir.value || undefined)
    setWorkDir(workDir.value)
    goal.value = ''
    await load()
    if (s?.id) viewSession(s.id)
  } catch (e) {
    ElMessage.error('创建会话失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// Enter 直接发送；Shift+Enter 换行（默认行为）；中文输入法组词回车不发送
function onGoalKeydown(e: KeyboardEvent) {
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    runSession()
  }
}

function viewSession(id: string) {
  router.push({ path: '/session', query: { id } })
}

// ---- 删除会话（硬删，不可恢复）----
const selectedIds = ref<string[]>([])
const deleting = ref(false)

function toggleSelect(id: string) {
  const i = selectedIds.value.indexOf(id)
  if (i >= 0) selectedIds.value.splice(i, 1)
  else selectedIds.value.push(id)
}

// 「全选」作用于**当前页**：列表分页后"全选"只对看得见的这 10 条生效，
// 否则一次全选会静默勾中全部会话（搜索/状态页签过滤后也一样），配合"删除选中"极易误删。
// 已选集合跨页保留——"删除选中(N)"仍显示真实总数。
const allSelected = computed(
  () => pagedSessions.value.length > 0 && pagedSessions.value.every((s) => selectedIds.value.includes(s.id)),
)
const someSelected = computed(
  () => !allSelected.value && pagedSessions.value.some((s) => selectedIds.value.includes(s.id)),
)

function toggleSelectAll() {
  if (allSelected.value) {
    const ids = new Set(pagedSessions.value.map((s) => s.id))
    selectedIds.value = selectedIds.value.filter((id) => !ids.has(id))
  } else {
    const merged = new Set(selectedIds.value)
    pagedSessions.value.forEach((s) => merged.add(s.id))
    selectedIds.value = [...merged]
  }
}

async function confirmDelete(ids: string[]) {
  if (ids.length === 0 || deleting.value) return
  try {
    await ElMessageBox.confirm(
      `将永久删除 ${ids.length} 个会话（含历史、事件、日志，不可恢复）。运行中的会话会先被终止。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
    )
  } catch {
    return // 用户取消
  }
  if (usingMock.value) {
    // 后端不可用（示例数据）：仅本地移除，不发请求。
    sessions.value = sessions.value.filter((s) => !ids.includes(s.id))
    selectedIds.value = []
    ElMessage.success(`已移除 ${ids.length} 个示例会话`)
    return
  }
  deleting.value = true
  try {
    let failed = 0
    if (ids.length === 1) {
      try {
        await deleteSession(ids[0])
      } catch {
        failed = 1
      }
    } else {
      const res = await deleteSessions(ids)
      failed = res.errors.length
    }
    selectedIds.value = []
    await load()
    if (failed > 0) ElMessage.error(`删除完成：${ids.length - failed} 成功，${failed} 失败`)
    else ElMessage.success(`已删除 ${ids.length} 个会话`)
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    deleting.value = false
  }
}

// 状态分档：运行中把"挂起等子/暂停于子"一并算上（对用户都是"还在跑"），
// 待澄清单列（等的是人，不是机器）。旧页签里的"已暂停"从来没统计过（后端无 paused 状态），
// 数字恒为 0，已由"待澄清"取代。
const RUNNING_STATUSES: SessionStatus[] = ['running', 'awaiting_child', 'paused_on_child']
const FILTER_STATUSES: Record<string, SessionStatus[]> = {
  running: RUNNING_STATUSES,
  clarify: ['awaiting_clarify'],
  completed: ['completed'],
  failed: ['error'],
}
const FILTERS = [
  { key: 'all', label: '全部' },
  { key: 'running', label: '运行中' },
  { key: 'clarify', label: '待澄清' },
  { key: 'completed', label: '已完成' },
  { key: 'failed', label: '已失败' },
] as const

const filteredSessions = computed(() => {
  let list = sessions.value
  const q = searchSession.value.trim().toLowerCase()
  if (q) {
    list = list.filter(
      (s) => (s.goal || '').toLowerCase().includes(q) || (s.work_dir || '').toLowerCase().includes(q),
    )
  }
  const want = FILTER_STATUSES[filter.value]
  return want ? list.filter((s) => want.includes(s.status)) : list
})

const counts = computed(() => {
  const total = sessions.value.length
  const by = (sts: SessionStatus[]) => sessions.value.filter((s) => sts.includes(s.status)).length
  return {
    all: total,
    running: by(RUNNING_STATUSES),
    clarify: by(['awaiting_clarify']),
    completed: by(['completed']),
    failed: by(['error']),
  }
})

// 会话列表分页：列表按页切片渲染（此前 el-pagination 只绑了 :total、列表渲染全量，
// 点页码只改分页器自身状态，看起来"点了没用"）。
const PAGE_SIZE = 10
const currentPage = ref(1)
const pagedSessions = computed(() => {
  const start = (currentPage.value - 1) * PAGE_SIZE
  return filteredSessions.value.slice(start, start + PAGE_SIZE)
})
const pageCount = computed(() => Math.max(1, Math.ceil(filteredSessions.value.length / PAGE_SIZE)))

// 过滤条件/搜索变化 → 回到第 1 页；数据减少（删除后）→ 把越界的页码收回来，
// 否则会停在一个空页上（"共 N 条会话"却一条都不显示）。
watch([() => filter.value, searchSession], () => { currentPage.value = 1 })
watch(filteredSessions, () => {
  if (currentPage.value > pageCount.value) currentPage.value = pageCount.value
})

// 耗时：运行中算到此刻，已结束算到 ended_at（旧的"进度 35%"是写死的假数据，已删）。
// 注意只存库的历史会话没有真实结束时间——后端用 created_at 占位，差值 ≤0 时留白，
// 别显示成"耗时 0s"。
function fmtElapsed(ms: number) {
  const sec = Math.round(ms / 1000)
  if (sec < 60) return `${sec}s`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ${sec % 60}s`
  const h = Math.floor(min / 60)
  return `${h}h ${min % 60}m`
}

function sessionDuration(s: SessionSummary) {
  const start = new Date(s.started_at).getTime()
  if (!Number.isFinite(start)) return '—'
  if (s.ended_at) {
    const end = new Date(s.ended_at).getTime()
    if (!Number.isFinite(end) || end <= start) return '—'
    return fmtElapsed(end - start)
  }
  return fmtElapsed(Date.now() - start)
}

const stats = computed(() => {
  const total = sessions.value.length
  const completed = counts.value.completed
  const calls = timeline.value.reduce((n, p) => n + (p.calls || 0), 0)
  const tokens = timeline.value.reduce((n, p) => n + (p.tokens || 0), 0)
  return {
    total,
    rate: total > 0 ? Math.round((completed / total) * 100) : 0,
    running: counts.value.running,
    calls,
    tokens,
  }
})

const trendValues = computed(() => {
  if (!timeline.value.length) return Array(12).fill(0)
  return timeline.value.map(p => p.tokens || p.calls || 0)
})

const tagBgMap: Record<string, string> = {
  success: 'bg-green-900/30',
  warning: 'bg-yellow-900/30',
  danger: 'bg-red-900/30',
  primary: 'bg-blue-900/30',
  info: 'bg-page',
}

const tagIconColorMap: Record<string, string> = {
  success: 'text-green-400',
  warning: 'text-yellow-400',
  danger: 'text-red-400',
  primary: 'text-blue-400',
  info: 'text-ink-2',
}

function activityStyle(kind: string) {
  const tag = kindTagType(kind)
  return {
    icon: kindIcon(kind),
    bgClass: tagBgMap[tag] || tagBgMap.info,
    iconClass: tagIconColorMap[tag] || tagIconColorMap.info,
  }
}
</script>

<template>
  <div class="h-full flex gap-6 overflow-hidden">
    <!-- Left Column -->
    <div class="flex-1 flex flex-col gap-6 min-w-0 overflow-y-auto pr-2">
      <!-- Mock data warning -->
      <div v-if="usingMock" class="bg-amber-50 border border-amber-300 rounded-lg px-4 py-3 text-sm text-amber-700 dark:bg-yellow-900/30 dark:border-yellow-700/50 dark:text-yellow-300 flex items-center gap-2">
        <el-icon><WarningFilled /></el-icon>
        <span>后端不可用，当前显示为示例数据</span>
      </div>

      <!-- Create Session -->
      <el-card class="!border-line !bg-card">
        <template #header>
          <div class="font-bold text-sm text-ink">创建新会话</div>
        </template>
        <div class="flex">
          <div class="flex-1 pr-6 space-y-4">
            <div class="text-xs text-ink-2">输入你的目标，我们将为你规划并执行任务</div>
            <el-input
              v-model="goal"
              type="textarea"
              :rows="3"
              placeholder="例如：分析 gin 项目的架构并生成设计文档（Enter 发送，Shift+Enter 换行）"
              class="w-full bg-page border-none"
              @keydown="onGoalKeydown"
            />
            <WorkDirPicker v-model="workDir" placeholder="进程默认目录（可更改）" />
          </div>
          <div class="w-48 h-32 flex flex-col items-center justify-center shrink-0 gap-3">
            <svg class="w-28 h-28" viewBox="0 0 140 140" fill="none">
              <defs>
                <linearGradient id="g1" x1="0" y1="0" x2="1" y2="1">
                  <stop offset="0%" stop-color="#3b82f6" />
                  <stop offset="100%" stop-color="#1d4ed8" />
                </linearGradient>
                <linearGradient id="g2" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="#60a5fa" />
                  <stop offset="100%" stop-color="#2563eb" />
                </linearGradient>
                <linearGradient id="g3" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="#93c5fd" />
                  <stop offset="100%" stop-color="#3b82f6" />
                </linearGradient>
              </defs>
              <g opacity="0.95">
                <path d="M70 10L130 44V98L70 132L10 98V44L70 10Z" fill="url(#g1)" />
                <path d="M70 10L130 44L70 78L10 44L70 10Z" fill="url(#g2)" />
                <path d="M70 78V132L130 98V44L70 78Z" fill="url(#g3)" />
                <path d="M70 78L10 44V98L70 132V78Z" fill="#1e40af" fill-opacity="0.6" />
                <circle cx="70" cy="78" r="8" fill="#bfdbfe" />
                <circle cx="70" cy="78" r="3" fill="#1e3a8a" />
              </g>
            </svg>
            <el-button type="primary" class="!bg-primary w-40" :loading="loading" @click="runSession">
              <el-icon class="mr-2"><Promotion /></el-icon> Run Session
            </el-button>
          </div>
        </div>
      </el-card>

      <!-- Session List -->
      <!-- 卡片按内容高度排布（去掉 flex-1）：列表不再内滚，改由外层页面滚动，
           保留 min-h 避免空结果时卡片塌成一条。 -->
      <el-card class="!border-line !bg-card flex flex-col min-h-[400px]">
        <template #header>
          <div class="flex justify-between items-center gap-3">
            <div class="font-bold text-sm text-ink shrink-0">会话列表</div>
            <el-input v-model="searchSession" size="small" placeholder="搜索会话或目录..." class="w-56 !bg-page">
              <template #suffix><el-icon><Search /></el-icon></template>
            </el-input>
          </div>
        </template>

        <!-- Tabs + 批量操作 -->
        <div class="flex gap-2 mb-4 items-center flex-wrap">
          <el-button
            v-for="f in FILTERS"
            :key="f.key"
            size="small"
            :type="filter===f.key ? 'primary' : ''"
            :class="filter===f.key ? '!bg-primary !border-none !text-white' : '!bg-transparent !border-none !text-ink-2 hover:!text-ink'"
            @click="filter=f.key"
          >
            {{ f.label }} {{ counts[f.key] }}
          </el-button>
          <div class="ml-auto flex items-center gap-2 shrink-0">
            <!-- 本页全选：分页后只勾当前页这 10 条（范围写在按钮上，不再用含义模糊的"全选"）。
                 已选集合跨页保留，"删除选中(N)"显示真实总数。 -->
            <el-button
              size="small"
              :type="allSelected ? 'primary' : ''"
              :plain="!allSelected"
              :disabled="pagedSessions.length === 0"
              :title="allSelected ? '取消本页选择的 ' + pagedSessions.length + ' 条' : '选中本页 ' + pagedSessions.length + ' 条'"
              @click="toggleSelectAll"
            >
              <el-icon class="mr-1"><Check /></el-icon>
              {{ allSelected ? '取消本页' : '本页全选' }}
              <span v-if="someSelected" class="ml-1 text-[10px] opacity-70">({{ pagedSessions.filter((s) => selectedIds.includes(s.id)).length }}/{{ pagedSessions.length }})</span>
            </el-button>
            <el-button
              v-if="selectedIds.length > 0"
              size="small"
              type="danger"
              :loading="deleting"
              @click="confirmDelete([...selectedIds])"
            >
              <el-icon class="mr-1"><Delete /></el-icon>删除选中 ({{ selectedIds.length }})
            </el-button>
          </div>
        </div>

        <!-- 列表不再自带滚动条：分页后每页固定 10 条，滚轮只滚页面本身——
             此前内层 overflow-y-auto 与外层页面各有一个滚动条，滚轮滚的是内层，
             和翻页语义打架（用户："滚轮和翻页冲突了，只保留翻页"）。 -->
        <div class="space-y-3">
          <div
            v-for="s in pagedSessions"
            :key="s.id"
            class="p-3 bg-page rounded border flex items-center justify-between group cursor-pointer transition-colors"
            :class="selectedIds.includes(s.id) ? 'border-primary ring-1 ring-primary/40' : 'border-line hover:border-primary'"
            @click="viewSession(s.id)"
          >
            <span class="mr-3 shrink-0" @click.stop>
              <el-checkbox
                :model-value="selectedIds.includes(s.id)"
                @change="toggleSelect(s.id)"
              />
            </span>
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-2 mb-1 min-w-0">
                <span class="font-bold text-sm text-ink truncate">{{ s.goal }}</span>
                <span class="shrink-0 inline-flex items-center gap-1.5 text-xs text-ink-2 whitespace-nowrap">
                  <span class="w-1.5 h-1.5 rounded-full" :class="statusDotClass(s.status)"></span>
                  {{ statusText(s.status) }}
                </span>
              </div>
              <div class="text-xs text-ink-2 truncate">
                {{ fmtDate(s.started_at) }}
                <span class="mx-1 text-ink-3">·</span>
                {{ s.work_dir || '默认目录' }}
              </div>
            </div>
            <div class="w-40 px-4 text-right shrink-0">
              <!-- 只存库的历史会话没有结束时间（后端用 created_at 占位），此时整格留白而不是写"耗时 —" -->
              <div v-if="sessionDuration(s) !== '—'" class="text-xs text-ink-2 whitespace-nowrap">
                耗时 {{ sessionDuration(s) }}
              </div>
              <div v-if="s.status === 'running'" class="text-[11px] text-blue-400 mt-0.5">执行中</div>
            </div>

            <div class="w-20 text-right text-xs text-ink-2 flex items-center justify-end gap-2 shrink-0">
              <el-button
                link
                type="danger"
                size="small"
                class="!p-1 opacity-0 group-hover:opacity-100 transition-opacity"
                :disabled="deleting"
                title="删除会话"
                @click.stop="confirmDelete([s.id])"
              >
                <el-icon><Delete /></el-icon>
              </el-button>
              <el-icon class="text-ink-3 group-hover:text-primary"><ArrowRight /></el-icon>
            </div>
          </div>

          <div v-if="!pagedSessions.length" class="text-xs text-ink-2 text-center py-10">
            {{ searchSession.trim() ? '没有匹配的会话' : '暂无会话' }}
          </div>
        </div>

        <div class="mt-4 flex justify-between items-center text-xs text-ink-2 gap-3 flex-wrap">
          <span>
            共 {{ filteredSessions.length }} 条会话
            <span v-if="listCapped" class="text-ink-3">（仅显示最近 {{ SESSION_LIST_LIMIT }} 条，更早的未加载）</span>
          </span>
          <el-pagination v-model:current-page="currentPage" small background layout="prev, pager, next"
                         :total="filteredSessions.length" :page-size="PAGE_SIZE" class="!p-0" />
        </div>
      </el-card>
    </div>

    <!-- Right Column -->
    <div class="w-[360px] flex flex-col gap-6 shrink-0 overflow-y-auto">
      <!-- Stats -->
      <el-card class="!border-line !bg-card">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-ink">统计概览</div>
            <el-button link type="primary" size="small" @click="load">刷新 <el-icon><Refresh /></el-icon></el-button>
          </div>
        </template>

        <div class="grid grid-cols-2 gap-4 mb-6">
          <div class="p-3 bg-page rounded border border-line relative overflow-hidden">
            <div class="text-xs text-ink-2 mb-1">会话总数</div>
            <div class="text-2xl font-bold text-ink">{{ listCapped ? SESSION_LIST_LIMIT + '+' : stats.total }}</div>
            <div class="text-xs text-ink-2 mt-1">运行中 {{ stats.running }} · 待澄清 {{ counts.clarify }}</div>
          </div>

          <div class="p-3 bg-page rounded border border-line relative overflow-hidden">
            <div class="text-xs text-ink-2 mb-1">完成率</div>
            <div class="text-2xl font-bold text-ink">{{ stats.rate }}%</div>
            <div class="text-xs text-ink-2 mt-1">已完成 {{ counts.completed }} / {{ stats.total }}</div>
          </div>

          <div class="p-3 bg-page rounded border border-line relative overflow-hidden">
            <div class="text-xs text-ink-2 mb-1">近 12 小时调用</div>
            <div class="text-2xl font-bold text-ink">{{ stats.calls.toLocaleString() }}</div>
            <div class="text-xs text-ink-2 mt-1">LLM 请求次数</div>
          </div>

          <div class="p-3 bg-page rounded border border-line relative overflow-hidden">
            <div class="text-xs text-ink-2 mb-1">近 12 小时 Token</div>
            <div class="text-2xl font-bold text-ink">{{ stats.tokens.toLocaleString() }}</div>
            <div class="text-xs text-ink-2 mt-1">输入 + 输出</div>
          </div>
        </div>

        <div>
          <div class="flex justify-between text-xs text-ink-2 mb-2">
            <span>Token 消耗趋势 (最近 12 小时)</span>
            <span>单位: tokens</span>
          </div>
          <div class="h-40 bg-page rounded-card border border-line p-3">
            <svg v-if="trendValues.length > 1" class="w-full h-full text-primary" viewBox="0 0 100 40" preserveAspectRatio="none" fill="none" stroke="currentColor" stroke-width="1.5">
              <path :d="`M0 ${40 - trendValues[0]/40} ${trendValues.slice(1).map((v,i)=>`L${(i+1)*100/(trendValues.length-1)} ${40 - v/40}`).join(' ')}`" />
              <circle v-for="(v,i) in trendValues" :key="i" :cx="i*100/(trendValues.length-1)" :cy="40 - v/40" r="1.2" fill="currentColor" />
            </svg>
            <div v-else class="h-full flex items-center justify-center text-xs text-ink-2">暂无趋势数据</div>
          </div>
        </div>
      </el-card>

      <!-- Recent Activity -->
      <el-card class="!border-line !bg-card flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-ink">最近活动</div>
          </div>
        </template>

        <div class="space-y-4">
          <div v-for="(activity, idx) in activities" :key="idx" class="flex items-start gap-3 text-sm">
            <div class="mt-0.5 rounded-full p-1 shrink-0" :class="activityStyle(activity.kind).bgClass">
              <el-icon :class="activityStyle(activity.kind).iconClass"><component :is="activityStyle(activity.kind).icon" /></el-icon>
            </div>
            <div class="flex-1 min-w-0">
              <div class="text-ink break-words line-clamp-2">
                <span class="text-ink-2 mr-1" v-if="activity.agent">{{ activity.agent }}</span>
                <span class="text-ink-2 mr-1">[{{ activity.kind }}]</span>
                {{ activity.content }}
              </div>
            </div>
            <div class="text-xs text-ink-2 shrink-0 whitespace-nowrap">{{ activity.time }}</div>
          </div>
          <div v-if="!activities.length" class="text-xs text-ink-2 text-center py-4">暂无活动</div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<style scoped>
/* 说明：原 .body-flex-1（让卡片 body 撑满并内滚）已随"列表改由页面滚动"移除，
   否则卡片仍会被压成固定高度、10 条列表在内部被裁掉。 */
:deep(.el-textarea__inner) {
  background-color: transparent;
  box-shadow: none !important;
  color: var(--bma-text);
}
:deep(.el-textarea__inner:focus) {
  box-shadow: none !important;
}
:deep(.el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
}
:deep(.el-pagination.is-background .el-pager li:not(.is-disabled).is-active) {
  background-color: var(--bma-primary);
}
</style>
