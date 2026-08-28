<script setup lang="ts">
import { ref, onMounted, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session, AgentNode, TaskBoardData } from '@/types'
import {
  getSession,
  getSessionBoard,
  getSessionAgents,
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
import { useRoleTree } from '@/composables/useRoleTree'
import { useTaskBoard } from '@/composables/useTaskBoard'
import { useSessionList } from '@/composables/useSessionList'
import ExecutionLog from './components/ExecutionLog.vue'
import SkillSet from './components/SkillSet.vue'
import FilePreview from './components/FilePreview.vue'
import MetricsCard from './components/MetricsCard.vue'
import TokenMetricsCard from './components/TokenMetricsCard.vue'
import MailboxCard from './components/MailboxCard.vue'
import HealthCard from './components/HealthCard.vue'
import SessionLogsPanel from './components/SessionLogsPanel.vue'

const route = useRoute()
const { sessions, loadSessions } = useSessionList()
const stream = useSessionStream()
const panel = usePanelRefresh()

const activeSession = ref<Session | null>(null)
const activeTab = ref('log')
const loading = ref(false)

const agents = ref<AgentNode[]>([])
const board = ref<TaskBoardData | null>(null)
const { roleTree, defaultProps } = useRoleTree(agents)
const { tasks, constraints, taskProgress: progress } = useTaskBoard(board)

const metrics = ref<SessionMetrics | null>(null)
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

watch(() => route.query.id, (id) => {
  if (id && typeof id === 'string') selectSessionById(id)
})

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
  stream.close()
  panel.invalidate()
  startStream(s)
  await loadSessionPanels(s.id)
}

async function loadSessionPanels(sessionID: string) {
  const [boardRes, agentRes, metricsRes, mbRes, healthRes] = await Promise.allSettled([
    panel.run(() => getSessionBoard(sessionID)),
    panel.run(() => getSessionAgents(sessionID)),
    panel.run(() => getSessionMetrics(sessionID)),
    panel.run(() => getSessionMailbox(sessionID)),
    panel.run(() => getHealth()),
  ])

  board.value = boardRes.status === 'fulfilled' && boardRes.value ? boardRes.value.board || null : null
  agents.value = agentRes.status === 'fulfilled' && agentRes.value ? agentRes.value.agents || [] : []
  metrics.value = metricsRes.status === 'fulfilled' && metricsRes.value ? metricsRes.value : null
  mailboxMessages.value = mbRes.status === 'fulfilled' && mbRes.value ? mbRes.value.messages || [] : []
  health.value = healthRes.status === 'fulfilled' && healthRes.value ? healthRes.value : null

  await loadSessionLogs(sessionID)
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
  stream.startStream(s.id, {
    onSnapshot(snap) {
      if (activeSession.value?.id === snap.id) {
        activeSession.value = { ...activeSession.value, status: snap.status, events: snap.events || [] }
      }
    },
    onEvent(ev) {
      // 推到 activeSession.events（非 sessions 列表项的 s），避免污染共享对象
      if (activeSession.value && activeSession.value.id === s.id) {
        activeSession.value = { ...activeSession.value, events: [...(activeSession.value.events || []), ev] }
      }
    },
    onDone() {
      loadSessions()
    },
    onError(err) {
      console.error('SSE error:', err)
      ElMessage.error('实时连接异常，请检查网络或刷新页面')
    },
  })
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
                       :class="{'!text-green-500': data.status==='active' || data.status==='done', '!text-yellow-500': data.status==='running' || data.status==='delivered-unverified', '!text-red-500': data.status==='failed', '!text-gray-500': data.status==='pending'}"
              >{{ data.status === 'delivered-unverified' ? '已交付未验证' : data.status }}</el-tag>
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
                <el-icon v-else-if="task.status === 'delivered-unverified'" class="text-yellow-500"><Warning /></el-icon>
                <el-icon v-else-if="task.status === 'failed'" class="text-red-500"><CircleCloseFilled /></el-icon>
                <el-icon v-else-if="task.status === 'blocked'" class="text-yellow-500"><Warning /></el-icon>
                <el-icon v-else class="text-gray-600"><Clock /></el-icon>
                <span :class="{'text-green-500': task.status==='done'||task.status==='completed', 'text-blue-500': task.status==='in_progress'||task.status==='running', 'text-yellow-500': task.status==='delivered-unverified'||task.status==='blocked', 'text-red-500': task.status==='failed', 'text-gray-500': task.status==='pending'}">{{ task.status === 'delivered-unverified' ? '已交付未验证' : task.status }}</span>
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
        <SkillSet v-if="activeTab === 'skill'" :agents="agents" />
        <FilePreview v-if="activeTab === 'file'" :session-id="activeSession?.id || ''" :agents="agents" />
        <SessionLogsPanel
          v-if="activeTab === 'logs'"
          v-model:agent="logFilterAgent"
          v-model:level="logFilterLevel"
          v-model:expandedLogId="expandedLogId"
          :logs="sessionLogs"
          @query="applyLogFilters"
        />
      </div>
    </div>

    <!-- Right: Metrics & Health -->
    <div class="w-[320px] flex flex-col gap-4 overflow-y-auto shrink-0 pl-1">
      <MetricsCard :metrics="metrics" />
      <TokenMetricsCard :token-metrics="tokenMetrics" />
      <MailboxCard :messages="mailboxMessages" />
      <HealthCard :health="health" />
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
