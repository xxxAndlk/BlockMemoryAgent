<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { Session, AgentNode } from '@/types'

const props = defineProps<{
  session: Session | null
  agents: AgentNode[]
}>()

const emit = defineEmits<{ (e: 'cancel'): void; (e: 'stop'): void; (e: 'interrupt'): void }>()

/** 用户主动"终止"的会话：后端落 error 态 + Result="cancelled by user"（见 service_react.cancel），
 *  但这是预期内的中止，不该和真失败共用红色"失败"——单列"已终止"。 */
const userCancelled = computed(
  () => props.session?.status === 'error' && (props.session?.result || '').includes('cancelled by user'),
)

const statusColor = computed(() => {
  if (!props.session) return 'text-ink-2'
  if (userCancelled.value) return 'text-ink-3'
  switch (props.session.status) {
    case 'running': return props.session.destroy_at ? 'text-orange-400' : 'text-blue-400'
    case 'completed': return 'text-green-400'
    case 'error': return 'text-red-400'
    case 'paused_on_child': return 'text-yellow-400'
    case 'awaiting_clarify': return 'text-yellow-400'
    case 'awaiting_child': return 'text-sky-400'
    default: return 'text-ink-2'
  }
})

const statusLabel = computed(() => {
  if (!props.session) return '未连接'
  if (userCancelled.value) return '已终止'
  if (props.session.destroy_at) return '停止中'
  switch (props.session.status) {
    case 'running': return '运行中'
    case 'completed': return '已完成'
    case 'error': return '失败'
    case 'paused_on_child': return '子 Agent 暂停'
    case 'awaiting_clarify': return '待澄清'
    case 'awaiting_child': return '挂起等待子 Agent'
    default: return props.session.status
  }
})

// 销毁倒计时（任务 21 遗留同步）：软停止后 destroy_at 非空，
// 每秒刷新"销毁 MM:SS"；续跑/到期后后端清空字段，倒计时消失。
const nowTs = ref(Date.now())
let timer: ReturnType<typeof setInterval> | null = null

onMounted(() => {
  timer = setInterval(() => { nowTs.value = Date.now() }, 1000)
})
onUnmounted(() => { if (timer) clearInterval(timer) })

const destroyCountdown = computed(() => {
  const at = props.session?.destroy_at
  if (!at) return ''
  const remain = new Date(at).getTime() - nowTs.value
  if (remain <= 0) return '销毁 00:00'
  const mm = Math.floor(remain / 60000)
  const ss = Math.floor((remain % 60000) / 1000)
  return `销毁 ${String(mm).padStart(2, '0')}:${String(ss).padStart(2, '0')}`
})

const chain = computed(() => {
  if (!props.agents.length) return [] as AgentNode[]
  // 按 type 排序: meta -> domain -> subdomain -> assistant
  const order = ['meta', 'domain', 'subdomain', 'assistant']
  return [...props.agents].sort((a, b) =>
    (order.indexOf(a.type) - order.indexOf(b.type)) || a.name.localeCompare(b.name))
})

// 存活判定（2026-10 用户诉求：续话反复新建子 Agent，顶栏只该看"还活着的"）：
// meta 常驻（回主会话入口）；其余 hot=true（运行中/热驻可唤醒）或 running 才算存活，
// 休眠（已销毁）与终态不再占展示位。hot 缺失的旧后端兜底按 running 算。
function isAlive(a: AgentNode): boolean {
  if (a.type === 'meta' || a.inst_id === 'meta') return true
  if (a.hot === undefined) return a.status === 'running'
  return a.hot || a.status === 'running'
}

const aliveChain = computed(() => chain.value.filter(isAlive))
const aliveCount = computed(
  () => aliveChain.value.filter((a) => a.type !== 'meta' && a.inst_id !== 'meta').length,
)

// 列表弹窗入口的显隐：有存活子 Agent，或当前选中着某个 Agent（可能已死，要留回主会话的路）才显示。
const route = useRoute()
const router = useRouter()
const selectedId = computed(() => (route.query.agent as string) || '')
const selectedAgent = computed(
  () => chain.value.find((a) => a.inst_id === selectedId.value) || null,
)
const showAgentEntry = computed(() => aliveCount.value > 0 || !!selectedId.value)
const agentListVisible = ref(false)

function handleChainClick(a: AgentNode) {
  const q = { ...route.query }
  if (a.type === 'meta' || a.inst_id === 'meta') delete q.agent
  else q.agent = a.inst_id
  void router.replace({ query: q })
  agentListVisible.value = false
}

function nodeColor(type: string) {
  if (type === 'meta') return 'text-blue-400'
  if (type === 'domain') return 'text-purple-400'
  if (type === 'subdomain') return 'text-fuchsia-400'
  if (type === 'assistant') return 'text-emerald-400'
  return 'text-ink-2'
}

/** 活/死指示：hot 缺失（旧后端）不额外标注；其余按 status+hot 给圆点与文案。 */
const TERMINAL_STATUS = new Set(['done', 'failed', 'error', 'cancelled', 'delivered-unverified'])
interface NodeLiveBadge { dot: string; text: string }

function nodeLiveBadge(a: AgentNode): NodeLiveBadge | null {
  if (a.hot === undefined) return null
  if (a.status === 'running') return { dot: 'bg-green-500 animate-pulse', text: '运行中' }
  if (a.status === 'idle') {
    return a.hot
      ? { dot: 'bg-yellow-500', text: '热驻' } // 可唤醒
      : { dot: 'bg-ink-3', text: '休眠' } // 已销毁，复用时自动冷恢复
  }
  if (TERMINAL_STATUS.has(a.status)) return { dot: 'bg-ink-3', text: '已结束' }
  return null // 未覆盖状态（paused 等）不强行归类
}

/** inst_id → 活/死徽标，模板按 key 取值避免重复计算。 */
const liveBadges = computed(() => {
  const m = new Map<string, NodeLiveBadge>()
  for (const a of chain.value) {
    const b = nodeLiveBadge(a)
    if (b) m.set(a.inst_id, b)
  }
  return m
})
</script>

<template>
  <!-- 单行布局：操作区 + 存活 Agent 列表弹窗入口（链条不再独立占行） -->
  <div class="border-b border-line bg-card px-6 py-2 shrink-0">
    <div class="flex items-center gap-2 min-w-0 flex-wrap">
      <el-icon class="text-blue-400"><ChatLineRound /></el-icon>
      <span class="font-semibold text-ink truncate">
        {{ session?.goal || '尚未选择会话' }}
      </span>
      <el-tag size="small" effect="plain"
              class="!bg-transparent !border-line scale-90 shrink-0"
              :class="statusColor">{{ statusLabel }}</el-tag>
      <!-- 抢占中断：打断当前执行并注入新指令（对齐 TUI /interrupt） -->
      <el-button v-if="(session?.status === 'running' || session?.status === 'awaiting_child') && !session?.destroy_at" size="small"
                 class="!bg-transparent !border-line !text-ink-2 hover:!text-primary shrink-0"
                 @click="emit('interrupt')">
        <el-icon class="mr-1"><Pointer /></el-icon>中断
      </el-button>
      <!-- 软停止：可续跑（TODO #37）；硬终止走原 cancel（带确认） -->
      <el-button v-if="(session?.status === 'running' || session?.status === 'awaiting_child') && !session?.destroy_at" size="small"
                 class="!bg-amber-50 !border-amber-300 !text-amber-700 dark:!bg-orange-900/30 dark:!border-orange-700/40 dark:!text-orange-400 shrink-0"
                 @click="emit('stop')">
        <el-icon class="mr-1"><SwitchButton /></el-icon>软停止
      </el-button>
      <span v-if="destroyCountdown" class="text-xs text-orange-400 font-mono shrink-0 animate-pulse">
        {{ destroyCountdown }}
      </span>
      <!-- 存活 Agent 列表入口（替代原顶部链条：只展示活着的，点开弹窗选择查看） -->
      <el-popover v-if="showAgentEntry" v-model:visible="agentListVisible"
                  placement="bottom-start" :width="320" trigger="click">
        <template #reference>
          <el-button size="small"
                     class="!bg-transparent !border-line !text-ink-2 hover:!text-primary shrink-0"
                     :class="selectedAgent ? '!text-primary !border-primary' : ''">
            <el-icon class="mr-1"><Connection /></el-icon>
            <span class="max-w-40 truncate">{{ selectedAgent ? selectedAgent.name : `Agent · ${aliveCount}` }}</span>
          </el-button>
        </template>
        <div class="text-xs">
          <div class="px-1 pb-1 text-ink-3">存活 Agent（{{ aliveCount }}）</div>
          <div class="max-h-72 overflow-y-auto">
            <button v-for="a in aliveChain" :key="a.inst_id"
                    class="w-full flex items-center gap-1.5 rounded px-2 py-1.5 text-left transition-colors hover:bg-page"
                    :class="a.inst_id === selectedId ? 'ring-1 ring-primary' : ''"
                    @click="handleChainClick(a)">
              <span v-if="liveBadges.get(a.inst_id)"
                    class="shrink-0 inline-block w-1.5 h-1.5 rounded-full"
                    :class="liveBadges.get(a.inst_id)!.dot"></span>
              <span class="flex-1 min-w-0 truncate" :class="nodeColor(a.type)">{{ a.name }}</span>
              <span v-if="liveBadges.get(a.inst_id)" class="shrink-0 text-[10px] text-ink-3">
                {{ liveBadges.get(a.inst_id)!.text }}
              </span>
            </button>
          </div>
        </div>
      </el-popover>
      <el-button v-if="(session?.status === 'running' || session?.status === 'awaiting_child') && !session?.destroy_at" size="small"
                 class="!bg-red-50 !border-red-300 !text-red-700 dark:!bg-red-900/30 dark:!border-red-700/40 dark:!text-red-400 shrink-0"
                 @click="emit('cancel')">
        <el-icon class="mr-1"><CircleClose /></el-icon>终止
      </el-button>
    </div>
  </div>
</template>
