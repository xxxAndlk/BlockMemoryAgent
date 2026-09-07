<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { Session, SessionEvent, AgentNode, TaskBoardData, ClarifyOption, WireImage } from '@/types'
import {
  createSession,
  sendMessage,
  clarifySession,
  cancelSession,
  stopSession,
  enqueueSession,
  interruptSession,
  getSession,
  getSessionAgents,
  getSessionBoard,
  getSessionMailbox,
  getSessionLogs,
  type MailboxMessage,
  type SessionLog,
} from '@/api/session'
import { getHealth, type HealthResponse } from '@/api/health'
import {
  getSessionMetrics,
  getSessionTokenMetrics,
  type SessionMetrics,
  type SessionTokenMetricsResponse,
} from '@/api/metrics'
import { useSessionStream } from '@/composables/useSessionStream'
import { usePanelRefresh } from '@/composables/usePanelRefresh'
import { useSessionList } from '@/composables/useSessionList'
import { useSessionStatus } from '@/composables/useSessionStatus'
import { useWorkDir } from '@/composables/useWorkDir'
import ChatView from './chat/ChatView.vue'
import MonitorView from './monitor/MonitorView.vue'
import TaskBoardPanel from './components/panels/TaskBoardPanel.vue'
import ToolPanel from './components/panels/ToolPanel.vue'
import SessionMemoryPanel from './components/panels/SessionMemoryPanel.vue'
import FilePreview from './components/FilePreview.vue'
import MetricsCard from './components/MetricsCard.vue'
import TokenMetricsCard from './components/TokenMetricsCard.vue'
import MailboxCard from './components/MailboxCard.vue'
import HealthCard from './components/HealthCard.vue'

const route = useRoute()
const router = useRouter()

const { sessions, loadSessions } = useSessionList()
const stream = useSessionStream()
const panel = usePanelRefresh()
const { statusDotClass, statusText } = useSessionStatus()
const { workDir, setWorkDir } = useWorkDir()

const activeSession = ref<Session | null>(null)
const events = ref<SessionEvent[]>([])
const agents = ref<AgentNode[]>([])
const metrics = ref<SessionMetrics | null>(null)
const tokenMetrics = ref<SessionTokenMetricsResponse | null>(null)
const mailboxMessages = ref<MailboxMessage[]>([])
const health = ref<HealthResponse | null>(null)
const board = ref<TaskBoardData | null>(null)

// 结构化会话日志（监控视图「日志分析」Tab）
const sessionLogs = ref<SessionLog[]>([])
const logFilterAgent = ref('')
const logFilterLevel = ref('')
const logLimit = ref(100)
const expandedLogId = ref<number | null>(null)

// 待澄清选项区：由 SSE awaiting_clarify 帧驱动（不 push 进 events），答复后复位
const clarifyPending = ref<{ options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null>(null)

const loading = ref(false)
const sending = ref(false)
const sessionFilter = ref('')

// 视图切换：?view=chat|monitor，缺省 chat；chat 为缺省视图不污染 URL
const view = ref<'chat' | 'monitor'>((route.query.view as string) === 'monitor' ? 'monitor' : 'chat')
watch(view, (v) => {
  router.replace({ query: { ...route.query, view: v === 'chat' ? undefined : v } })
})

// 右栏统一 5 Tab；侧栏默认收起为图标条，点击展开
const rightTab = ref<'board' | 'tools' | 'files' | 'memory' | 'metrics'>('board')
const sidebarOpen = ref(false)
const sideTabs = [
  { name: 'board', label: '任务看板', icon: 'DataLine' },
  { name: 'tools', label: '工具', icon: 'Tools' },
  { name: 'files', label: '文件', icon: 'FolderOpened' },
  { name: 'memory', label: '记忆', icon: 'Coin' },
  { name: 'metrics', label: '指标', icon: 'DataAnalysis' },
] as const
const activeSideTab = computed(() => sideTabs.find((t) => t.name === rightTab.value))

onMounted(async () => {
  // 工作目录页「发起新会话」跳入：?work_dir= 预填并固化（与提交时 setWorkDir 同策略）
  const qwd = route.query.work_dir as string
  if (qwd) setWorkDir(qwd)

  await loadSessions()
  const id = route.query.id as string
  if (id) {
    await openSession(id)
  } else {
    // 无 URL id 时优先恢复上次活跃会话（localStorage），否则打开最近一个
    const last = localStorage.getItem('lastSessionID')
    if (last && sessions.value.some((s) => s.id === last)) {
      await openSession(last)
    } else if (sessions.value.length) {
      await openSession(sessions.value[0].id)
    }
  }
})

watch(() => route.query.id, (id) => {
  if (id && typeof id === 'string' && id !== activeSession.value?.id) {
    openSession(id)
  }
})

async function openSession(id: string) {
  loading.value = true
  stream.close()
  panel.stopPanelTimer()
  panel.invalidate()
  events.value = []
  clarifyPending.value = null // 切换会话时复位待澄清选项，避免串会话残留
  try {
    const s = await getSession(id)
    activeSession.value = s
    events.value = [...(s.events || [])]
    localStorage.setItem('lastSessionID', id)
    startStream(s)
    await refreshPanels(id)
  } catch (e) {
    localStorage.removeItem('lastSessionID')
    activeSession.value = null
    ElMessage.error('会话不存在或加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function refreshPanels(id: string) {
  const [agentsRes, boardRes, metricsRes, mbRes, healthRes] = await Promise.allSettled([
    panel.run(() => getSessionAgents(id)),
    panel.run(() => getSessionBoard(id)),
    panel.run(() => getSessionMetrics(id)),
    panel.run(() => getSessionMailbox(id)),
    panel.run(() => getHealth()),
  ])
  agents.value = agentsRes.status === 'fulfilled' && agentsRes.value ? agentsRes.value.agents || [] : []
  board.value = boardRes.status === 'fulfilled' && boardRes.value ? boardRes.value.board || null : null
  metrics.value = metricsRes.status === 'fulfilled' && metricsRes.value ? metricsRes.value : null
  mailboxMessages.value = mbRes.status === 'fulfilled' && mbRes.value ? mbRes.value.messages || [] : []
  health.value = healthRes.status === 'fulfilled' && healthRes.value ? healthRes.value : null
  await loadSessionLogs(id)
}

async function loadSessionLogs(sessionID: string) {
  const logsRes = await panel.run(() =>
    getSessionLogs(sessionID, {
      agent: logFilterAgent.value || undefined,
      level: logFilterLevel.value || undefined,
      limit: logLimit.value,
    })
  )
  sessionLogs.value = logsRes?.logs || []
  const tokenRes = await panel.run(() => getSessionTokenMetrics(sessionID))
  tokenMetrics.value = tokenRes || null
}

function applyLogFilters() {
  if (activeSession.value) loadSessionLogs(activeSession.value.id)
}

function startStream(s: Session) {
  panel.startAutoRefresh(s.id, refreshPanels)
  stream.startStream(s.id, {
    onSnapshot(snap) {
      activeSession.value = snap
      events.value = [...(snap.events || [])]
      if (snap.status !== 'awaiting_clarify' && clarifyPending.value) {
        clarifyPending.value = null
      }
    },
    onEvent(ev) {
      if ((ev as any).type === 'awaiting_clarify') {
        clarifyPending.value = {
          options: (ev as any).options || [],
          multiSelect: !!(ev as any).multi_select,
          questionId: (ev as any).question_id || '',
        }
        return
      }
      events.value.push(ev)
    },
    onDone(finalStatus?: string) {
      panel.stopPanelTimer()
      refreshPanels(s.id)
      loadSessions()
      if (activeSession.value) {
        const status = (finalStatus as Session['status']) || 'completed'
        activeSession.value = { ...activeSession.value, status }
      }
      sending.value = false
    },
    onError(err) {
      panel.stopPanelTimer()
      console.error('SSE error:', err)
      sending.value = false
      ElMessage.error('实时连接异常，请检查网络或刷新页面')
    },
  })
}

async function handleSubmit(content: string, images: WireImage[] = []) {
  if (!content.trim() && !images.length) return
  sending.value = true
  try {
    // 1) 待澄清会话 → 调 /clarify 提交答复
    if (activeSession.value && activeSession.value.status === 'awaiting_clarify') {
      if (images.length) ElMessage.warning('澄清答复不支持携带图片，已忽略')
      await clarifySession(activeSession.value.id, content)
      activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
      await openSession(activeSession.value.id)
      return
    }
    // 2) 已有会话 → 追加消息 / 入队 / 续跑
    if (activeSession.value) {
      const wasRunning = activeSession.value.status === 'running'
      if (wasRunning && !activeSession.value.destroy_at) {
        if (images.length) ElMessage.warning('运行中入队不支持携带图片，已忽略')
        await enqueueSession(activeSession.value.id, content)
        sending.value = false
        return
      }
      await sendMessage(activeSession.value.id, content, images)
      if (!wasRunning) {
        activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
        await openSession(activeSession.value.id)
      }
      return
    }
    // 3) 无选中会话 → 创建新会话
    const s = await createSession(content, images, workDir.value || undefined)
    setWorkDir(workDir.value)
    sessions.value.unshift(s)
    router.replace({ path: '/session', query: { id: s.id } })
    await openSession(s.id)
  } catch (e) {
    console.error('submit failed:', e)
    sending.value = false
    ElMessage.error('发送失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleClarifySubmitted() {
  if (!activeSession.value) return
  activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
  await openSession(activeSession.value.id)
}

async function handleCancel() {
  if (!activeSession.value) return
  try {
    await ElMessageBox.confirm(
      '硬终止会立即取消全部子 Agent 与当前任务，且不可续跑。建议优先使用软停止。',
      '确认硬终止？',
      { confirmButtonText: '硬终止', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }
  try {
    await cancelSession(activeSession.value.id)
  } catch (e) {
    ElMessage.error('取消会话失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleInterrupt() {
  if (!activeSession.value) return
  try {
    const { value } = await ElMessageBox.prompt(
      '将打断当前执行，并把输入内容作为新指令注入继续运行',
      '抢占中断',
      { confirmButtonText: '中断', cancelButtonText: '取消', inputPlaceholder: '新指令（必填）', type: 'warning' }
    )
    if (!value || !value.trim()) return
    await interruptSession(activeSession.value.id, value.trim())
    ElMessage.success('已中断并注入新指令')
  } catch (e) {
    if (e === 'cancel' || (e instanceof Error && e.message === 'cancel')) return
    ElMessage.error('中断失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleStop() {
  if (!activeSession.value) return
  try {
    await stopSession(activeSession.value.id)
    ElMessage.success('已软停止，倒计时内发送新消息可续跑')
    const s = await getSession(activeSession.value.id)
    activeSession.value = s
  } catch (e) {
    ElMessage.error('软停止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleNewSession() {
  stream.close()
  panel.stopPanelTimer()
  events.value = []
  activeSession.value = null
  agents.value = []
  metrics.value = null
  tokenMetrics.value = null
  board.value = null
  clarifyPending.value = null
  router.replace({ path: '/session' })
}

const filteredSessions = computed(() => {
  const q = sessionFilter.value.trim().toLowerCase()
  if (!q) return sessions.value
  return sessions.value.filter(s =>
    s.id.toLowerCase().includes(q) ||
    (s.goal || '').toLowerCase().includes(q))
})

const tokenUsage = computed(() => {
  let input = 0
  let output = 0
  for (const ev of events.value) {
    if (ev.kind !== 'token_usage') continue
    input += ev.input_tokens || 0
    output += ev.output_tokens || 0
  }
  return { input, output }
})

function fmtDateTime(iso: string) {
  if (!iso) return ''
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-ink">
    <!-- 左侧会话列表 -->
    <aside class="w-[280px] flex flex-col shrink-0 bg-card border border-line rounded-card overflow-hidden">
      <div class="px-3 py-3 border-b border-line flex items-center justify-between gap-2">
        <span class="text-sm font-bold text-ink">会话列表</span>
        <el-button size="small" plain @click="handleNewSession">
          <el-icon><Plus /></el-icon>
        </el-button>
      </div>
      <div class="px-3 py-2 border-b border-line">
        <el-input v-model="sessionFilter" size="small" placeholder="搜索会话…" class="chat-search">
          <template #prefix><el-icon class="text-ink-3"><Search /></el-icon></template>
        </el-input>
      </div>

      <div class="flex-1 overflow-y-auto">
        <button v-for="s in filteredSessions" :key="s.id"
                @click="openSession(s.id)"
                class="w-full text-left px-3 py-2.5 border-b border-line hover:bg-page transition-colors"
                :class="{ 'bg-primary-soft': activeSession?.id === s.id }">
          <div class="flex items-center gap-2 mb-1">
            <span class="w-1.5 h-1.5 rounded-full shrink-0" :class="statusDotClass(s.status)"></span>
            <span class="text-xs text-ink-3 font-mono shrink-0">{{ s.id }}</span>
            <span class="text-[10px] text-ink-3 ml-auto shrink-0">{{ statusText(s.status) }}</span>
          </div>
          <div class="text-sm text-ink line-clamp-2 break-words">{{ s.goal || '(无目标)' }}</div>
          <div class="text-[10px] text-ink-3 mt-1">{{ fmtDateTime(s.started_at) }}</div>
        </button>

        <div v-if="!filteredSessions.length" class="text-center text-xs text-ink-3 py-10">
          暂无会话，在下方输入框直接下达命令即可创建。
        </div>
      </div>
    </aside>

    <!-- 中栏：标题 + 视图切换 + 双视图 -->
    <main class="flex-1 flex flex-col min-w-0 gap-3">
      <div class="h-12 bg-card border border-line rounded-card flex items-center px-4 gap-3 shrink-0">
        <span class="font-bold text-sm text-ink truncate">{{ activeSession?.goal || '新会话' }}</span>
        <span v-if="activeSession" class="text-xs text-ink-3 font-mono shrink-0">{{ activeSession.id }}</span>
        <div class="ml-auto flex bg-page border border-line rounded-lg p-0.5 shrink-0">
          <button class="px-3 py-1 text-xs rounded-md transition-colors"
                  :class="view === 'chat' ? 'bg-card text-primary font-bold shadow-sm' : 'text-ink-2 hover:text-ink'"
                  @click="view = 'chat'">💬 对话</button>
          <button class="px-3 py-1 text-xs rounded-md transition-colors"
                  :class="view === 'monitor' ? 'bg-card text-primary font-bold shadow-sm' : 'text-ink-2 hover:text-ink'"
                  @click="view = 'monitor'">📊 监控</button>
        </div>
      </div>

      <ChatView
        v-if="view === 'chat'"
        :session="activeSession"
        :events="events"
        :agents="agents"
        :clarify="clarifyPending"
        :sending="sending"
        :input-tokens="tokenUsage.input"
        :output-tokens="tokenUsage.output"
        @submit="handleSubmit"
        @cancel="handleCancel"
        @stop="handleStop"
        @interrupt="handleInterrupt"
        @new-session="handleNewSession"
        @clarify-submitted="handleClarifySubmitted"
      />
      <MonitorView
        v-else
        :session="activeSession"
        :events="events"
        :agents="agents"
        :session-logs="sessionLogs"
        v-model:log-agent="logFilterAgent"
        v-model:log-level="logFilterLevel"
        v-model:expanded-log-id="expandedLogId"
        @query-logs="applyLogFilters"
      />
    </main>

    <!-- 右栏：可收起侧栏（默认收起为图标条，点击展开；展开宽度 640px） -->
    <aside class="shrink-0 bg-card border border-line rounded-card overflow-hidden flex flex-col transition-all duration-200"
           :class="sidebarOpen ? 'w-[640px]' : 'w-[48px]'">
      <template v-if="!sidebarOpen">
        <div class="flex flex-col items-center gap-1 py-3 flex-1">
          <el-tooltip content="展开侧栏" placement="left">
            <button class="p-2 rounded-lg text-ink-2 hover:text-ink hover:bg-page transition-colors"
                    @click="sidebarOpen = true">
              <el-icon :size="16"><ArrowLeft /></el-icon>
            </button>
          </el-tooltip>
          <div class="w-6 border-t border-line my-1"></div>
          <el-tooltip v-for="t in sideTabs" :key="t.name" :content="t.label" placement="left">
            <button class="p-2 rounded-lg transition-colors"
                    :class="rightTab === t.name ? 'text-primary bg-primary-soft' : 'text-ink-2 hover:text-ink hover:bg-page'"
                    @click="rightTab = t.name; sidebarOpen = true">
              <el-icon :size="16"><component :is="t.icon" /></el-icon>
            </button>
          </el-tooltip>
        </div>
      </template>
      <template v-else>
        <div class="h-10 shrink-0 flex items-center justify-between px-4 border-b border-line">
          <span class="text-sm font-bold text-ink">{{ activeSideTab?.label }}</span>
          <el-tooltip content="收起侧栏" placement="left">
            <button class="p-1.5 rounded-lg text-ink-2 hover:text-ink hover:bg-page transition-colors"
                    @click="sidebarOpen = false">
              <el-icon :size="16"><ArrowRight /></el-icon>
            </button>
          </el-tooltip>
        </div>
        <el-tabs v-model="rightTab" class="session-right-tabs flex-1 flex flex-col min-h-0">
          <el-tab-pane label="任务看板" name="board" class="flex-1 overflow-y-auto p-3">
            <TaskBoardPanel :agents="agents" :board="board" />
          </el-tab-pane>
          <el-tab-pane label="工具" name="tools" class="flex-1 overflow-y-auto p-3">
            <ToolPanel :events="events" />
          </el-tab-pane>
          <el-tab-pane label="文件" name="files" class="flex-1 overflow-hidden p-0">
            <FilePreview :session-id="activeSession?.id || ''" :agents="agents" />
          </el-tab-pane>
          <el-tab-pane label="记忆" name="memory" class="flex-1 overflow-y-auto p-3">
            <SessionMemoryPanel :session-id="activeSession?.id || ''" />
          </el-tab-pane>
          <el-tab-pane label="指标" name="metrics" class="flex-1 overflow-y-auto p-3">
            <div class="space-y-3">
              <MetricsCard :metrics="metrics" />
              <TokenMetricsCard :token-metrics="tokenMetrics" />
              <MailboxCard :messages="mailboxMessages" />
              <HealthCard :health="health" />
            </div>
          </el-tab-pane>
        </el-tabs>
      </template>
    </aside>
  </div>
</template>

<style scoped>
:deep(.chat-search .el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
}
:deep(.chat-search .el-input__wrapper.is-focus) {
  box-shadow: 0 0 0 1px var(--bma-primary) inset;
}

.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

/* 右栏 Tab：标签栏固定在卡片顶部，内容区自适应滚动 */
.session-right-tabs :deep(.el-tabs__header) {
  margin: 0;
  padding: 0 12px;
  border-bottom: 1px solid var(--bma-border);
}
.session-right-tabs :deep(.el-tabs__content) {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.session-right-tabs :deep(.el-tab-pane) {
  height: 100%;
}
</style>
