<script setup lang="ts">
import { ref, onMounted, computed, watch, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session, AgentNode, TaskBoardData } from '@/types'
import {
  listSessions,
  getSession,
  getSessionBoard,
  getSessionAgents,
  streamSession,
  getSessionWatchdog,
  getSessionMailbox,
  getSessionLogs,
  type WatchdogDecision,
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
import ExecutionLog from './components/ExecutionLog.vue'
import MemoryExplorer from './components/MemoryExplorer.vue'
import SkillSet from './components/SkillSet.vue'
import FilePreview from './components/FilePreview.vue'

const route = useRoute()
const sessions = ref<Session[]>([])
const activeSession = ref<Session | null>(null)
const activeTab = ref('log')
const loading = ref(false)
const closeStream = ref<(() => void) | null>(null)

const agents = ref<AgentNode[]>([])
const board = ref<TaskBoardData | null>(null)

const metrics = ref<SessionMetrics | null>(null)
const watchdogDecisions = ref<WatchdogDecision[]>([])
const mailboxMessages = ref<MailboxMessage[]>([])
const health = ref<HealthResponse | null>(null)

// P1-2：结构化会话日志与 Token 面板
const sessionLogs = ref<SessionLog[]>([])
const tokenMetrics = ref<SessionTokenMetricsResponse | null>(null)
const logFilterAgent = ref('')
const logFilterLevel = ref('')
const logLimit = ref(100)
const expandedLogId = ref<number | null>(null)

onMounted(async () => {
  await loadSessions()
  const id = route.query.id as string
  if (id) {
    selectSessionById(id)
  } else if (sessions.value.length) {
    selectSession(sessions.value[0])
  }
})

onUnmounted(() => {
  closeStream.value?.()
})

watch(() => route.query.id, (id) => {
  if (id && typeof id === 'string') selectSessionById(id)
})

async function loadSessions() {
  try {
    sessions.value = await listSessions()
  } catch (e) {
    sessions.value = []
    ElMessage.error('加载会话列表失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function selectSessionById(id: string) {
  loading.value = true
  try {
    const s = await getSession(id)
    selectSession(s)
  } catch (e) {
    const s = sessions.value.find(x => x.id === id)
    if (s) {
      selectSession(s)
    } else {
      ElMessage.error('会话不存在或加载失败：' + (e instanceof Error ? e.message : String(e)))
    }
  } finally {
    loading.value = false
  }
}

async function selectSession(s: Session) {
  activeSession.value = s
  closeStream.value?.()
  startStream(s)
  await loadSessionPanels(s.id)
}

async function loadSessionPanels(sessionID: string) {
  try {
    const boardRes = await getSessionBoard(sessionID)
    board.value = boardRes.board || null
  } catch {
    board.value = null
  }
  try {
    const agentRes = await getSessionAgents(sessionID)
    agents.value = agentRes.agents || []
  } catch {
    agents.value = []
  }
  try {
    metrics.value = await getSessionMetrics(sessionID)
  } catch {
    metrics.value = null
  }
  try {
    const wdRes = await getSessionWatchdog(sessionID)
    watchdogDecisions.value = wdRes.decisions || []
  } catch {
    watchdogDecisions.value = []
  }
  try {
    const mbRes = await getSessionMailbox(sessionID)
    mailboxMessages.value = mbRes.messages || []
  } catch {
    mailboxMessages.value = []
  }
  try {
    health.value = await getHealth()
  } catch {
    health.value = null
  }
  await loadSessionLogs(sessionID)
}

async function loadSessionLogs(sessionID: string) {
  try {
    const res = await getSessionLogs(sessionID, {
      agent: logFilterAgent.value || undefined,
      level: logFilterLevel.value || undefined,
      limit: logLimit.value,
    })
    sessionLogs.value = res.logs || []
  } catch {
    sessionLogs.value = []
  }
  try {
    tokenMetrics.value = await getSessionTokenMetrics(sessionID)
  } catch {
    tokenMetrics.value = null
  }
}

function applyLogFilters() {
  if (activeSession.value) loadSessionLogs(activeSession.value.id)
}

function startStream(s: Session) {
  closeStream.value = streamSession(
    s.id,
    (ev) => {
      // SSE 连接建立 / 重连时后端推送完整 Session 快照（含 id/goal/events）。
      // F12 修复：原实现直接 return 丢弃快照，导致 activeSession.status 永不更新
      // （header 一直显示 running 即使会话已 completed/error）。
      // 改为用快照刷新 activeSession 状态 + events（重连去重靠快照重置）。
      if (ev && 'id' in ev && 'goal' in ev && 'events' in ev) {
        const snap = ev as unknown as Session
        if (activeSession.value?.id === snap.id) {
          activeSession.value = { ...activeSession.value, status: snap.status, events: snap.events || [] }
        }
        return
      }
      // 推到 activeSession.events（非 sessions 列表项的 s），避免污染共享对象
      if (activeSession.value && activeSession.value.id === s.id) {
        activeSession.value = { ...activeSession.value, events: [...(activeSession.value.events || []), ev] }
      }
    },
    () => {
      loadSessions()
    },
    (err) => {
      console.error('SSE error:', err)
      ElMessage.error('实时连接异常，请检查网络或刷新页面')
    }
  )
}

const roleTree = computed(() => {
  const root: any[] = []
  const map = new Map<string, any>()
  agents.value.forEach(a => {
    const node = {
      label: a.name,
      status: a.status,
      statusType: a.status === 'active' ? 'success' : a.status === 'running' ? 'warning' : 'info',
      active: a.status === 'active' || a.status === 'running',
      isUser: a.type === 'domain' || a.type === 'subdomain',
      iconColor: a.status === 'active' ? 'text-green-500' : a.status === 'running' ? 'text-yellow-500' : 'text-gray-500',
      children: [] as any[],
    }
    map.set(a.inst_id, node)
    if (!a.parent_id) root.push(node)
  })
  agents.value.forEach(a => {
    if (a.parent_id && map.has(a.parent_id)) {
      map.get(a.parent_id)!.children.push(map.get(a.inst_id))
    }
  })
  // 无 Agent 数据时返回空数组，模板渲染空状态占位，不再展示伪造的角色树
  return root
})

interface TaskItem {
  title?: string
  name?: string
  assignee?: string
  status: string
}

const tasks = computed(() => {
  if (board.value?.tasks?.length) {
    return board.value.tasks.map(t => ({ title: t.title, assignee: t.assignee, status: t.status })) as TaskItem[]
  }
  // 无任务数据返回空数组，模板展示空状态，不再伪造任务
  return [] as TaskItem[]
})

const constraints = computed(() => {
  if (board.value?.constraints) return Object.entries(board.value.constraints)
  // 无约束数据返回空数组
  return [] as [string, string][]
})


const progress = computed(() => {
  const done = tasks.value.filter(t => t.status === 'done').length
  const total = tasks.value.length
  return total ? Math.round((done / total) * 100) : 0
})

const defaultProps = { children: 'children', label: 'label' }

function fmtTime(iso: string) {
  return new Date(iso).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

function fmtDateTime(iso: string) {
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function watchdogTagType(level: string) {
  switch (level) {
    case 'OK': return 'success'
    case 'WARN': return 'warning'
    case 'COMPRESS': return 'warning'
    case 'EVICT': return 'danger'
    default: return 'info'
  }
}

function watchdogLabel(level: string) {
  switch (level) {
    case 'OK': return '正常'
    case 'WARN': return '警告'
    case 'COMPRESS': return '压缩'
    case 'EVICT': return '驱逐'
    default: return level
  }
}

function mailboxTagType(type: string) {
  switch (type) {
    case 'milestone': return 'success'
    case 'request': return 'primary'
    case 'escalate': return 'danger'
    case 'dependency': return 'warning'
    case 'info': return 'info'
    default: return 'info'
  }
}

function mailboxIcon(type: string) {
  switch (type) {
    case 'milestone': return 'Trophy'
    case 'request': return 'Connection'
    case 'escalate': return 'WarningFilled'
    case 'dependency': return 'Link'
    case 'info': return 'Document'
    default: return 'Message'
  }
}

function healthDotClass(service?: { online?: boolean }) {
  return service?.online ? 'text-green-500' : 'text-red-500'
}

function healthStatusText(service?: { online?: boolean; detail?: string }) {
  return service?.online ? (service.detail || 'Connected') : (service?.detail || 'Offline')
}

</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-gray-300">
    <!-- Left: Hierarchy & Task Board -->
    <div class="w-[320px] flex flex-col gap-4 overflow-y-auto shrink-0 pr-1">
      <!-- Role Hierarchy -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">角色层级 (Role Hierarchy)</div>
          </div>
        </template>
        <div v-if="!roleTree.length" class="text-gray-500 text-xs py-4 text-center">暂无角色实例，会话启动后自动创建</div>
        <el-tree
          v-else
          :data="roleTree"
          :props="defaultProps"
          default-expand-all
          class="!bg-transparent text-sm custom-tree"
          :expand-on-click-node="false"
        >
          <template #default="{ node, data }">
            <div class="flex items-center justify-between w-full pr-2 py-1">
              <span class="flex items-center gap-2">
                <el-icon :class="data.iconColor" class="text-lg"><UserFilled v-if="data.isUser" /><User v-else /></el-icon>
                <span :class="{'text-gray-200': data.active, 'text-gray-500': !data.active}">{{ node.label }}</span>
              </span>
              <el-tag v-if="data.status" :type="data.statusType" size="small" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90 origin-right"
                       :class="{'!text-green-500': data.status==='active', '!text-yellow-500': data.status==='running', '!text-gray-500': data.status==='pending'}"
              >{{ data.status }}</el-tag>
            </div>
          </template>
        </el-tree>
      </el-card>

      <!-- Task Board -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] flex-1 min-h-0 flex flex-col body-flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">任务看板 (Task Board)</div>
          </div>
        </template>

        <div class="flex-1 overflow-y-auto">
          <div class="text-xs mb-6">
            <div class="flex justify-between items-start mb-2">
              <span class="text-gray-400">Goal: <span class="text-gray-200">{{ activeSession?.goal || '未选择会话' }}</span></span>
              <el-tag size="small" effect="dark" class="!bg-[#1e3a8a] !border-none !text-blue-300 scale-90">IN_PROGRESS</el-tag>
            </div>
            <div class="flex items-center justify-between mt-4 mb-1">
              <span class="text-gray-400">Progress</span>
              <span class="text-gray-400">{{ tasks.filter(t=>t.status==='done').length }} / {{ tasks.length }} ({{ progress }}%)</span>
            </div>
            <el-progress :percentage="progress" :show-text="false" class="custom-progress" />
          </div>

          <div class="flex text-xs text-gray-500 mb-2 px-2">
            <div class="flex-1">子任务列表</div>
            <div class="w-24">接收方</div>
            <div class="w-20 text-right">状态</div>
          </div>

          <div class="space-y-1 mb-6">
            <div v-if="!tasks.length" class="text-gray-500 text-xs py-3 text-center">暂无子任务，等待 DomainAgent 拆解</div>
            <div v-for="(task, index) in tasks" :key="index" class="flex items-center text-xs p-2 hover:bg-[#2a2d35] rounded transition-colors group">
              <div class="flex-1 flex items-center gap-2 truncate pr-2" :class="{'text-gray-200': task.status !== 'pending', 'text-gray-500': task.status === 'pending'}">
                <span class="text-gray-500">{{ index + 1 }}.</span>
                {{ task.title || task.name }}
              </div>
              <div class="w-24 text-gray-400 truncate">{{ task.assignee }}</div>
              <div class="w-20 text-right flex items-center justify-end gap-1">
                <el-icon v-if="task.status === 'done' || task.status === 'completed'" class="text-green-500"><Check /></el-icon>
                <el-icon v-else-if="task.status === 'in_progress' || task.status === 'running'" class="text-blue-500 is-loading"><Loading /></el-icon>
                <el-icon v-else-if="task.status === 'blocked'" class="text-yellow-500"><Warning /></el-icon>
                <el-icon v-else class="text-gray-600"><Clock /></el-icon>
                <span :class="{'text-green-500': task.status==='done'||task.status==='completed', 'text-blue-500': task.status==='in_progress'||task.status==='running', 'text-yellow-500': task.status==='blocked', 'text-gray-500': task.status==='pending'}">{{ task.status }}</span>
              </div>
            </div>
          </div>

          <div class="border-t border-[#2a2d35] pt-4">
            <div class="text-xs font-bold text-gray-400 mb-3">约束条件 (Constraints)</div>
            <div class="space-y-2 text-xs">
              <div v-if="!constraints.length" class="text-gray-500 py-2 text-center">暂无约束条件</div>
              <div v-for="(c, i) in constraints" :key="i" class="flex justify-between p-2 bg-[#0f1115] rounded">
                <span class="text-gray-500">{{ c[0] }}</span>
                <span class="text-gray-200">{{ c[1] }}</span>
              </div>
            </div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Middle: Tabs -->
    <div class="flex-1 flex flex-col bg-[#1a1d24] border border-[#2a2d35] rounded-lg overflow-hidden min-w-0">
      <div class="h-12 border-b border-[#2a2d35] flex items-center px-4 bg-[#14161a] gap-6 text-sm shrink-0">
        <span @click="activeTab = 'log'" :class="activeTab === 'log' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer"
        >
          <el-icon class="mr-1"><Document /></el-icon> 执行日志
        </span>
        <span @click="activeTab = 'memory'" :class="activeTab === 'memory' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer"
        >
          <el-icon class="mr-1"><List /></el-icon> 记忆浏览器
        </span>
        <span @click="activeTab = 'skill'" :class="activeTab === 'skill' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer"
        >
          <el-icon class="mr-1"><Connection /></el-icon> Skill 装配
        </span>
        <span @click="activeTab = 'file'" :class="activeTab === 'file' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer"
        >
          <el-icon class="mr-1"><Files /></el-icon> 文件预览
        </span>
        <span @click="activeTab = 'logs'" :class="activeTab === 'logs' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer"
        >
          <el-icon class="mr-1"><Tickets /></el-icon> 日志分析
        </span>
      </div>

      <div class="flex-1 overflow-hidden relative flex flex-col">
        <ExecutionLog v-if="activeTab === 'log'" :events="activeSession?.events || []" />
        <MemoryExplorer v-if="activeTab === 'memory'" :session-id="activeSession?.id || ''" :agents="agents" />
        <SkillSet v-if="activeTab === 'skill'" :agents="agents" />
        <FilePreview v-if="activeTab === 'file'" :session-id="activeSession?.id || ''" :agents="agents" />
        <div v-if="activeTab === 'logs'" class="flex-1 overflow-hidden flex flex-col p-4">
          <div class="flex items-center gap-3 mb-3 shrink-0">
            <el-input v-model="logFilterAgent" placeholder="Agent 过滤" size="small" class="w-40" />
            <el-select v-model="logFilterLevel" placeholder="Level" size="small" class="w-28">
              <el-option label="全部" value="" />
              <el-option label="info" value="info" />
              <el-option label="warn" value="warn" />
              <el-option label="error" value="error" />
            </el-select>
            <el-button size="small" type="primary" @click="applyLogFilters">查询</el-button>
          </div>
          <div class="flex-1 overflow-y-auto space-y-2 pr-1">
            <div v-if="!sessionLogs.length" class="text-gray-500 text-sm text-center py-10">暂无结构化日志</div>
            <div v-for="log in sessionLogs" :key="log.id" class="text-xs border border-[#2a2d35] rounded p-2 bg-[#14161a]">
              <div class="flex items-center justify-between mb-1">
                <div class="flex items-center gap-2">
                  <el-tag size="small" :type="log.level === 'error' ? 'danger' : log.level === 'warn' ? 'warning' : 'info'" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90 origin-left">{{ log.level }}</el-tag>
                  <span class="text-gray-400">{{ log.phase }}</span>
                  <span class="text-gray-500">{{ log.agent }}</span>
                </div>
                <span class="text-gray-500">{{ fmtDateTime(log.created_at) }}</span>
              </div>
              <div class="text-gray-200 mb-1">{{ log.message }}</div>
              <div v-if="log.input_tokens || log.output_tokens" class="text-gray-500 mb-1">tokens: {{ log.input_tokens }} / {{ log.output_tokens }} · latency: {{ log.latency_ms }}ms · model: {{ log.model || '-' }}</div>
              <div v-if="log.prompt || log.response" class="mt-2">
                <el-button link size="small" type="primary" @click="expandedLogId = expandedLogId === log.id ? null : log.id">
                  {{ expandedLogId === log.id ? '收起' : '展开 Prompt/Response' }}
                </el-button>
                <div v-if="expandedLogId === log.id" class="mt-2 space-y-2">
                  <div v-if="log.prompt" class="bg-[#0f1115] p-2 rounded text-gray-400 whitespace-pre-wrap">{{ log.prompt }}</div>
                  <div v-if="log.response" class="bg-[#0f1115] p-2 rounded text-gray-400 whitespace-pre-wrap">{{ log.response }}</div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- Right: Metrics & Health -->
    <div class="w-[320px] flex flex-col gap-4 overflow-y-auto shrink-0 pl-1">
      <!-- Metrics -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">实时指标 (Metrics)</div>
          </div>
        </template>

        <div class="text-xs text-gray-400 mb-2">LLM 调用统计</div>
        <div class="grid grid-cols-4 gap-2 mb-6">
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">总调用</div>
            <div class="text-xl font-bold text-gray-200">{{ metrics?.calls ?? 0 }}<span class="text-xs font-normal ml-1">次</span></div>
          </div>
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">超时</div>
            <div class="text-xl font-bold text-red-400">{{ metrics?.timeouts ?? 0 }}<span class="text-xs font-normal ml-1">次</span></div>
          </div>
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">平均耗时</div>
            <div class="text-xl font-bold text-green-400">{{ metrics?.avg_duration ?? '-' }}</div>
          </div>
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">最长耗时</div>
            <div class="text-xl font-bold text-gray-200">{{ metrics?.max_duration ?? '-' }}</div>
          </div>
        </div>

        <div class="text-xs text-gray-400 mb-2">上下文用量</div>
        <div class="mb-6 text-xs">
          <div class="flex justify-between mb-2">
            <span>当前会话 Token 消耗</span>
            <span class="text-blue-400 font-bold">{{ metrics && metrics.total_tokens > 0 ? Math.min(100, Math.round(metrics.total_tokens / 128000 * 100)) : 0 }}%</span>
          </div>
          <div class="text-gray-500 mb-2">{{ (metrics?.total_tokens ?? 0).toLocaleString() }} / 128,000 <span class="text-[10px]">tokens</span></div>
          <div class="relative pt-1">
            <el-progress :percentage="metrics && metrics.total_tokens > 0 ? Math.min(100, Math.round(metrics.total_tokens / 128000 * 100)) : 0" :show-text="false" class="custom-progress" />
            <div class="absolute top-0 bottom-0 left-[80%] border-l-2 border-yellow-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
            <div class="absolute top-0 bottom-0 left-[95%] border-l-2 border-red-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
            <div class="flex justify-between mt-1 text-[10px]">
              <span class="text-yellow-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-yellow-500"></div> 80% 警告线</span>
              <span class="text-red-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-red-500"></div> 95% 硬限制</span>
            </div>
          </div>
        </div>

        <div class="text-xs text-gray-400 mb-2">看门狗状态</div>
        <div class="space-y-1 text-xs">
          <div v-for="d in watchdogDecisions.slice(0, 5)" :key="d.agent_id + d.occurred_at" class="flex items-center gap-4">
            <span class="text-gray-500 w-12">{{ fmtTime(d.occurred_at) }}</span>
            <el-tag size="small" :type="watchdogTagType(d.level)" effect="plain" class="!bg-transparent !border-[#2a2d35] w-16 text-center">{{ d.level }}</el-tag>
            <span class="text-gray-300 truncate flex-1">{{ watchdogLabel(d.level) }}</span>
          </div>
          <div v-if="!watchdogDecisions.length" class="text-gray-500 text-xs">暂无看门狗决策</div>
        </div>
      </el-card>

      <!-- Token Metrics -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">Token 消耗 (Token Metrics)</div>
          </div>
        </template>
        <div class="text-xs text-gray-400 mb-2">总计</div>
        <div class="grid grid-cols-3 gap-2 mb-4 text-center">
          <div>
            <div class="text-xs text-gray-500">Input</div>
            <div class="text-lg font-bold text-gray-200">{{ (tokenMetrics?.total_input_tokens ?? 0).toLocaleString() }}</div>
          </div>
          <div>
            <div class="text-xs text-gray-500">Output</div>
            <div class="text-lg font-bold text-gray-200">{{ (tokenMetrics?.total_output_tokens ?? 0).toLocaleString() }}</div>
          </div>
          <div>
            <div class="text-xs text-gray-500">Calls</div>
            <div class="text-lg font-bold text-gray-200">{{ tokenMetrics?.total_calls ?? 0 }}</div>
          </div>
        </div>
        <div class="text-xs text-gray-400 mb-2">按 Agent / Model</div>
        <div class="space-y-1 text-xs">
          <div v-for="s in tokenMetrics?.stats || []" :key="s.agent + '|' + s.model" class="flex justify-between p-2 bg-[#0f1115] rounded">
            <span class="text-gray-400 truncate flex-1">{{ s.agent }} <span v-if="s.model" class="text-gray-600">({{ s.model }})</span></span>
            <span class="text-gray-200">{{ s.input_tokens + s.output_tokens }}</span>
          </div>
          <div v-if="!tokenMetrics?.stats?.length" class="text-gray-500 text-xs text-center py-2">暂无 token 数据</div>
        </div>
      </el-card>

      <!-- Mailbox -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">邮箱通知 (Mailbox)</div>
          </div>
        </template>
        <div class="flex justify-between text-xs mb-4">
          <span class="text-gray-400">未读消息 ({{ mailboxMessages.length }})</span>
        </div>
        <div class="space-y-4">
          <div v-for="mail in mailboxMessages.slice(0, 8)" :key="mail.id" class="flex gap-3 text-xs">
            <div class="mt-0.5 rounded-full p-1 shrink-0 bg-gray-800/50">
              <el-icon class="text-gray-400"><component :is="mailboxIcon(mail.type)" /></el-icon>
            </div>
            <div class="flex-1 min-w-0">
              <div class="flex justify-between mb-1">
                <span class="font-bold" :class="'text-' + mailboxTagType(mail.type) + '-500'">{{ mail.type }}</span>
                <span class="text-gray-500">{{ fmtDateTime(mail.created_at) }}</span>
              </div>
              <div class="text-gray-300 truncate">{{ mail.subject }}</div>
              <div class="text-gray-500 mt-1 truncate">{{ mail.from }} → {{ mail.to }}</div>
            </div>
          </div>
          <div v-if="!mailboxMessages.length" class="text-gray-500 text-xs">暂无未读消息</div>
        </div>
      </el-card>

      <!-- System Health -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">系统状态 (System Health)</div>
          </div>
        </template>
        <div class="space-y-2 text-xs">
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><Coin /></el-icon>
            <div class="w-16 text-gray-300">Postgres</div>
            <div class="w-16" :class="healthDotClass(health?.postgres)">{{ healthStatusText(health?.postgres) }}</div>
            <div class="text-gray-500 flex-1">{{ health?.postgres?.online ? `延迟: ${health?.postgres?.latency_ms}ms` : '未连接' }}</div>
          </div>
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><DataLine /></el-icon>
            <div class="w-16 text-gray-300">Redis</div>
            <div class="w-16" :class="healthDotClass(health?.redis)">{{ healthStatusText(health?.redis) }}</div>
            <div class="text-gray-500 flex-1">{{ health?.redis?.online ? `延迟: ${health?.redis?.latency_ms}ms` : '未连接' }}</div>
          </div>
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><Connection /></el-icon>
            <div class="w-16 text-gray-300">LLM API</div>
            <div class="w-16" :class="healthDotClass(health?.llm)">{{ healthStatusText(health?.llm) }}</div>
            <div class="text-gray-500 flex-1 truncate">{{ health?.llm?.detail || '未配置' }}</div>
          </div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<style scoped>
:deep(.custom-tree .el-tree-node__content) {
  background-color: transparent !important;
  height: 32px;
}
:deep(.custom-tree .el-tree-node__content:hover) {
  background-color: #2a2d35 !important;
}
:deep(.custom-tree .el-tree-node:focus > .el-tree-node__content) {
  background-color: transparent !important;
}

:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 0;
}
:deep(.body-flex-1 .el-card__body > div) {
  padding: 16px;
}

:deep(.custom-progress .el-progress-bar__outer) {
  background-color: #2a2d35;
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: #1e3a8a;
}
</style>
