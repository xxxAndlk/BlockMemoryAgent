<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session, AgentNode, TrustMode, SessionGear, SessionThinking } from '@/types'
import { setTrustMode, setSessionGear, setSessionThinking } from '@/api/session'

const props = defineProps<{
  session: Session | null
  agents: AgentNode[]
}>()

const emit = defineEmits<{ (e: 'cancel'): void; (e: 'stop'): void; (e: 'interrupt'): void }>()

// 信任模式（TODO 第10⑥ 三级信任，对标 Codex）：三态下拉，切换即时 POST 后端，
// 下一工具调用生效。本地值以会话快照回显（trust_mode 空 = 后端回退现网语义，显示 full-auto）。
const trustMode = ref<TrustMode>('full-auto')
watch(
  () => props.session?.trust_mode,
  (m) => { trustMode.value = m === 'suggest' || m === 'auto-edit' ? m : 'full-auto' },
  { immediate: true }
)

async function onTrustModeChange(mode: TrustMode) {
  if (!props.session) return
  try {
    await setTrustMode(props.session.id, mode)
    trustMode.value = mode
    ElMessage.success(`信任模式已切换为 ${mode}（下一工具调用生效）`)
  } catch (e) {
    ElMessage.error('切换信任模式失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// 执行档位（TODO #14 三档全手动）：fast=文档助手直达 / daily=DomainAgent 直接执行 / cluster=Meta 全装编排。
// 手动切档任意向、原子即时生效；在飞子 Agent 不强杀，下轮按新档选角色（同 trustMode 语义）。
// 本地值以会话快照回显（gear 空 = 未设置，显示默认 daily）。
const gear = ref<SessionGear>('daily')
watch(
  () => props.session?.gear,
  (g) => { gear.value = g === 'fast' || g === 'cluster' ? g : 'daily' },
  { immediate: true }
)

const GEAR_LABEL: Record<SessionGear, string> = { fast: '快速', daily: '日常', cluster: '集群' }

async function onGearChange(v: SessionGear) {
  if (!props.session) return
  try {
    await setSessionGear(props.session.id, v)
    gear.value = v
    ElMessage.success(`执行档位已切换为${GEAR_LABEL[v]}档（即时生效，下一轮按新档选角色）`)
  } catch (e) {
    ElMessage.error('切换执行档位失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// 会话级思考强度（2026-09-16）：空 = 跟随角色默认；切换即时 POST 后端，下一次 LLM 调用生效（热）。
// 只影响本会话顶层 Agent（meta/domain/doc_assistant 按档位），在飞子 Agent 不受影响。
const thinking = ref<SessionThinking>('')
watch(
  () => props.session?.thinking,
  (t) => { thinking.value = t === 'off' || t === 'low' || t === 'medium' || t === 'high' ? t : '' },
  { immediate: true }
)

async function onThinkingChange(v: SessionThinking) {
  if (!props.session) return
  try {
    await setSessionThinking(props.session.id, v)
    thinking.value = v
    ElMessage.success(`思考强度已切换为 ${v === '' ? '跟随角色默认' : v}（下一次 LLM 调用生效）`)
  } catch (e) {
    ElMessage.error('切换思考强度失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

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
      <!-- 信任模式三态下拉（TODO 第10⑥）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主 -->
      <el-select v-if="session" :model-value="trustMode" size="small" class="!w-32 shrink-0"
                 title="信任模式：变更类操作的审批档位，切换下一工具调用生效"
                 @update:model-value="onTrustModeChange($event as TrustMode)">
        <el-option value="suggest" label="suggest 逐条审批" />
        <el-option value="auto-edit" label="auto-edit 审命令" />
        <el-option value="full-auto" label="full-auto 全自主" />
      </el-select>
      <!-- 执行档位三档下拉（TODO #14 三档全手动）：fast=文档助手直达 / daily=DomainAgent 直接执行 / cluster=Meta 全装编排 -->
      <el-select v-if="session" :model-value="gear" size="small" class="!w-24 shrink-0"
                 title="执行档位：快速档=文档助手直达，日常档=DomainAgent 直接执行，集群档=Meta 全装编排；手动切换即时生效"
                 @update:model-value="onGearChange($event as SessionGear)">
        <el-option value="fast" label="快速档" />
        <el-option value="daily" label="日常档" />
        <el-option value="cluster" label="集群档" />
      </el-select>
      <!-- 思考强度下拉（2026-09-16 会话级）：空=跟随角色默认；切换下一次 LLM 调用生效（热） -->
      <el-select v-if="session" :model-value="thinking" size="small" class="!w-28 shrink-0"
                 title="思考强度（本会话顶层 Agent）：空=跟随角色默认；只影响本会话顶层 Agent，切换下一次 LLM 调用生效"
                 @update:model-value="onThinkingChange($event as SessionThinking)">
        <el-option value="" label="思考 跟随角色" />
        <el-option value="off" label="思考 off" />
        <el-option value="low" label="思考 low" />
        <el-option value="medium" label="思考 medium" />
        <el-option value="high" label="思考 high" />
      </el-select>
    </div>

    <!-- Agent 链路（独立一行，横向滚动；chip 可点击选中，选中高亮，点 meta 清除选择） -->
    <div v-if="chain.length" class="flex items-center gap-1.5 text-xs text-ink-2 overflow-x-auto mt-1.5">
      <template v-for="(a, i) in chain" :key="a.inst_id">
        <button class="whitespace-nowrap rounded px-1 transition-colors hover:bg-page"
                :class="[nodeColor(a.type), a.inst_id === selectedId ? 'font-bold ring-1 ring-primary' : '']"
                title="查看该 Agent 的对话与监控"
                @click="handleChainClick(a)">{{ a.name }}</button>
        <el-icon v-if="i < chain.length - 1" class="text-ink-3 text-[10px]"><ArrowRight /></el-icon>
      </template>
    </div>
  </div>
</template>
