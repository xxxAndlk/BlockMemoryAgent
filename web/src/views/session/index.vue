<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { Session, SessionEvent, AgentNode, TaskBoardData, ClarifyPending, ClarifyQuestionItem, WireImage } from '@/types'
import { isToolCallEvent, isUserMessageEvent } from '@/types'
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

// 待澄清选项区：由 SSE awaiting_clarify 帧驱动（不 push 进 events），答复后复位。
// 任务 140 扩展：detail（长上下文）+ questions（批量模式全量题目，长度>1 为批量）。
const clarifyPending = ref<ClarifyPending | null>(null)
// 问题②确认反馈：任一澄清提交路径成功后置位，MessageList 底部渲染「已收到，正在思考中…」；
// 首个真实时间线事件（think/llm/tool 等）到达即清（user_message 回显不清，由其开的
// 新回合占位符接力展示进行中状态）。
const clarifyAck = ref(false)
// 批量问答草稿（任务 140 问题③）：键=question_id，值=逐题草稿（下标对齐 questions）。
// 上浮到本层抗 500ms 帧重推/断线重连；状态离开 awaiting_clarify 或切换会话时清除。
const clarifyDrafts = ref<Record<string, string[]>>({})

// 模型实时汇报/思考文本：由 SSE live 帧驱动（不 push 进 events），对齐 TUI 流式展示
const liveStreaming = ref('')
const liveThinking = ref('')

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
  { name: 'board', label: '任务看板', icon: 'DataLine', width: 760 },
  { name: 'tools', label: '工具', icon: 'Tools', width: 640 },
  { name: 'files', label: '文件', icon: 'FolderOpened', width: 760 },
  { name: 'memory', label: '记忆', icon: 'Coin', width: 560 },
  { name: 'metrics', label: '指标', icon: 'DataAnalysis', width: 600 },
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
  clarifyAck.value = false
  clarifyDrafts.value = {}
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
      // live 文本随快照回填：运行中即得当前值，结束后为空自然清零
      liveStreaming.value = snap.streaming_text || ''
      liveThinking.value = snap.thinking_text || ''
      clarifyAck.value = false
      if (snap.status !== 'awaiting_clarify' && clarifyPending.value) {
        clarifyPending.value = null
      }
    },
    onLive(d) {
      liveStreaming.value = d.streaming_text || ''
      liveThinking.value = d.thinking_text || ''
    },
    onEvent(ev) {
      // 会话状态变化帧（后端 500ms 轮询，状态翻转即推）：同步头部状态徽标与输入答复
      // 路由依据。此前快照仅在连接建立时推一次，awaiting_clarify 帧只喂选项区不更新
      // status，导致待澄清时输入框答复误走 enqueue 通道（内容丢失 + 会话卡死不恢复）。
      if ((ev as any).type === 'session_status') {
        const st = (ev as any).status as Session['status']
        if (activeSession.value) {
          activeSession.value = { ...activeSession.value, status: st }
        }
        if (st !== 'awaiting_clarify') {
          // 状态离开待澄清：复位问答卡与该题草稿（答复已被后端接收，SSE 增量接管）
          if (clarifyPending.value) delete clarifyDrafts.value[clarifyPending.value.questionId]
          clarifyPending.value = null
        }
        return
      }
      if ((ev as any).type === 'awaiting_clarify') {
        const qid = (ev as any).question_id || ''
        const qs = ((ev as any).questions || []) as ClarifyQuestionItem[]
        clarifyPending.value = {
          options: (ev as any).options || [],
          multiSelect: !!(ev as any).multi_select,
          questionId: qid,
          detail: (ev as any).detail || '',
          questions: qs.length > 1 ? qs : undefined,
        }
        // 批量题：懒初始化逐题草稿（键=question_id，抗帧重推/重连）
        if (qs.length > 1 && !clarifyDrafts.value[qid]) {
          clarifyDrafts.value[qid] = qs.map(() => '')
        }
        clarifyAck.value = false
        // 输入答复路由依据（handleSubmit 按 status === 'awaiting_clarify' 走 /clarify）。
        if (activeSession.value) {
          activeSession.value = { ...activeSession.value, status: 'awaiting_clarify' }
        }
        return
      }
      // 工具调用/新指令落地 = 上一段流式输出已终结：清 live 缓冲，防旧正文在新回合
      // 重复渲染（任务 140 问题⑤ web 侧双保险，后端已在 hook 恢复时清 StreamingText）。
      if (isToolCallEvent(ev) || isUserMessageEvent(ev)) {
        liveStreaming.value = ''
        liveThinking.value = ''
      }
      // 确认条（问题②）：首个真实时间线事件到达即收起（user_message 回显不清，
      // 由其开启的新回合「正在生成回答…」占位符接力）。
      if (clarifyAck.value && !isUserMessageEvent(ev)) {
        clarifyAck.value = false
      }
      events.value.push(ev)
    },
    onDone(finalStatus?: string) {
      liveStreaming.value = ''
      liveThinking.value = ''
      clarifyAck.value = false
      panel.stopPanelTimer()
      refreshPanels(s.id)
      loadSessions()
      if (activeSession.value) {
        const status = (finalStatus as Session['status']) || 'completed'
        activeSession.value = { ...activeSession.value, status }
      }
      // 对账重取（任务 140 问题⑥）：SSE 增量按切片索引 diff，头部裁剪/重连等竞态
      // 丢掉的尾部事件（含最终答复 agent_done）在此原子替换补齐，对话栏不再卡
      // 「正在生成答复…」。
      getSession(s.id).then((fresh) => {
        if (activeSession.value?.id !== s.id) return
        activeSession.value = fresh
        events.value = [...(fresh.events || [])]
      }).catch((e) => console.error('done reconcile failed:', e))
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
      // 批量提问（任务 140 问题③）：输入框通道拒绝，引导走问答卡逐题作答统一提交
      if ((clarifyPending.value?.questions?.length || 0) > 1) {
        sending.value = false
        ElMessage.info('当前为批量提问：请在上方问答卡逐题作答后统一提交')
        return
      }
      if (images.length) ElMessage.warning('澄清答复不支持携带图片，已忽略')
      await clarifySession(activeSession.value.id, content)
      activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
      clarifyAck.value = true
      sending.value = false
      // 不再 openSession 全量重载（任务 140 问题④）：重载清空 events 导致滚动跳变，
      // 后续事件由 SSE session_status/增量帧驱动。
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
  // 问题②：提交成功确认条；后续事件由 SSE 接管。
  clarifyAck.value = true
  // 不再 openSession 全量重载（任务 140 问题④）：重载清空 events 导致滚动跳变。
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
  clarifyAck.value = false
  clarifyDrafts.value = {}
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

// 批量问答草稿视图（下传问答卡）：批量态返回逐题草稿数组（下标对齐 questions），单题态为空
const clarifyDraftsArr = computed<string[]>(() => {
  const p = clarifyPending.value
  if (!p || !p.questions || p.questions.length <= 1) return []
  return clarifyDrafts.value[p.questionId] || p.questions.map(() => '')
})

// 问答卡草稿变更回写（键=question_id，抗 awaiting_clarify 帧 500ms 重推/断线重连）
function handleUpdateClarifyDrafts(drafts: string[]) {
  const p = clarifyPending.value
  if (!p) return
  clarifyDrafts.value = { ...clarifyDrafts.value, [p.questionId]: [...drafts] }
}

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
        :clarify-ack="clarifyAck"
        :clarify-drafts="clarifyDraftsArr"
        :sending="sending"
        :input-tokens="tokenUsage.input"
        :output-tokens="tokenUsage.output"
        :live-streaming="liveStreaming"
        :live-thinking="liveThinking"
        @submit="handleSubmit"
        @cancel="handleCancel"
        @stop="handleStop"
        @interrupt="handleInterrupt"
        @new-session="handleNewSession"
        @clarify-submitted="handleClarifySubmitted"
        @update-clarify-drafts="handleUpdateClarifyDrafts"
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

    <!-- 右栏：可收起侧栏（默认收起为图标条，点击展开；各面板独立宽度，无 tab 切换） -->
    <aside class="shrink-0 bg-card border border-line rounded-card overflow-hidden flex flex-col transition-all duration-200"
           :style="{ width: (sidebarOpen ? (activeSideTab?.width ?? 640) : 48) + 'px' }">
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
        <!-- 单面板渲染：打开哪个只看哪个（无 tab 切换条），面板切换走图标条 -->
        <div class="flex-1 min-h-0" :class="rightTab === 'files' ? 'overflow-hidden' : 'overflow-y-auto p-3'">
          <TaskBoardPanel v-if="rightTab === 'board'" :agents="agents" :board="board"
                          :goal="activeSession?.goal || ''" />
          <ToolPanel v-else-if="rightTab === 'tools'" :events="events" />
          <FilePreview v-else-if="rightTab === 'files'" :session-id="activeSession?.id || ''" :agents="agents" />
          <SessionMemoryPanel v-else-if="rightTab === 'memory'" :session-id="activeSession?.id || ''" />
          <div v-else class="space-y-3">
            <MetricsCard :metrics="metrics" />
            <TokenMetricsCard :token-metrics="tokenMetrics" />
            <MailboxCard :messages="mailboxMessages" />
            <HealthCard :health="health" />
          </div>
        </div>
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
</style>
