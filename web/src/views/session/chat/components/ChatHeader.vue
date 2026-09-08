<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { Session, AgentNode, TrustMode } from '@/types'
import { setTrustMode } from '@/api/session'

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

const statusColor = computed(() => {
  if (!props.session) return 'text-ink-2'
  switch (props.session.status) {
    case 'running': return props.session.destroy_at ? 'text-orange-400' : 'text-blue-400'
    case 'completed': return 'text-green-400'
    case 'error': return 'text-red-400'
    case 'paused_on_child': return 'text-yellow-400'
    case 'awaiting_clarify': return 'text-yellow-400'
    default: return 'text-ink-2'
  }
})

const statusLabel = computed(() => {
  if (!props.session) return '未连接'
  if (props.session.destroy_at) return '停止中'
  switch (props.session.status) {
    case 'running': return '运行中'
    case 'completed': return '已完成'
    case 'error': return '失败'
    case 'paused_on_child': return '子 Agent 暂停'
    case 'awaiting_clarify': return '待澄清'
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

function nodeColor(type: string) {
  if (type === 'meta') return 'text-blue-400'
  if (type === 'domain') return 'text-purple-400'
  if (type === 'subdomain') return 'text-fuchsia-400'
  if (type === 'assistant') return 'text-emerald-400'
  return 'text-ink-2'
}
</script>

<template>
  <div class="h-14 border-b border-line bg-card flex items-center px-6 gap-4 shrink-0">
    <div class="flex items-center gap-2 min-w-0 flex-1">
      <el-icon class="text-blue-400"><ChatLineRound /></el-icon>
      <span class="font-semibold text-ink truncate">
        {{ session?.goal || '尚未选择会话' }}
      </span>
      <el-tag size="small" effect="plain"
              class="!bg-transparent !border-line scale-90 shrink-0"
              :class="statusColor">{{ statusLabel }}</el-tag>
      <!-- 抢占中断：打断当前执行并注入新指令（对齐 TUI /interrupt） -->
      <el-button v-if="session?.status === 'running' && !session?.destroy_at" size="small"
                 class="!bg-transparent !border-line !text-ink-2 hover:!text-primary shrink-0"
                 @click="emit('interrupt')">
        <el-icon class="mr-1"><Pointer /></el-icon>中断
      </el-button>
      <!-- 软停止：可续跑（TODO #37）；硬终止走原 cancel（带确认） -->
      <el-button v-if="session?.status === 'running' && !session?.destroy_at" size="small"
                 class="!bg-amber-50 !border-amber-300 !text-amber-700 dark:!bg-orange-900/30 dark:!border-orange-700/40 dark:!text-orange-400 shrink-0"
                 @click="emit('stop')">
        <el-icon class="mr-1"><SwitchButton /></el-icon>软停止
      </el-button>
      <span v-if="destroyCountdown" class="text-xs text-orange-400 font-mono shrink-0 animate-pulse">
        {{ destroyCountdown }}
      </span>
      <el-button v-if="session?.status === 'running' && !session?.destroy_at" size="small"
                 class="!bg-red-50 !border-red-300 !text-red-700 dark:!bg-red-900/30 dark:!border-red-700/40 dark:!text-red-400 shrink-0"
                 @click="emit('cancel')">
        <el-icon class="mr-1"><CircleClose /></el-icon>终止
      </el-button>
      <span class="text-xs text-ink-2 truncate shrink-0">{{ session?.id || '' }}</span>

      <!-- 信任模式三态下拉（TODO 第10⑥）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主 -->
      <el-select v-if="session" :model-value="trustMode" size="small" class="!w-32 shrink-0"
                 title="信任模式：变更类操作的审批档位，切换下一工具调用生效"
                 @update:model-value="onTrustModeChange($event as TrustMode)">
        <el-option value="suggest" label="suggest 逐条审批" />
        <el-option value="auto-edit" label="auto-edit 审命令" />
        <el-option value="full-auto" label="full-auto 全自主" />
      </el-select>
    </div>

    <!-- Agent 链路 -->
    <div v-if="chain.length" class="flex items-center gap-1.5 text-xs text-ink-2 overflow-x-auto max-w-[60%]">
      <template v-for="(a, i) in chain" :key="a.inst_id">
        <span :class="nodeColor(a.type)" class="whitespace-nowrap">{{ a.name }}</span>
        <el-icon v-if="i < chain.length - 1" class="text-ink-3 text-[10px]"><ArrowRight /></el-icon>
      </template>
    </div>
  </div>
</template>
