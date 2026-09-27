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

// 链条 chip 可点击选中 Agent（?agent=<inst_id>，留在当前 tab）：对话/监控随选中切换；
// 点 meta 节点清除选择回主会话。当前选中 chip 高亮。
const route = useRoute()
const router = useRouter()
const selectedId = computed(() => (route.query.agent as string) || '')

function handleChainClick(a: AgentNode) {
  const q = { ...route.query }
  if (a.type === 'meta' || a.inst_id === 'meta') delete q.agent
  else q.agent = a.inst_id
  void router.replace({ query: q })
}

function nodeColor(type: string) {
  if (type === 'meta') return 'text-blue-400'
  if (type === 'domain') return 'text-purple-400'
  if (type === 'subdomain') return 'text-fuchsia-400'
  if (type === 'assistant') return 'text-emerald-400'
  return 'text-ink-2'
}

/** 切换条活/死指示：hot 缺失（旧后端）不额外标注；其余按 status+hot 给圆点与文案。 */
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
  <!-- 两行布局：操作区与 Agent 链路分行，中栏被侧栏挤窄时不再互相叠压 -->
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
      <el-button v-if="(session?.status === 'running' || session?.status === 'awaiting_child') && !session?.destroy_at" size="small"
                 class="!bg-red-50 !border-red-300 !text-red-700 dark:!bg-red-900/30 dark:!border-red-700/40 dark:!text-red-400 shrink-0"
                 @click="emit('cancel')">
        <el-icon class="mr-1"><CircleClose /></el-icon>终止
      </el-button>
    </div>

    <!-- Agent 链路（独立一行，横向滚动；chip 可点击选中，选中高亮，点 meta 清除选择） -->
    <div v-if="chain.length" class="flex items-center gap-1.5 text-xs text-ink-2 overflow-x-auto mt-1.5">
      <template v-for="(a, i) in chain" :key="a.inst_id">
        <button class="whitespace-nowrap rounded px-1 transition-colors hover:bg-page"
                :class="[nodeColor(a.type), a.inst_id === selectedId ? 'font-bold ring-1 ring-primary' : '']"
                :title="`查看该 Agent 的对话与监控${liveBadges.get(a.inst_id) ? ' · ' + liveBadges.get(a.inst_id)!.text : ''}`"
                @click="handleChainClick(a)">
          <span v-if="liveBadges.get(a.inst_id)"
                class="inline-block w-1.5 h-1.5 rounded-full align-middle mr-1"
                :class="liveBadges.get(a.inst_id)!.dot"></span>{{ a.name }}<span v-if="liveBadges.get(a.inst_id)"
                class="text-[10px] text-ink-3 ml-0.5">{{ liveBadges.get(a.inst_id)!.text }}</span></button>
        <el-icon v-if="i < chain.length - 1" class="text-ink-3 text-[10px]"><ArrowRight /></el-icon>
      </template>
    </div>
  </div>
</template>
