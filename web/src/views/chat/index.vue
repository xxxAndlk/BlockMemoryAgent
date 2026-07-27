<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session, SessionEvent, AgentNode, TaskBoardData } from '@/types'
import {
  createSession,
  sendMessage,
  clarifySession,
  cancelSession,
  getSession,
  getSessionAgents,
  getSessionBoard,
} from '@/api/session'
import { getSessionMetrics, type SessionMetrics } from '@/api/metrics'
import { useSessionStream } from '@/composables/useSessionStream'
import { usePanelRefresh } from '@/composables/usePanelRefresh'
import { useRoleTree } from '@/composables/useRoleTree'
import { useTaskBoard } from '@/composables/useTaskBoard'
import { useSessionList } from '@/composables/useSessionList'
import { useSessionStatus } from '@/composables/useSessionStatus'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'
import ChatHeader from './components/ChatHeader.vue'

const route = useRoute()
const router = useRouter()

const { sessions, loadSessions } = useSessionList()
const stream = useSessionStream()
const panel = usePanelRefresh()
const { statusDotClass, statusText } = useSessionStatus()

const activeSession = ref<Session | null>(null)
const events = ref<SessionEvent[]>([])
const agents = ref<AgentNode[]>([])
const metrics = ref<SessionMetrics | null>(null)
const board = ref<TaskBoardData | null>(null)

const { roleTree, defaultProps } = useRoleTree(agents)
const { tasks, taskProgress } = useTaskBoard(board)

const loading = ref(false)
const sending = ref(false)

// 用户偏好
const verbose = ref(false)
const memoryEnabled = ref(true)
const sessionFilter = ref('')

onMounted(async () => {
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
  try {
    const s = await getSession(id)
    activeSession.value = s
    events.value = [...(s.events || [])]
    localStorage.setItem('lastSessionID', id) // 记住上次活跃会话，刷新后恢复
    startStream(s)
    await refreshPanels(id)
  } catch (e) {
    localStorage.removeItem('lastSessionID') // 会话不存在则清除，避免反复加载失败
    activeSession.value = null
    ElMessage.error('会话不存在或加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function refreshPanels(id: string) {
  const agentsRes = await panel.run(() => getSessionAgents(id))
  if (agentsRes) agents.value = agentsRes.agents || []
  const boardRes = await panel.run(() => getSessionBoard(id))
  if (boardRes) board.value = boardRes.board || null
  const metricsRes = await panel.run(() => getSessionMetrics(id))
  if (metricsRes) metrics.value = metricsRes
}

function startStream(s: Session) {
  panel.startAutoRefresh(s.id, refreshPanels)
  stream.startStream(s.id, {
    onSnapshot(snap) {
      activeSession.value = snap
      events.value = [...(snap.events || [])]
    },
    onEvent(ev) {
      events.value.push(ev)
    },
    onDone(finalStatus?: string) {
      panel.stopPanelTimer()
      // 完成：使用后端 done 帧携带的真实 status，避免把 error/awaiting_clarify 误显示为 completed
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
      // SSE 错误必须重置 sending，否则发送按钮永久禁用（F2 修复）。
      sending.value = false
      ElMessage.error('实时连接异常，请检查网络或刷新页面')
    },
  })
}

async function handleSubmit(content: string) {
  if (!content.trim()) return
  sending.value = true
  try {
    // 1) 待澄清会话 → 调 /clarify 提交答复，复用同一会话
    if (activeSession.value && activeSession.value.status === 'awaiting_clarify') {
      await clarifySession(activeSession.value.id, content)
      activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
      await openSession(activeSession.value.id)
      return
    }
    // 2) 已有会话（running/completed/error）→ 追加消息到同一会话，不新开栏
    //    后端 POST /api/sessions/{id}/message 支持向已完成会话追加并 resumeSession。
    if (activeSession.value) {
      const wasRunning = activeSession.value.status === 'running'
      await sendMessage(activeSession.value.id, content)
      if (!wasRunning) {
        // 会话此前已完成/出错：onDone 已关闭 SSE，需重新打开会话以重建事件流并置 running
        activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
        await openSession(activeSession.value.id)
      }
      // wasRunning 时 SSE 仍在监听，会自动推送 user_message 事件
      return
    }
    // 3) 无选中会话 → 创建新会话
    const s = await createSession(content)
    sessions.value.unshift(s)
    router.replace({ path: '/chat', query: { id: s.id } })
    await openSession(s.id)
  } catch (e) {
    console.error('submit failed:', e)
    // API 异常时 SSE 不会建立，onDone 永不触发，必须在此重置 sending（F11 修复）。
    sending.value = false
    ElMessage.error('发送失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleCancel() {
  if (!activeSession.value) return
  try {
    await cancelSession(activeSession.value.id)
  } catch (e) {
    console.error('cancel failed:', e)
    ElMessage.error('取消会话失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleNewSession() {
  stream.close()
  panel.stopPanelTimer()
  events.value = []
  activeSession.value = null
  agents.value = []
  metrics.value = null
  board.value = null
  router.replace({ path: '/chat' })
}

const filteredSessions = computed(() => {
  const q = sessionFilter.value.trim().toLowerCase()
  if (!q) return sessions.value
  return sessions.value.filter(s =>
    s.id.toLowerCase().includes(q) ||
    (s.goal || '').toLowerCase().includes(q))
})

// 累加当前会话 token_usage 事件的输入/输出 token，供 ChatInput 实时展示。
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
  <div class="h-full flex gap-4 overflow-hidden text-gray-300">
    <!-- 左侧会话列表 -->
    <aside class="w-[280px] flex flex-col shrink-0 bg-[#1a1d24] border border-[#2a2d35] rounded-lg overflow-hidden">
      <div class="px-3 py-3 border-b border-[#2a2d35] flex items-center justify-between gap-2">
        <span class="text-sm font-bold text-gray-200">会话列表</span>
        <el-button size="small" plain class="!bg-transparent !border-[#2a2d35] !text-gray-400 hover:!text-white"
                   @click="handleNewSession">
          <el-icon><Plus /></el-icon>
        </el-button>
      </div>
      <div class="px-3 py-2 border-b border-[#2a2d35]">
        <el-input v-model="sessionFilter" size="small" placeholder="搜索会话…" class="!bg-[#0f1115] chat-search">
          <template #prefix><el-icon class="text-gray-500"><Search /></el-icon></template>
        </el-input>
      </div>

      <div class="flex-1 overflow-y-auto">
        <button v-for="s in filteredSessions" :key="s.id"
                @click="openSession(s.id)"
                class="w-full text-left px-3 py-2.5 border-b border-[#2a2d35]/60 hover:bg-[#2a2d35] transition-colors"
                :class="{'bg-[#1e3a8a]/30': activeSession?.id === s.id}">
          <div class="flex items-center gap-2 mb-1">
            <span class="w-1.5 h-1.5 rounded-full shrink-0" :class="statusDotClass(s.status)"></span>
            <span class="text-xs text-gray-500 font-mono shrink-0">{{ s.id }}</span>
            <span class="text-[10px] text-gray-500 ml-auto shrink-0">{{ statusText(s.status) }}</span>
          </div>
          <div class="text-sm text-gray-200 line-clamp-2 break-words">{{ s.goal || '(无目标)' }}</div>
          <div class="text-[10px] text-gray-500 mt-1">{{ fmtDateTime(s.started_at) }}</div>
        </button>

        <div v-if="!filteredSessions.length" class="text-center text-xs text-gray-500 py-10">
          暂无会话，在下方输入框直接下达命令即可创建。
        </div>
      </div>
    </aside>

    <!-- 中间对话区 -->
    <main class="flex-1 flex flex-col bg-[#1a1d24] border border-[#2a2d35] rounded-lg overflow-hidden min-w-0">
      <ChatHeader :session="activeSession" :agents="agents" @cancel="handleCancel" />
      <MessageList :events="events" :verbose="verbose" />
      <ChatInput :loading="sending"
                 :session-active="activeSession?.status === 'running'"
                 :input-tokens="tokenUsage.input"
                 :output-tokens="tokenUsage.output"
                 v-model:verbose="verbose"
                 v-model:memory-enabled="memoryEnabled"
                 @submit="handleSubmit"
                 @new-session="handleNewSession" />
    </main>

    <!-- 右侧上下文面板（精简版指标） -->
    <aside class="w-[280px] flex flex-col gap-4 shrink-0 overflow-y-auto">
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="text-sm font-bold text-gray-200">会话指标</div>
        </template>
        <div v-if="metrics" class="grid grid-cols-2 gap-3 text-xs">
          <div class="p-2 bg-[#0f1115] rounded border border-[#2a2d35]">
            <div class="text-gray-500 mb-1">LLM 调用</div>
            <div class="text-lg font-bold text-gray-200">{{ metrics.calls }}</div>
          </div>
          <div class="p-2 bg-[#0f1115] rounded border border-[#2a2d35]">
            <div class="text-gray-500 mb-1">超时</div>
            <div class="text-lg font-bold text-red-400">{{ metrics.timeouts }}</div>
          </div>
          <div class="p-2 bg-[#0f1115] rounded border border-[#2a2d35]">
            <div class="text-gray-500 mb-1">平均耗时</div>
            <div class="text-lg font-bold text-green-400">{{ metrics.avg_duration || '-' }}</div>
          </div>
          <div class="p-2 bg-[#0f1115] rounded border border-[#2a2d35]">
            <div class="text-gray-500 mb-1">总 Token</div>
            <div class="text-lg font-bold text-blue-400">{{ (metrics.total_tokens || 0).toLocaleString() }}</div>
          </div>
        </div>
        <div v-else class="text-xs text-gray-500">暂无指标数据</div>
      </el-card>

      <!-- Agent编排栏 - 角色层级 -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="text-sm font-bold text-gray-200">Agent编排 (Role Hierarchy)</div>
        </template>
        <div v-if="roleTree.length" class="text-xs">
          <el-tree
            :data="roleTree"
            :props="defaultProps"
            default-expand-all
            class="!bg-transparent custom-tree"
            :expand-on-click-node="false"
          >
            <template #default="{ node, data }">
              <div class="flex items-center justify-between w-full pr-1 py-0.5">
                <span class="flex items-center gap-1.5">
                  <el-icon :class="data.iconColor" class="text-sm">
                    <UserFilled v-if="data.isUser" /><User v-else />
                  </el-icon>
                  <span :class="{'text-gray-200': data.active, 'text-gray-500': !data.active}" class="text-xs">{{ node.label }}</span>
                </span>
                <el-tag v-if="data.status" :type="data.statusType" size="small" effect="plain"
                  class="!bg-transparent !border-[#2a2d35] scale-75 origin-right"
                  :class="{'!text-green-500': data.status==='active', '!text-yellow-500': data.status==='running', '!text-gray-500': data.status==='pending'}">
                  {{ data.status }}
                </el-tag>
              </div>
            </template>
          </el-tree>
        </div>
        <div v-else class="text-xs text-gray-500">暂无 Agent 实例，会话启动后自动创建</div>
      </el-card>

      <!-- 任务栏 - Task Board -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="text-sm font-bold text-gray-200">任务栏 (Task Board)</div>
            <el-progress v-if="tasks.length" :percentage="taskProgress" :show-text="false" class="w-20 custom-progress" />
          </div>
        </template>
        <div v-if="tasks.length" class="space-y-1.5 text-xs">
          <div v-for="(t, i) in tasks" :key="i"
               class="flex items-center gap-2 p-1.5 bg-[#0f1115] rounded border border-[#2a2d35]">
            <el-icon v-if="t.status === 'done'" class="text-green-500 text-sm"><CircleCheck /></el-icon>
            <el-icon v-else-if="t.status === 'running'" class="text-yellow-500 text-sm"><Loading /></el-icon>
            <el-icon v-else class="text-gray-500 text-sm"><CirclePlus /></el-icon>
            <span class="text-gray-200 truncate flex-1">{{ t.title || t.name }}</span>
            <span v-if="t.assignee" class="text-[10px] text-gray-500 shrink-0 font-mono">{{ t.assignee }}</span>
            <el-tag v-if="t.status === 'done'" size="small" type="success" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-75 origin-right">完成</el-tag>
            <el-tag v-else-if="t.status === 'running'" size="small" type="warning" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-75 origin-right">进行中</el-tag>
            <el-tag v-else size="small" type="info" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-75 origin-right">待办</el-tag>
          </div>
        </div>
        <div v-else class="text-xs text-gray-500">暂无任务数据</div>
      </el-card>

      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]" v-if="activeSession">
        <template #header>
          <div class="text-sm font-bold text-gray-200">操作</div>
        </template>
        <div class="space-y-2 text-xs">
          <router-link :to="{ path: '/session', query: { id: activeSession.id }}"
                       class="block px-2 py-1.5 bg-[#0f1115] rounded border border-[#2a2d35] hover:border-blue-500 text-gray-300 hover:text-white text-center transition-colors">
            <el-icon class="mr-1"><Monitor /></el-icon>
            查看完整监控面板
          </router-link>
        </div>
      </el-card>
    </aside>
  </div>
</template>

<style scoped>
:deep(.chat-search .el-input__wrapper) {
  background-color: #0f1115;
  box-shadow: 0 0 0 1px #2a2d35 inset;
}
:deep(.chat-search .el-input__wrapper.is-focus) {
  box-shadow: 0 0 0 1px #3b82f6 inset;
}

.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
