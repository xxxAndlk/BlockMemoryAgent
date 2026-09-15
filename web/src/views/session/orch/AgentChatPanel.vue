<script setup lang="ts">
// AgentChatPanel.vue 单 Agent 完整对话面板 + 多态发送框（选中 Agent 的对话视图；
// 执行中发送走邮箱排队 queued=true，P0-2 steering）。
// 数据源：GET /sessions/:id/agents/:aid/messages（热层+PG 合并分页）+ POST .../message（用户直连）。
// 轮询策略与面板其余部分一致（3s 增量 after_seq），不新增推送通道（设计 §5）。
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { AgentNode } from '@/types'
import { getAgentMessages, sendAgentMessage } from '@/api/session'
import type { AgentMessageItem } from '@/api/session'
import { cancelSessionAgent, pauseSessionAgent } from '@/api/metrics'
import { isChildWaiting } from '@/composables/useTreeLayout'
import { groupAgentMessagesToTurns } from './utils/agentTurns'
import AgentTurnList from './AgentTurnList.vue'

const props = defineProps<{
  sessionId: string
  agent: AgentNode | null
  /** 会话内全部 Agent 节点：透传 AgentTurnList 解析多人对话发送者显示名 */
  agents?: AgentNode[]
}>()
const emit = defineEmits<{ refresh: [] }>()

const messages = ref<AgentMessageItem[]>([])
const hasMore = ref(false)
const loading = ref(false)
const sending = ref(false)
const draft = ref('')

/** 消息流适配为对话页回合结构（思考链/工具卡/最终回答），computed 仅在消息变化时重算。 */
const turns = computed(() => groupAgentMessagesToTurns(messages.value, props.agent))

const PAGE = 100

/** 按 seq 去重合并（增量轮询与首次加载可能重叠，幂等合并最稳）。 */
function mergeMessages(incoming: AgentMessageItem[]) {
  if (!incoming.length) return
  const seen = new Set(messages.value.map((m) => m.seq))
  const fresh = incoming.filter((m) => !seen.has(m.seq))
  if (!fresh.length) return
  messages.value = [...messages.value, ...fresh].sort((a, b) => a.seq - b.seq)
}

async function reload() {
  const agent = props.agent
  if (!agent || !props.sessionId) {
    messages.value = []
    hasMore.value = false
    return
  }
  loading.value = true
  try {
    const res = await getAgentMessages(props.sessionId, agent.inst_id, { limit: PAGE })
    messages.value = res.messages
    hasMore.value = res.has_more
  } catch (e) {
    ElMessage.error('加载对话失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

/** 增量拉取（3s 轮询）：只取比当前最大 seq 更新的消息。 */
async function pullIncrement() {
  const agent = props.agent
  if (!agent || !props.sessionId || loading.value) return
  const lastSeq = messages.value.length ? messages.value[messages.value.length - 1].seq : -1
  // 尚无任何消息时走全量（after_seq=0 在后端语义是"取尾部"，会与轮询语义混淆）。
  if (lastSeq < 0) {
    await reload()
    return
  }
  try {
    const res = await getAgentMessages(props.sessionId, agent.inst_id, { afterSeq: lastSeq, limit: PAGE })
    mergeMessages(res.messages)
  } catch {
    // 轮询失败静默（下一周期重试），避免每 3s 弹一次错误。
  }
}

/** 上翻更早消息：before_seq = 当前最小 seq，结果前插。 */
async function loadEarlier() {
  const agent = props.agent
  if (!agent || !messages.value.length) return
  // 已到 seq 0：真正的最早一条，且后端把 before_seq<=0 当"取尾部"——就此收口，
  // 否则每次点击都会把尾部窗口重复前插（面板出现重复 seq 条目）。
  if (messages.value[0].seq <= 0) {
    hasMore.value = false
    return
  }
  loading.value = true
  try {
    const res = await getAgentMessages(props.sessionId, agent.inst_id, {
      beforeSeq: messages.value[0].seq,
      limit: PAGE,
    })
    if (res.messages.length) {
      const seen = new Set(messages.value.map((m) => m.seq))
      const older = res.messages.filter((m) => !seen.has(m.seq))
      messages.value = [...older, ...messages.value]
    }
    hasMore.value = res.has_more
  } catch (e) {
    ElMessage.error('加载更早消息失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// 切换 Agent/会话：清空并全量重载（旧 Agent 的消息不能留在新面板里）。
watch(
  () => [props.sessionId, props.agent?.inst_id],
  () => {
    messages.value = []
    hasMore.value = false
    void reload()
  },
  { immediate: true }
)

let timer: number | undefined
function startPolling() {
  stopPolling()
  timer = window.setInterval(() => void pullIncrement(), 3000)
}
function stopPolling() {
  if (timer !== undefined) {
    window.clearInterval(timer)
    timer = undefined
  }
}
startPolling()
onUnmounted(stopPolling)

/** 发送框状态（设计 §4 表 + P0-2 steering 排队）：waiting/running/终态/热驻均可发，仅 paused/meta 禁用。 */
const sendState = computed<'meta' | 'waiting' | 'running' | 'done' | 'paused' | 'idle'>(() => {
  const a = props.agent
  if (!a || a.inst_id === 'meta' || a.type === 'meta') return 'meta'
  if (a.status === 'running') return isChildWaiting(a) ? 'waiting' : 'running'
  if (['done', 'failed', 'cancelled', 'delivered-unverified'].includes(a.status)) return 'done'
  if (a.status === 'paused') return 'paused'
  return 'idle'
})

// idle（热驻待复用）可发：发送即唤醒热驻 Agent 续聊并刷新墙钟（后端 WakeIdleWithMessage）。
// running 可发（P0-2 steering）：消息入邮箱排队（后端 200{queued:true}），下一步生效。
const canSend = computed(
  () => sendState.value === 'waiting' || sendState.value === 'running' || sendState.value === 'done' || sendState.value === 'idle'
)
const placeholder = computed(() => {
  switch (sendState.value) {
    case 'waiting':
      return 'Agent 正在等下级返回，消息将立即注入并唤醒'
    case 'done':
      return '发送将复活该 Agent 并以消息为增量输入重跑'
    case 'running':
      return '正在执行任务——消息将排队入邮箱，下一步生效'
    case 'paused':
      return '已暂停——请到监控页恢复'
    case 'idle':
      return '热驻待复用——发送即唤醒该 Agent 续聊并刷新墙钟'
    default:
      return '主 Agent 请用主对话页'
  }
})

async function handleSend() {
  const agent = props.agent
  const content = draft.value.trim()
  if (!agent || !content || !canSend.value || sending.value) return
  const state = sendState.value
  sending.value = true
  try {
    const res = await sendAgentMessage(props.sessionId, agent.inst_id, content)
    draft.value = ''
    // queued=true：Agent 执行中，消息已入邮箱排队（区别于等子返回时的立即注入）。
    if (res.queued) {
      ElMessage.success('已排队：Agent 执行中，消息将在下一步生效')
    } else {
      ElMessage.success(state === 'done' ? '已请求复活重跑' : '已注入并唤醒')
    }
    emit('refresh')
    // 立即拉一次：注入/复活/排队留痕产生的消息不必等下一个 3s 周期。
    await pullIncrement()
  } catch (e) {
    // 后端 409 = 不可直连或注入失败竞态，如实回显原因而非笼统失败。
    ElMessage.error('发送失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    sending.value = false
  }
}

const canControl = computed(
  () => props.agent?.type === 'domain' && (props.agent?.status === 'running')
)

async function handlePause() {
  const agent = props.agent
  if (!agent) return
  try {
    await ElMessageBox.confirm('暂停并保留进度？（可在监控页恢复续跑）', '确认中断？', {
      confirmButtonText: '中断',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await pauseSessionAgent(props.sessionId, agent.inst_id)
    ElMessage.success('已请求中断')
    emit('refresh')
  } catch (e) {
    ElMessage.error('中断失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleCancel() {
  const agent = props.agent
  if (!agent) return
  try {
    await ElMessageBox.confirm('终止该 Agent？其未完成任务将标记失败，且不可续跑。', '确认终止？', {
      confirmButtonText: '终止',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await cancelSessionAgent(props.sessionId, agent.inst_id)
    ElMessage.success('已请求终止')
    emit('refresh')
  } catch (e) {
    ElMessage.error('终止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

const statusBadge = computed(() => {
  const s = props.agent?.status || ''
  const map: Record<string, string> = {
    // MetaAgent 的会话级活动态（RoleStatusActive），需与子 Agent 的 running 一样显示为执行中，
    // 否则头部徽章直接露出英文原值 "active"。
    active: '执行中',
    running: '执行中',
    done: '已完成',
    failed: '失败',
    cancelled: '已取消',
    paused: '已暂停',
    idle: '热驻待复用',
    'delivered-unverified': '未验证交付',
  }
  return map[s] || s
})

const statusClass = computed(() => {
  const s = props.agent?.status || ''
  if (s === 'done') return 'bg-green-50 text-green-600'
  if (s === 'failed') return 'bg-red-50 text-red-600'
  if (s === 'running' || s === 'active') return 'bg-primary-soft text-primary'
  if (s === 'delivered-unverified') return 'bg-amber-50 text-amber-600'
  return 'bg-page text-ink-3'
})
</script>

<template>
  <div class="flex flex-col h-full min-h-0">
    <!-- 头部：名称 + 状态 + 实例 ID + 中断/终止 -->
    <div class="flex items-center gap-2 px-3 py-2 border-b border-line shrink-0">
      <template v-if="agent">
        <span class="font-bold text-sm text-ink truncate">{{ agent.name || agent.inst_id }}</span>
        <span class="text-[11px] px-1.5 py-0.5 rounded shrink-0" :class="statusClass">{{ statusBadge }}</span>
        <span class="font-mono text-[11px] text-ink-3 truncate min-w-0">{{ agent.inst_id }}</span>
        <div class="ml-auto flex items-center gap-1 shrink-0">
          <el-icon v-if="loading" class="text-ink-3 animate-spin"><Loading /></el-icon>
          <template v-if="canControl">
            <button class="p-1 rounded hover:bg-page text-ink-2 hover:text-primary" title="中断（软停止，可恢复）" @click="handlePause">
              <el-icon><VideoPause /></el-icon>
            </button>
            <button class="p-1 rounded hover:bg-page text-ink-2 hover:text-red-500" title="终止（硬取消，不可恢复）" @click="handleCancel">
              <el-icon><CircleClose /></el-icon>
            </button>
          </template>
        </div>
      </template>
      <span v-else class="text-xs text-ink-3">未选中 Agent</span>
    </div>

    <!-- 对话：回合式渲染（UserBubble/PeerBubble/AssistantTurn），分页与吸底在 AgentTurnList 内 -->
    <div v-if="agent" class="flex-1 flex flex-col min-h-0">
      <AgentTurnList :turns="turns" :session-id="sessionId" :agents="agents || []"
                     :has-more="hasMore" :loading="loading"
                     @load-earlier="loadEarlier" />
    </div>

    <!-- 发送框（三态） -->
    <div v-if="agent" class="border-t border-line p-2 shrink-0">
      <div class="flex items-end gap-2">
        <textarea v-model="draft" rows="2" :disabled="!canSend || sending"
                  :placeholder="placeholder"
                  class="flex-1 resize-none text-xs bg-page border border-line rounded px-2 py-1.5 text-ink placeholder:text-ink-3 focus:outline-none focus:border-primary disabled:opacity-60"
                  @keydown.enter.exact.prevent="handleSend" />
        <button class="px-3 py-2 rounded text-xs font-bold shrink-0 flex items-center gap-1"
                :class="canSend && draft.trim() ? 'bg-primary text-white hover:opacity-90' : 'bg-page text-ink-3 cursor-not-allowed'"
                :disabled="!canSend || !draft.trim() || sending"
                @click="handleSend">
          <el-icon v-if="sending" class="animate-spin"><Loading /></el-icon>
          <span>{{ sendState === 'done' ? '复活' : '发送' }}</span>
        </button>
      </div>
      <div v-if="!canSend" class="text-[11px] text-ink-3 mt-1">{{ placeholder }}</div>
    </div>
  </div>
</template>
