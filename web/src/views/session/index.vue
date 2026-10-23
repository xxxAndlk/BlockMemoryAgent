<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, watch, provide, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { Session, SessionEvent, AgentNode, TaskBoardData, ClarifyPending, ClarifyQuestionItem, WireImage, SessionGear, SessionThinking, SessionDirectiveSpec } from '@/types'
import { isAssistantTextEvent, isToolCallEvent, isUserMessageEvent, clarifyArtifactsFromFrame } from '@/types'
import {
  createSession,
  sendMessage,
  stopSession,
  getSession,
  getSessionAgents,
  getSessionBoard,
  getSessionMailbox,
  getSessionLogs,
  setSessionWorkDir,
  getSessionMailboxTrace,
  type AgentMailItem,
  type MailboxMessage,
  type SessionLog,
} from '@/api/session'
import { getCapabilities, getHealth, type HealthResponse } from '@/api/health'
import {
  getSessionMetrics,
  getSessionTokenMetrics,
  type SessionMetrics,
  type SessionTokenMetricsResponse,
} from '@/api/metrics'
import { useSessionStream } from '@/composables/useSessionStream'
import { classifyEvent } from '@/views/session/chat/utils/turns'
import { submitClarify } from '@/views/session/chat/utils/clarifySubmit'
import { usePanelRefresh } from '@/composables/usePanelRefresh'
import { useSessionList } from '@/composables/useSessionList'
import { useWorkDir } from '@/composables/useWorkDir'
import { FILE_OPENER_KEY, resolveWorkspacePath, type FileOpener } from '@/composables/fileOpener'
import { knownWorkDirs as knownWorkDirsOf } from '@/utils/dir'
import { maybeNotifySessionDone, maybeNotifyClarifyWaiting } from '@/utils/notifications'
import { setWidgetData } from '@/utils/widgetBus'
import ChatView from './chat/ChatView.vue'
import MonitorView from './monitor/MonitorView.vue'
import TaskBoardPanel from './components/panels/TaskBoardPanel.vue'
import ToolPanel from './components/panels/ToolPanel.vue'
import SessionMemoryPanel from './components/panels/SessionMemoryPanel.vue'
import FilePreview from './components/FilePreview.vue'
import FileViewer from '@/components/FileViewer.vue'
import MetricsCard from './components/MetricsCard.vue'
import TokenMetricsCard from './components/TokenMetricsCard.vue'
import MailboxCard from './components/MailboxCard.vue'
import HealthCard from './components/HealthCard.vue'

const route = useRoute()
const router = useRouter()

const { sessions, loadSessions } = useSessionList()
const stream = useSessionStream()
const panel = usePanelRefresh()
const { workDir, setWorkDir, setDefaultDir } = useWorkDir()

const activeSession = ref<Session | null>(null)
const events = ref<SessionEvent[]>([])
const agents = ref<AgentNode[]>([])
const metrics = ref<SessionMetrics | null>(null)
const tokenMetrics = ref<SessionTokenMetricsResponse | null>(null)
const mailboxMessages = ref<MailboxMessage[]>([])
// 会话级邮件留痕（全部 Agent 收发的历史往来）：对话栏里渲染成邮件气泡。
// 与 mailboxMessages（未取走的邮箱队列 = 待办）语义不同，别混用。
const mails = ref<AgentMailItem[]>([])
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

// 新提问到达（questionId 变化）且页面在后台 → 系统通知"AI 等待你的回答"。
// 同一 questionId 只弹一次（通知 tag 去重 + watcher 只在变化时触发）；答复/超时后
// clarifyPending 复位，下一场提问 id 必变，不会漏通知（2026-09-20：此前只有终态通知，
// ask_user 超时自行决策用户毫无感知）。
watch(
  () => clarifyPending.value?.questionId,
  (qid) => {
    if (!qid) return
    maybeNotifyClarifyWaiting(qid, clarifyPending.value?.detail || clarifyPending.value?.questions?.[0]?.question || '')
  },
)

// 模型实时汇报/思考文本：由 SSE live 帧驱动（不 push 进 events），对齐 TUI 流式展示
const liveStreaming = ref('')
const liveThinking = ref('')

// 接替回合的流式文本快照（key=接替用 user_message 事件时间戳）：运行中会话里 MetaAgent
// 单循环常驻、逐轮流式汇报只存于 live 帧；新用户消息接替当前回合时把此刻的流式文本
// 收编为上一回合的最终答复（否则上一回合永远"处理中"且与当前回合重复渲染同一份 live 帧）。
const replyStash = ref<Record<string, string>>({})

const loading = ref(false)
const sending = ref(false)

// 视图切换：?view=chat|monitor，缺省 chat；chat 为缺省视图不污染 URL。
// 旧链接的 view=orch（已删除的编排页）兼容映射为 chat。
const initialView = route.query.view
const view = ref<'chat' | 'monitor'>(initialView === 'monitor' ? 'monitor' : 'chat')
watch(view, (v) => {
  // 本地 ref 是权威：切换后同步 URL（chat 为缺省不落 query）。
  const cur = route.query.view
  const next = v === 'chat' ? undefined : v
  if (cur === next || (cur === undefined && next === undefined)) return
  router.replace({ query: { ...route.query, view: next } })
})
// URL → ref 反向同步：外部入口只改 query（router.replace），没有这条 watcher 时 ref 仍是
// 'chat'，按钮看起来"点了没反应"（且随后任何切换都会用陈旧 ref 覆盖掉 query）。
// 两个 watcher 收敛到同一值，不会互相触发循环。view=orch 兼容落 chat。
watch(
  () => route.query.view,
  (v) => {
    const next = v === 'monitor' ? 'monitor' : 'chat'
    if (view.value !== next) view.value = next
  }
)

// 右栏统一 5 Tab；侧栏默认收起为图标条，点击展开；展开/收起状态 localStorage 记忆（TODO #26 E）
const rightTab = ref<'board' | 'tools' | 'files' | 'memory' | 'metrics'>('board')
const sidebarOpen = ref(localStorage.getItem('bma.sidebarOpen') === '1')
watch(sidebarOpen, (v) => localStorage.setItem('bma.sidebarOpen', v ? '1' : '0'))
const sideTabs = [
  { name: 'board', label: '任务看板', icon: 'DataLine', width: 760 },
  { name: 'tools', label: '工具', icon: 'Tools', width: 640 },
  { name: 'files', label: '文件', icon: 'FolderOpened', width: 760 },
  { name: 'memory', label: '记忆', icon: 'Coin', width: 560 },
  { name: 'metrics', label: '指标', icon: 'DataAnalysis', width: 600 },
] as const
const activeSideTab = computed(() => sideTabs.find((t) => t.name === rightTab.value))

// ── 文件打开器（TODO #26 阶段 A/C）：消息卡片 / Markdown 本地文件链接 →
//    切到文件 tab、树展开祖先链并打开预览。FilePreview 通过 locateRequest 消费请求
//    （组件可能尚未挂载，用响应式请求而非直接调方法）；绝对路径解析拼会话 work_dir。
const locateRequest = ref<{ path: string; seq: number } | null>(null)
let locateSeq = 0
/** 全屏 FileViewer 实例（TODO #26 阶段 B）：openInViewer 统一入口。 */
const fileViewerRef = ref<InstanceType<typeof FileViewer> | null>(null)
const fileOpener: FileOpener = {
  openInTree: (path: string) => {
    locateRequest.value = { path, seq: ++locateSeq }
    rightTab.value = 'files'
    sidebarOpen.value = true
  },
  openInViewer: (path: string, opts?: { siblings?: string[]; size?: number }) => {
    fileViewerRef.value?.open(path, opts)
  },
  // 回退全局默认目录（useWorkDir）：会话 work_dir 为空 = 服务端按其进程默认目录解析，
  // 前端同口径回退，否则"未选目录的会话"里附件/产物卡片点预览·下载一律提示无工作区。
  resolveAbsolute: (path: string) => resolveWorkspacePath(path, activeSession.value?.work_dir || workDir.value || ''),
  workDir: computed(() => activeSession.value?.work_dir || workDir.value || ''),
  locateRequest,
}
provide(FILE_OPENER_KEY, fileOpener)

// 选中 Agent（经任务看板迷你画布/头部链条点击，?agent=<inst_id> 双向同步，刷新可恢复）：
// 对话/监控两视图随选中 Agent 切换；未选中或选中 MetaAgent 时保持主会话视图。
const selectedAgent = computed<AgentNode | null>(
  () => agents.value.find((a) => a.inst_id === (route.query.agent as string)) || null
)
const isMetaSelected = computed(
  () => selectedAgent.value?.type === 'meta' || selectedAgent.value?.inst_id === 'meta'
)
// 节点已从树中消失（会话切换/清理）时清掉悬挂的 ?agent=，避免空面板（逻辑搬自已删除的 OrchView）。
watch(
  () => route.query.agent,
  (id) => {
    if (id && !agents.value.some((a) => a.inst_id === id)) {
      const q = { ...route.query }
      delete q.agent
      void router.replace({ query: q })
    }
  }
)
// 选中 Agent 变化时同步日志分析过滤（服务端按 agent 名过滤）：选中 meta/无选中即清空回全会话。
watch(selectedAgent, (a) => {
  logFilterAgent.value = a && !isMetaSelected.value ? a.name : ''
  applyLogFilters()
})

onMounted(async () => {
  // 工作目录页「发起新会话」跳入：?work_dir= 预填为新会话默认目录。
  // 带 id 时是"打开既有会话"，不写全局——否则只是浏览一次旧会话，就会把新会话默认目录
  // 悄悄改成该会话的目录（会话目录本身以服务端 work_dir 为准，与此无关）。
  const qwd = route.query.work_dir as string
  // ?new=1 = 侧栏/工作目录页点「新建会话」：要空白新会话，不是恢复上次那个。
  const isNew = !!route.query.new
  if (qwd && !route.query.id) setWorkDir(qwd)

  // 目录选择器占位要显示「不选目录时的真实落盘根」（服务端 agent.default_workdir 解析值）：
  // 挂载拉一次即可，失败静默——占位退回通用文案，不影响建会话。
  void getCapabilities().then((c) => setDefaultDir(c.default_work_dir || '')).catch(() => {})

  await loadSessions()
  const rawId = route.query.id
  // vue-router 会把 undefined 序列化成字面量 "undefined"，过滤脏 id 防止 GET /sessions/undefined
  const id = typeof rawId === 'string' && rawId && rawId !== 'undefined' ? rawId : ''
  if (id) {
    await openSession(id)
  } else if (!isNew) {
    // 无 URL id 时优先恢复上次活跃会话（localStorage），否则打开最近一个。
    // 自动打开失败静默——用户没主动选会话，弹「会话不存在」是噪音（截图场景）。
    const last = localStorage.getItem('lastSessionID')
    const autoId = last && sessions.value.some((s) => s.id === last) ? last : (sessions.value[0]?.id ?? '')
    if (autoId) {
      try {
        await openSession(autoId)
      } catch {
        // openSession 内部已吞掉异常并置 activeSession=null，这里保底不冒泡
      }
    }
  }
})

watch(() => route.query.id, (id) => {
  if (id && typeof id === 'string' && id !== 'undefined' && id !== activeSession.value?.id) {
    openSession(id)
  }
})

// ?new=1 意图的「路由内」响应：在会话页再点一次「新建会话/在此目录新建会话」时，
// 同路由只变 query 不重新挂载，onMounted 不会再跑——不补这条 watcher 就是"点了没反应"。
// 带 id 的导航优先（点会话 = 打开它），只有纯新会话意图才清空。
watch(() => [route.query.new, route.query.work_dir], () => {
  if (!route.query.new || route.query.id) return
  const qwd = route.query.work_dir
  if (typeof qwd === 'string') setWorkDir(qwd)
  resetToBlank()
})

// 断线补播兜底（TODO #16-5 T20）：SSE 流自身带可见期持续重连（api/session.ts），
// 这里负责「回页面/网络恢复」时的面板数据刷新——后台标签页的 3s 自动刷新定时器
// 被浏览器节流，切回来时 agents/看板/指标往往已过期。SSE 重连后的全量快照只补
// 事件流，面板得另刷一次。全部静默失败（面板刷新本就是 allSettled）。
function reconcileAfterWake() {
  if (document.visibilityState !== 'visible') return
  const id = activeSession.value?.id
  if (!id) return
  void refreshPanels(id)
  void loadSessions()
}
const onVisChange = () => {
  if (document.visibilityState === 'visible') reconcileAfterWake()
}
const onNetBack = () => reconcileAfterWake()

onMounted(() => {
  document.addEventListener('visibilitychange', onVisChange)
  window.addEventListener('online', onNetBack)
})
onUnmounted(() => {
  document.removeEventListener('visibilitychange', onVisChange)
  window.removeEventListener('online', onNetBack)
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
  replyStash.value = {}
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
    // 自动打开（无 ?id=）失败时静默——列表还在，用户可手动点选，不需要红色报错
    const isExplicit = !!(typeof route.query.id === 'string' && route.query.id && route.query.id !== 'undefined')
    if (isExplicit) {
      ElMessage.error('会话不存在或加载失败：' + (e instanceof Error ? e.message : String(e)))
    }
  } finally {
    loading.value = false
  }
}

async function refreshPanels(id: string) {
  // 共享同一刷新周期 epoch：Promise.allSettled 里多个 run 若各自 ++epoch，
  // 只有最后一个能存活（前四个结果到达时 epoch 已变被丢弃）→ 面板永远空白。
  const ep = panel.cycle()
  const [agentsRes, boardRes, metricsRes, mbRes, healthRes, mailsRes] = await Promise.allSettled([
    panel.run(() => getSessionAgents(id), ep),
    panel.run(() => getSessionBoard(id), ep),
    panel.run(() => getSessionMetrics(id), ep),
    panel.run(() => getSessionMailbox(id), ep),
    panel.run(() => getHealth(), ep),
    panel.run(() => getSessionMailboxTrace(id), ep),
  ])
  agents.value = agentsRes.status === 'fulfilled' && agentsRes.value ? agentsRes.value.agents || [] : []
  board.value = boardRes.status === 'fulfilled' && boardRes.value ? boardRes.value.board || null : null
  metrics.value = metricsRes.status === 'fulfilled' && metricsRes.value ? metricsRes.value : null
  mailboxMessages.value = mbRes.status === 'fulfilled' && mbRes.value ? mbRes.value.messages || [] : []
  health.value = healthRes.status === 'fulfilled' && healthRes.value ? healthRes.value : null
  mails.value = mailsRes.status === 'fulfilled' && mailsRes.value ? mailsRes.value : []
  await loadSessionLogs(id)
}

async function loadSessionLogs(sessionID: string) {
  // 选中子 Agent 时日志分析走客户端归一化过滤（MonitorView），不下发 agent 参数：
  // 服务端是精确等值匹配，匹配不上带"领域Agent"后缀/截断的展示名，会查空。
  const agentLocked = !!selectedAgent.value && !isMetaSelected.value
  const logsRes = await panel.run(() =>
    getSessionLogs(sessionID, {
      agent: agentLocked ? undefined : logFilterAgent.value || undefined,
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
      // 界面部件快照数据（重连/刷新补断档）：逐项写入总线，WidgetFrame 订阅后自动推给 iframe
      if (snap.widget_data) {
        for (const [wid, payload] of Object.entries(snap.widget_data)) {
          setWidgetData(wid, payload)
        }
      }
    },
    onLive(d) {
      liveStreaming.value = d.streaming_text || ''
      liveThinking.value = d.thinking_text || ''
    },
    onEvent(ev) {
      // 界面部件实时数据帧（瞬时通道，不落时间线）：写入总线即返回，WidgetFrame 订阅后推给 iframe。
      if ((ev as any).type === 'widget_data') {
        const wid = (ev as any).widget_id
        if (typeof wid === 'string' && wid) {
          setWidgetData(wid, typeof (ev as any).payload === 'string' ? (ev as any).payload : '')
        }
        return
      }
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
        // 停放态空壳帧：软停止后会话状态复用 awaiting_clarify（后端无 PendingClarify），
        // 但流仍每 150ms 推该帧（question/question_id/questions 全空）。这不是真实提问，
        // 不能当待澄清——否则徽标误显「待澄清」、按钮变「答复」、提交路由错走 /clarify，
        // 停止后发消息会直接卡死（2026-10-17 用户实证：停止后发送按钮一直不可点）。
        if (!qid && !(ev as any).question && !qs.length) {
          clarifyPending.value = null
          return
        }
        // 帧附带的产物（演示视频等）：脏数据在 clarifyArtifactsFromFrame 内清洗/按扩展名兜底推断
        const arts = clarifyArtifactsFromFrame((ev as any).artifacts)
        clarifyPending.value = {
          options: (ev as any).options || [],
          multiSelect: !!(ev as any).multi_select,
          questionId: qid,
          detail: (ev as any).detail || '',
          questions: qs.length > 1 ? qs : undefined,
          artifacts: arts.length ? arts : undefined,
          // 剩余秒数服务端每帧现算（150ms 重推），直接透给问答卡做倒计时
          timeoutSec: typeof (ev as any).timeout_sec === 'number' ? (ev as any).timeout_sec : undefined,
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
      // 新用户消息接替当前回合：快照此刻流式汇报为上一回合答复 + 复位 live 文本（新回合从零开始）。
      // classifyEvent 与 turns.ts 的回合划分同源（含 type=clarify agent=User 的答复回显），保证
      // replyStash 键与 groupEventsToTurns 查找键一致。答复回显时回合为 awaiting_clarify 非接管态，
      // 快照为空串无害。
      if (classifyEvent(ev) === 'user_message') {
        replyStash.value = { ...replyStash.value, [ev.timestamp]: liveStreaming.value }
      }
      // 工具调用/新指令/中间正文落地 = 上一段流式输出已终结：清 live 缓冲，防旧正文在新回合
      // 重复渲染（任务 140 问题⑤ web 侧双保险，后端已在 hook 恢复时清 StreamingText）。
      // assistant_text（2026-09-13）：正文已落事件由对话栏持久渲染，live 行不清会与之同屏重复。
      if (isToolCallEvent(ev) || isUserMessageEvent(ev) || isAssistantTextEvent(ev)) {
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
      // 浏览器通知（T32）：页面在后台（锁屏/切走）时终态弹系统级提醒
      maybeNotifySessionDone(finalStatus, activeSession.value?.goal)
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

async function handleSubmit(content: string, images: WireImage[] = [], files: File[] = [], gear: SessionGear = 'daily', thinking: SessionThinking = '', plugins: string[] = [], skills: string[] = [], directive: SessionDirectiveSpec | null = null, model = '') {
  if (!content.trim() && !images.length && !files.length) return
  sending.value = true
  try {
    // 1) 待澄清会话（有真实提问）→ 调 /clarify 提交答复。
    // 注意 awaiting_clarify 亦承载"已停止"停放态（无提问）——该情况是普通发送=续跑，
    // 不能路由到 /clarify（后端无待答提问会报错，用户消息也就丢了）。
    if (activeSession.value && activeSession.value.status === 'awaiting_clarify' && clarifyPending.value) {
      // 批量提问（任务 140 问题③）：输入框通道拒绝，引导走问答卡逐题作答统一提交
      if ((clarifyPending.value?.questions?.length || 0) > 1) {
        sending.value = false
        ElMessage.info('当前为批量提问：请在上方问答卡逐题作答后统一提交')
        return
      }
      if (images.length) ElMessage.warning('澄清答复不支持携带图片，已忽略')
      if (files.length) ElMessage.warning('澄清答复不支持携带文件，已忽略')
      // 答复提交带网络级重试（后端重启窗口内不再一句话丢答复），确认条只在真正落地后显示。
      await submitClarify(
        activeSession.value.id,
        { answer: content },
        { onRetry: (n, total) => ElMessage.warning(`后端连接中断，正在重试提交答复（${n}/${total}）…`) },
      )
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
        // 运行中：走 sendMessage —— 后端把内容**即时投进 MetaAgent 邮箱**，本轮内的下一个
        // 检查点就读到它（等子 Agent 时立即生效，其他阶段下一步生效）。此前的 enqueue
        // 只写事件不投递，消息静默丢失：用户看着"已发送"，Agent 毫无反应（2026-09-12 实证）。
        if (images.length) ElMessage.warning('运行中注入的指令不支持携带图片，已忽略')
        // 文件附件可注入：服务端落盘 + 引用文本随邮箱文本注入（比图片强，运行中补文件有效）。
        if (!content.trim() && !files.length) {
          sending.value = false // 只带了图片：没内容可注入，别把发送按钮卡在 loading
          return
        }
        await sendMessage(activeSession.value.id, content, [], files)
        ElMessage.success('指令已注入当前执行（等待子 Agent 时立即生效）')
        sending.value = false
        return
      }
      await sendMessage(activeSession.value.id, content, images, files, directive)
      if (!wasRunning) {
        activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
        await openSession(activeSession.value.id)
      }
      // 提交已受理即收 loading：sending 只表示"提交请求在途"。此前只靠 SSE 终态 done 复位，
      // 会话一落入非终态暂停（awaiting_child / 迭代上限 / 软停止停放）done 永远不来，
      // 发送按钮就永远转圈不可点（2026-10-17 实证）。
      sending.value = false
      return
    }
    // 3) 无选中会话 → 创建新会话（携带创建时选档 + 会话级思考强度 + 指定插件 + 会话模型覆盖，后端非法值 400/记事件）
    const s = await createSession(content, images, workDir.value || undefined, gear, thinking, plugins, files, directive, model, skills)
    setWorkDir(workDir.value)
    sessions.value.unshift(s)
    router.replace({ path: '/session', query: { id: s.id } })
    await openSession(s.id)
    sending.value = false
  } catch (e) {
    console.error('submit failed:', e)
    sending.value = false
    ElMessage.error('发送失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// ── 每会话工作目录（本会话目录，落库即时保存）──
// 有活跃会话 → 展示/修改"该会话自己的"目录；无会话 → 展示全局默认（新会话用）。
const workDirSaving = ref(false)
const activeWorkDir = computed(() =>
  activeSession.value ? activeSession.value.work_dir || '' : workDir.value,
)
/** 已有工作目录（跨会话去重，规范化大小写与尾分隔符）：新建会话时直接挑旧的，不必每次浏览文件系统。 */
const knownWorkDirs = computed(() => knownWorkDirsOf(sessions.value))

/**
 * 目录变更：有活跃会话 → 保存到该会话（服务端权威值就地回填，SSE 快照随后亦是该值，
 * 不会回跳）；无会话 → 只写全局默认（供新建会话使用）。
 */
async function handleWorkDirChange(dir: string) {
  const sess = activeSession.value
  if (!sess) {
    setWorkDir(dir)
    ElMessage.success('新会话目录已设置')
    return
  }
  workDirSaving.value = true
  try {
    const r = await setSessionWorkDir(sess.id, dir)
    activeSession.value = { ...sess, work_dir: r.work_dir }
    const idx = sessions.value.findIndex((s) => s.id === sess.id)
    if (idx >= 0) sessions.value[idx] = { ...sessions.value[idx], work_dir: r.work_dir }
    ElMessage.success('工作目录已更新（下一回合生效）')
  } catch (e) {
    ElMessage.error('修改工作目录失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    workDirSaving.value = false
  }
}

async function handleClarifySubmitted() {
  if (!activeSession.value) return
  activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
  // 问题②：提交成功确认条；后续事件由 SSE 接管。
  clarifyAck.value = true
  // 不再 openSession 全量重载（任务 140 问题④）：重载清空 events 导致滚动跳变。
}

/**
 * 停止（2026-10-16 统一停止交互，唯一停止入口：运行中发送区红色「停止」按钮 /
 * 待澄清态发送区小「停止」逃生口）。
 * 弹窗带勾选框（默认不勾选）：勾选=停止子 Agent + 主 Agent（全停，软停止语义可续跑）；
 * 不勾选=只停主 Agent——运行中的子 Agent 继续执行、结果保留，停止后发送消息即可继续。
 * 待澄清态 = 放弃当前提问并停止（无勾选，固定全停）。
 */
async function handleStop() {
  const sess = activeSession.value
  if (!sess) return
  // 待澄清（有真实提问）：放弃提问并停止，无勾选框。
  const clarifyAsk = sess.status === 'awaiting_clarify' && !!clarifyPending.value
  // 勾选值（非响应式渲染——原生 checkbox 自持视觉态，事件回写即可）。
  const picked = { stopAll: false }
  try {
    await ElMessageBox({
      title: clarifyAsk ? '放弃当前提问并停止？' : '停止运行？',
      message: h('div', { class: 'space-y-1.5 text-sm leading-relaxed', style: 'max-width: 380px' }, clarifyAsk
        ? [
            h('div', null, '将放弃当前待回答的提问，并停止运行中的全部 Agent（含子 Agent）。停止后发送消息可继续。'),
          ]
        : [
            h('div', null, '不勾选（默认）：只停主 Agent——运行中的子 Agent 会继续执行，结果保留；停止后发送消息即可继续。'),
            h('label', { class: 'flex items-center gap-2 pt-1 cursor-pointer select-none' }, [
              h('input', {
                type: 'checkbox',
                class: 'accent-[var(--bma-primary)]',
                onChange: (e: Event) => { picked.stopAll = (e.target as HTMLInputElement).checked },
              }),
              h('span', null, '同时停止运行中的子 Agent'),
            ]),
          ]),
      showCancelButton: true,
      confirmButtonText: '停止',
      cancelButtonText: '取消',
      confirmButtonClass: 'el-button--danger',
      type: 'warning',
    })
  } catch {
    return // 取消/关闭
  }
  try {
    await stopSession(sess.id, { stop_children: clarifyAsk ? true : picked.stopAll })
    ElMessage.success('已停止；发送消息可继续')
    // 立即回取快照收口：SSE 状态帧到达前先把徽章/按钮切到停止态，避免"点了没反应"
    //（对齐原终止路径的即时收口）。
    liveStreaming.value = ''
    liveThinking.value = ''
    // 停止即放弃一切待答提问（停放态 awaiting_clarify 不是提问，发送=续跑）。
    clarifyPending.value = null
    clarifyAck.value = false
    // 软停止落停放态、SSE 不推终态 done（onDone 不会来清 loading），此处必须复位，
    // 否则发送按钮永远卡在 loading/disabled（2026-10-17 用户实证）。
    sending.value = false
    const s = await getSession(sess.id)
    activeSession.value = s
    events.value = [...(s.events || [])]
    const idx = sessions.value.findIndex((x) => x.id === s.id)
    if (idx >= 0) sessions.value[idx] = { ...sessions.value[idx], status: s.status }
  } catch (e) {
    ElMessage.error('停止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

/** 清空一切会话态回到"待创建"（不动路由）：?new=1 入口与页内「新建会话」按钮共用。 */
function resetToBlank() {
  stream.close()
  panel.stopPanelTimer()
  events.value = []
  replyStash.value = {}
  activeSession.value = null
  agents.value = []
  metrics.value = null
  tokenMetrics.value = null
  board.value = null
  sessionLogs.value = []
  liveStreaming.value = ''
  liveThinking.value = ''
  sending.value = false
  clarifyPending.value = null
  clarifyAck.value = false
  clarifyDrafts.value = {}
}

function handleNewSession() {
  resetToBlank()
  // 落 ?new=1：刷新/分享链接仍是新会话，且与上一条会话的 id 明确区分
  router.replace({ path: '/session', query: { new: '1' } })
}

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
</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-ink">
    <!-- 中栏：标题 + 视图切换 + 双视图（左列「会话列表」面板已删除——会话入口统一走
         全局侧栏目录树，见 2026-10-16 用户定调；原搜索/新建入口随之移除） -->
    <main class="flex-1 flex flex-col min-w-0 gap-3">
      <div class="h-12 bg-card border border-line rounded-card flex items-center px-4 gap-3 shrink-0">
        <span class="font-bold text-sm text-ink truncate">{{ activeSession?.goal || '新会话' }}</span>
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
        :agent="isMetaSelected ? null : selectedAgent"
        :clarify="clarifyPending"
        :clarify-ack="clarifyAck"
        :clarify-drafts="clarifyDraftsArr"
        :sending="sending"
        :input-tokens="tokenUsage.input"
        :output-tokens="tokenUsage.output"
        :live-streaming="liveStreaming"
        :live-thinking="liveThinking"
        :prior-replies="replyStash"
        :mails="mails"
        :session-bound="!!activeSession"
        :work-dir="activeWorkDir"
        :work-dir-saving="workDirSaving"
        :known-dirs="knownWorkDirs"
        @submit="handleSubmit"
        @stop="handleStop"
        @new-session="handleNewSession"
        @clarify-submitted="handleClarifySubmitted"
        @update-clarify-drafts="handleUpdateClarifyDrafts"
        @update-workdir="handleWorkDirChange"
        @refresh="activeSession && refreshPanels(activeSession.id)"
      />
      <MonitorView
        v-else-if="view === 'monitor'"
        :session="activeSession"
        :events="events"
        :agents="agents"
        :agent="isMetaSelected ? null : selectedAgent"
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
                          :goal="activeSession?.goal || ''" :events="events" />
          <ToolPanel v-else-if="rightTab === 'tools'" :events="events" />
          <FilePreview v-else-if="rightTab === 'files'" :session-id="activeSession?.id || ''" :agents="agents" />
          <SessionMemoryPanel v-else-if="rightTab === 'memory'" :session-id="activeSession?.id || ''" />
          <div v-else class="space-y-3">
            <MetricsCard :metrics="metrics" />
            <TokenMetricsCard :token-metrics="tokenMetrics" :session-id="activeSession?.id || ''" />
            <MailboxCard :messages="mailboxMessages" />
            <HealthCard :health="health" />
          </div>
        </div>
      </template>
    </aside>

    <!-- 全局全屏预览层（TODO #26 阶段 B）：fileOpener.openInViewer 打开 -->
    <FileViewer ref="fileViewerRef" />
  </div>
</template>

<style scoped>
/* 会话列表面板删除后原 .chat-search / .line-clamp-2 样式一并移除（2026-10-16）。 */
</style>
