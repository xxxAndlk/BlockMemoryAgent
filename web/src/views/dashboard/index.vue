<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import type { Session } from '@/types'
import { listSessions, createSession, getTimeline, getActivity, type TimelinePoint, type ActivityItem } from '@/api/session'

const router = useRouter()
const goal = ref('')
const searchSession = ref('')
const sessions = ref<Session[]>([])
const loading = ref(false)
const filter = ref('all')


const quickTags = ['系统架构设计', '代码审查', '接口测试', '混沌演练', '根因分析', '生成周报']

const timeline = ref<TimelinePoint[]>([])
const activities = ref<ActivityItem[]>([])

onMounted(load)

async function load() {
  loading.value = true
  try {
    const [sessionsRes, timelineRes, activityRes] = await Promise.all([
      listSessions().catch(() => null),
      getTimeline(12).catch(() => null),
      getActivity(8).catch(() => null),
    ])
    sessions.value = sessionsRes || mockSessions()
    timeline.value = timelineRes?.points || []
    activities.value = activityRes?.activities || []
  } finally {
    loading.value = false
  }
}

function mockSessions(): Session[] {
  return [
    { id: 's-1', goal: '生成项目架构设计文档', status: 'running', started_at: new Date(Date.now() - 3600000).toISOString(), events: [], messages: [] },
    { id: 's-2', goal: '排查 Redis 连接池告警', status: 'completed', started_at: new Date(Date.now() - 1800000).toISOString(), events: [], messages: [] },
    { id: 's-3', goal: '编写 v3 接口测试用例', status: 'completed', started_at: new Date(Date.now() - 7200000).toISOString(), events: [], messages: [] },
    { id: 's-4', goal: '重构 Skill 注册逻辑', status: 'error', started_at: new Date(Date.now() - 10800000).toISOString(), events: [], messages: [] },
    { id: 's-5', goal: '演练 Chaos 自动回滚', status: 'completed', started_at: new Date(Date.now() - 14400000).toISOString(), events: [], messages: [] },
    { id: 's-6', goal: '分析日志异常模式', status: 'running', started_at: new Date(Date.now() - 15000000).toISOString(), events: [], messages: [] },
  ]
}

async function runSession() {
  if (!goal.value.trim()) return
  loading.value = true
  try {
    const s = await createSession(goal.value.trim())
    goal.value = ''
    await load()
    if (s?.id) viewSession(s.id)
  } finally {
    loading.value = false
  }
}

function quickStart(g: string) {
  goal.value = g
  runSession()
}

function viewSession(id: string) {
  router.push({ path: '/session', query: { id } })
}

function fmtDate(iso: string) {
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function statusType(status: string) {
  switch (status) {
    case 'running': return 'primary'
    case 'completed': return 'success'
    case 'error': return 'danger'
    default: return 'info'
  }
}

function statusLabel(status: string) {
  const m: Record<string, string> = { running: '运行中', completed: '已完成', error: '失败', idle: '待处理' }
  return m[status] || status
}

function progressOf(s: Session) {
  if (s.status === 'completed') return 100
  if (s.status === 'error') return 0
  return 35
}

const filteredSessions = computed(() => {
  let list = sessions.value
  const q = searchSession.value.trim().toLowerCase()
  if (q) list = list.filter(s => s.goal.toLowerCase().includes(q))
  if (filter.value === 'all') return list
  const map: Record<string, string> = { running: 'running', completed: 'completed', failed: 'error', paused: 'paused' }
  const want = map[filter.value]
  if (!want) return list
  return list.filter(s => s.status === want)
})

const counts = computed(() => {
  const total = sessions.value.length
  return {
    all: total,
    running: sessions.value.filter(s => s.status === 'running').length,
    completed: sessions.value.filter(s => s.status === 'completed').length,
    failed: sessions.value.filter(s => s.status === 'error').length,
    paused: 0,
  }
})

const stats = computed(() => {
  const total = sessions.value.length
  const completed = sessions.value.filter(s => s.status === 'completed').length
  const rate = total > 0 ? Math.round((completed / total) * 100) : 0
  let calls = 0, tokens = 0, timeouts = 0
  sessions.value.forEach(s => {
    (s.events || []).forEach(ev => {
      if (ev.kind === 'token_usage') {
        calls++
        tokens += (ev.input_tokens || 0) + (ev.output_tokens || 0)
      }
      if (ev.kind === 'error' && (ev.message?.toLowerCase().includes('timeout') || ev.message?.includes('超时'))) {
        timeouts++
      }
    })
  })
  const timeoutRate = calls > 0 ? parseFloat(((timeouts / calls) * 100).toFixed(1)) : 0
  return {
    total,
    rate,
    calls,
    timeout: timeoutRate,
    tokens,
  }
})

const trendValues = computed(() => {
  if (!timeline.value.length) return Array(12).fill(0)
  return timeline.value.map(p => p.tokens || p.calls || 0)
})

function activityStyle(kind: string) {
  switch (kind) {
    case 'tool_call': return { icon: 'Connection', bgClass: 'bg-blue-900/30', iconClass: 'text-blue-400' }
    case 'tool_result': return { icon: 'CircleCheck', bgClass: 'bg-green-900/30', iconClass: 'text-green-400' }
    case 'error': return { icon: 'WarningFilled', bgClass: 'bg-red-900/30', iconClass: 'text-red-400' }
    case 'agent_created': return { icon: 'UserFilled', bgClass: 'bg-purple-900/30', iconClass: 'text-purple-400' }
    case 'token_usage': return { icon: 'Coin', bgClass: 'bg-yellow-900/30', iconClass: 'text-yellow-400' }
    case 'milestone': return { icon: 'Trophy', bgClass: 'bg-green-900/30', iconClass: 'text-green-400' }
    default: return { icon: 'InfoFilled', bgClass: 'bg-gray-800/50', iconClass: 'text-gray-400' }
  }
}
</script>

<template>
  <div class="h-full flex gap-6 overflow-hidden">
    <!-- Left Column -->
    <div class="flex-1 flex flex-col gap-6 min-w-0 overflow-y-auto pr-2">
      <!-- Create Session -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="font-bold text-sm text-gray-200">创建新会话</div>
        </template>
        <div class="flex">
          <div class="flex-1 pr-6 space-y-4">
            <div class="text-xs text-gray-400">输入你的目标，我们将为你规划并执行任务</div>
            <el-input
              v-model="goal"
              type="textarea"
              :rows="3"
              placeholder="例如：分析 gin 项目的架构并生成设计文档"
              class="w-full bg-[#0f1115] border-none"
              @keydown.enter.prevent="runSession"
            />
            <div class="flex flex-wrap items-center gap-4">
              <span class="text-sm text-gray-400">快捷模板:</span>
              <div class="flex flex-wrap gap-2">
                <el-tag
                  v-for="q in quickTags"
                  :key="q"
                  effect="plain"
                  class="!bg-transparent !border-[#2a2d35] !text-gray-300 cursor-pointer hover:!border-primary"
                  @click="quickStart(q)"
                >{{ q }}</el-tag>
              </div>
            </div>
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
      <el-card class="flex-1 !border-[#2a2d35] !bg-[#1a1d24] flex flex-col body-flex-1 min-h-[400px]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">会话列表</div>
            <el-input v-model="searchSession" size="small" placeholder="搜索会话..." class="w-48 !bg-[#0f1115]">
              <template #suffix><el-icon><Search /></el-icon></template>
            </el-input>
          </div>
        </template>

        <!-- Tabs -->
        <div class="flex gap-2 mb-4">
          <el-button
            v-for="f in ['all','running','completed','failed','paused']"
            :key="f"
            size="small"
            :type="filter===f ? 'primary' : ''"
            :class="filter===f ? '!bg-[#1e3a8a] !border-none !text-white' : '!bg-transparent !border-none !text-gray-400 hover:!text-gray-200'"
            @click="filter=f"
          >
            {{ {all:'全部', running:'运行中', completed:'已完成', failed:'已失败', paused:'已暂停'}[f] }} {{ counts[f as keyof typeof counts] }}
          </el-button>
        </div>

        <div class="space-y-3 flex-1 overflow-y-auto">
          <div
            v-for="s in filteredSessions"
            :key="s.id"
            class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] flex items-center justify-between group hover:border-primary transition-colors cursor-pointer"
            @click="viewSession(s.id)"
          >
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-3 mb-1">
                <span class="font-bold text-sm text-gray-200 truncate">{{ s.goal }}</span>
                <el-tag :type="statusType(s.status)" size="small" effect="plain" class="!bg-transparent !border-none px-0"
                >
                  {{ statusLabel(s.status) }} <span class="ml-1" :class="s.status==='running'?'text-blue-500':s.status==='completed'?'text-green-500':'text-red-500'">●</span>
                </el-tag>
              </div>
              <div class="text-xs text-gray-500">创建时间: {{ fmtDate(s.started_at) }}</div>
            </div>
            <div class="w-48 px-4 flex flex-col items-end">
              <div v-if="s.status === 'running'" class="w-full flex items-center gap-2">
                <span class="text-xs text-gray-400 whitespace-nowrap">进度 {{ progressOf(s) }}%</span>
                <el-progress :percentage="progressOf(s)" :show-text="false" class="flex-1" />
              </div>
              <div v-else-if="s.status === 'completed'" class="w-full flex items-center gap-2">
                <span class="text-xs text-gray-400 whitespace-nowrap">100%</span>
                <el-progress :percentage="100" :show-text="false" status="success" class="flex-1" />
              </div>
              <div v-else class="text-xs text-gray-400">-</div>
            </div>

            <div class="w-20 text-right text-xs text-gray-500 flex items-center justify-end gap-2">
              <el-icon class="text-gray-600 group-hover:text-primary"><ArrowRight /></el-icon>
            </div>
          </div>
        </div>

        <div class="mt-4 flex justify-between items-center text-xs text-gray-500">
          <span>共 {{ filteredSessions.length }} 条会话</span>
          <el-pagination small background layout="prev, pager, next" :total="filteredSessions.length" class="!p-0" />
        </div>
      </el-card>
    </div>

    <!-- Right Column -->
    <div class="w-[400px] flex flex-col gap-6 shrink-0 overflow-y-auto">
      <!-- Stats -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">统计概览 (今日)</div>
            <el-button link type="primary" size="small">查看更多 <el-icon><ArrowRight /></el-icon></el-button>
          </div>
        </template>

        <div class="grid grid-cols-2 gap-4 mb-6">
          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">会话总数</div>
            <div class="text-2xl font-bold text-gray-200">{{ stats.total }}</div>
            <div class="text-xs text-gray-500 mt-1">当前内存中的会话</div>
          </div>

          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">完成率</div>
            <div class="text-2xl font-bold text-gray-200">{{ stats.rate }}%</div>
            <div class="text-xs text-gray-500 mt-1">已完成 / 总数</div>
          </div>

          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">LLM 调用总数</div>
            <div class="text-2xl font-bold text-gray-200">{{ stats.calls.toLocaleString() }}</div>
            <div class="text-xs text-gray-500 mt-1">累计 Token: {{ stats.tokens.toLocaleString() }}</div>
          </div>

          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">超时率</div>
            <div class="text-2xl font-bold text-gray-200">{{ stats.timeout }}%</div>
            <div class="text-xs text-gray-500 mt-1">按 token_usage 事件估算</div>
          </div>
        </div>

        <div>
          <div class="flex justify-between text-xs text-gray-400 mb-2">
            <span>Token 消耗趋势 (最近 12 小时)</span>
            <span>单位: tokens</span>
          </div>
          <div class="h-40 bg-[#0f1115] rounded border border-[#2a2d35] p-3">
            <svg v-if="trendValues.length > 1" class="w-full h-full text-blue-500" viewBox="0 0 100 40" preserveAspectRatio="none" fill="none" stroke="currentColor" stroke-width="1.5">
              <path :d="`M0 ${40 - trendValues[0]/40} ${trendValues.slice(1).map((v,i)=>`L${(i+1)*100/(trendValues.length-1)} ${40 - v/40}`).join(' ')}`" />
              <circle v-for="(v,i) in trendValues" :key="i" :cx="i*100/(trendValues.length-1)" :cy="40 - v/40" r="1.2" fill="currentColor" />
            </svg>
            <div v-else class="h-full flex items-center justify-center text-xs text-gray-500">暂无趋势数据</div>
          </div>
        </div>
      </el-card>

      <!-- Recent Activity -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">最近活动</div>
            <el-button link type="primary" size="small">查看更多 <el-icon><ArrowRight /></el-icon></el-button>
          </div>
        </template>

        <div class="space-y-4">
          <div v-for="(activity, idx) in activities" :key="idx" class="flex items-start gap-3 text-sm">
            <div class="mt-0.5 rounded-full p-1 shrink-0" :class="activityStyle(activity.kind).bgClass">
              <el-icon :class="activityStyle(activity.kind).iconClass"><component :is="activityStyle(activity.kind).icon" /></el-icon>
            </div>
            <div class="flex-1 min-w-0">
              <div class="text-gray-300 break-words line-clamp-2">
                <span class="text-gray-400 mr-1" v-if="activity.agent">{{ activity.agent }}</span>
                <span class="text-gray-500 mr-1">[{{ activity.kind }}]</span>
                {{ activity.content }}
              </div>
            </div>
            <div class="text-xs text-gray-500 shrink-0 whitespace-nowrap">{{ activity.time }}</div>
          </div>
          <div v-if="!activities.length" class="text-xs text-gray-500 text-center py-4">暂无活动</div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<style scoped>
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:deep(.el-textarea__inner) {
  background-color: transparent;
  box-shadow: none !important;
  color: #e5e7eb;
}
:deep(.el-textarea__inner:focus) {
  box-shadow: none !important;
}
:deep(.el-input__wrapper) {
  background-color: #0f1115;
  box-shadow: 0 0 0 1px #2a2d35 inset;
}
:deep(.el-pagination.is-background .el-pager li:not(.is-disabled).is-active) {
  background-color: var(--el-color-primary);
}
</style>
