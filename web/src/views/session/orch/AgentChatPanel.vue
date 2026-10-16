<script setup lang="ts">
// AgentChatPanel.vue 单 Agent 完整对话面板 + 多态发送框（选中 Agent 的对话视图；
// 执行中发送走邮箱排队 queued=true，P0-2 steering）。
// 数据源：GET /sessions/:id/agents/:aid/messages（热层+PG 合并分页）+ POST .../message（用户直连）。
// 轮询策略与面板其余部分一致（3s 增量 after_seq），不新增推送通道（设计 §5）。
import { computed, h, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { AgentNode } from '@/types'
import { getAgentMessages, sendAgentMessage } from '@/api/session'
import type { AgentMessageItem, AgentMailItem } from '@/api/session'
import { pauseSessionAgent } from '@/api/metrics'
import { isChildWaiting } from '@/composables/useTreeLayout'
import { groupAgentMessagesToTurns } from './utils/agentTurns'
import { attachMailsToTurns } from '../chat/utils/mails'
import AgentTurnList from './AgentTurnList.vue'

const props = defineProps<{
  sessionId: string
  agent: AgentNode | null
  /** 会话内全部 Agent 节点：透传 AgentTurnList 解析多人对话发送者显示名 */
  agents?: AgentNode[]
}>()
const emit = defineEmits<{ refresh: [] }>()

const messages = ref<AgentMessageItem[]>([])
const mails = ref<AgentMailItem[]>([])
// 后端对话序号（复活重跑递增）：变了一律整表重载，见 pullIncrement 注释。
const runId = ref(-1)
const hasMore = ref(false)
const loading = ref(false)
const sending = ref(false)
const draft = ref('')

// props.agent 每 3s 面板刷新（refreshPanels 整表替换 agents）都会是新的对象引用：若 turns
// 直接依赖它，全量回合每 3s 重建一轮（无谓 re-render）。用只含展示字段的签名做闸门，签名不变
// 就不换快照——新增 agentTurns 读取的字段时记得同步加进这里。
const agentSig = computed(() => {
  const a = props.agent
  return a ? [a.inst_id, a.name, a.status, a.type, a.parent_id].join('|') : ''
})
const agentSnap = ref<AgentNode | null>(null)
watch(agentSig, () => { agentSnap.value = props.agent }, { immediate: true })

/** 消息流适配为对话页回合结构（思考链/工具卡/最终回答），computed 仅在消息/快照变化时重算。 */
const turns = computed(() => {
  const ts = groupAgentMessagesToTurns(messages.value, agentSnap.value)
  // 只挂**外发**邮件：入站邮件已在消息流里（`[mailbox from X]` 注入 → PeerBubble），
  // 再渲染一遍就是同一封信两个气泡。外发此前完全不可见——它只是一次被折叠的工具调用，
  // 「我怎么回上级的」无从查起（2026-09-17 用户实证）。
  const self = props.agent?.inst_id || ''
  attachMailsToTurns(ts, mails.value, (m) => !!self && m.from === self)
  return ts
})

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
    mails.value = res.mails
    runId.value = res.runId
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
    // 复活重跑检测：后端清了热层、新 run 从 seq 0 重编号，本地游标（旧最大值）此后
    // 永远取不到新消息——不整表重载就会一直停在旧对话上（用户实证："切换时闪一下新内容
    // 又变回旧的"）。runId 是权威判据；hotMaxSeq 兜底旧后端/Redis 冷启动。
    if (res.runId !== runId.value || (res.hotMaxSeq >= 0 && res.hotMaxSeq < lastSeq)) {
      await reload()
      return
    }
    mergeMessages(res.messages)
    // 外发邮件不在消息流里（增量只跟着 seq 走），轮询时整取一次留痕（信件量小，成本可忽略）
    if (res.mails.length || mails.value.length) mails.value = res.mails.length ? res.mails : mails.value
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
// 必须用"分源数组"形态而不是 `() => [props.sessionId, props.agent?.inst_id]`——后者每次
// 重建数组字面量都是新引用，Vue 按 Object.is 比较新旧值恒判"已变化"；而父组件每 3s 面板
// 刷新都会整表替换 agents（selectedAgent 换新对象 → props.agent 引用变化 → watch 依赖被
// 触发）→ 面板每 3s 清空重载一次（用户实证：对话区周期闪烁 + 滚动位置跳顶/跳底）。
// 分源数组逐项按值比较，只在真的换 Agent/会话时触发。
watch(
  [() => props.sessionId, () => props.agent?.inst_id],
  () => {
    messages.value = []
    mails.value = []
    runId.value = -1
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
// paused（用户停止后）可发：消息投箱并触发续跑恢复（2026-10-16，后端 ResumePausedWithMessage）。
const canSend = computed(
  () => sendState.value === 'waiting' || sendState.value === 'running' || sendState.value === 'done'
    || sendState.value === 'idle' || sendState.value === 'paused'
)
const placeholder = computed(() => {
  switch (sendState.value) {
    case 'waiting':
      return 'Agent 正在等下级返回，消息将立即注入并唤醒'
    case 'done':
      return '发送将复活该 Agent 并以消息为增量输入重跑'
    case 'running':
      return '正在执行任务——消息将排队入邮箱，下一步生效；点「停止」可停下'
    case 'paused':
      return '已停止——发送消息将继续执行'
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
    } else if (state === 'paused') {
      ElMessage.success('已恢复续跑')
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

// 停止按钮只在"该 Agent 运行中且类型为 domain"时出现（手动暂停仅 domain 支持；
// 叶子是短生命周期工人，消息注入/排队即够用）。2026-10-16 统一停止交互：发送区
// 按钮即唯一停止入口，头部原 中断/终止 图标按钮已移除。
const canStop = computed(
  () => sendState.value === 'running' && props.agent?.type === 'domain'
)

/**
 * 停止该子 Agent（弹窗带勾选框，默认不勾选）：
 * 不勾选 = 只停本 Agent——其运行中的子 Agent 继续跑完、结果入它邮箱，等恢复时 drain；
 * 勾选 = 连同其子 Agent 一起停止。停止后发送消息即自动恢复续跑（后端 Paused 分支）。
 */
async function handleStop() {
  const agent = props.agent
  if (!agent) return
  const picked = { stopAll: false }
  try {
    await ElMessageBox({
      title: '停止该 Agent？',
      message: h('div', { class: 'space-y-1.5 text-sm leading-relaxed', style: 'max-width: 380px' }, [
        h('div', null, '不勾选（默认）：只停这个 Agent——它正在运行的子 Agent 会继续执行，结果保留；停止后发送消息即可继续。'),
        h('label', { class: 'flex items-center gap-2 pt-1 cursor-pointer select-none' }, [
          h('input', {
            type: 'checkbox',
            class: 'accent-[var(--bma-primary)]',
            onChange: (e: Event) => { picked.stopAll = (e.target as HTMLInputElement).checked },
          }),
          h('span', null, '同时停止它的子 Agent'),
        ]),
      ]),
      showCancelButton: true,
      confirmButtonText: '停止',
      cancelButtonText: '取消',
      confirmButtonClass: 'el-button--danger',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await pauseSessionAgent(props.sessionId, agent.inst_id, { cascade: picked.stopAll })
    ElMessage.success('已停止；发送消息可继续')
    emit('refresh')
    // 立即拉一次状态与消息留痕，不等下一个 3s 面板刷新周期。
    await pullIncrement()
  } catch (e) {
    ElMessage.error('停止失败：' + (e instanceof Error ? e.message : String(e)))
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
    <!-- 头部：名称 + 状态 + 实例 ID（停止统一走发送区红色按钮，2026-10-16） -->
    <div class="flex items-center gap-2 px-3 py-2 border-b border-line shrink-0">
      <template v-if="agent">
        <span class="font-bold text-sm text-ink truncate">{{ agent.name || agent.inst_id }}</span>
        <span class="text-[11px] px-1.5 py-0.5 rounded shrink-0" :class="statusClass">{{ statusBadge }}</span>
        <span class="font-mono text-[11px] text-ink-3 truncate min-w-0">{{ agent.inst_id }}</span>
        <div class="ml-auto flex items-center gap-1 shrink-0">
          <el-icon v-if="loading" class="text-ink-3 animate-spin"><Loading /></el-icon>
        </div>
      </template>
      <span v-else class="text-xs text-ink-3">未选中 Agent</span>
    </div>

    <!-- 对话：回合式渲染（UserBubble/PeerBubble/AssistantTurn），分页与吸底在 AgentTurnList 内 -->
    <div v-if="agent" class="flex-1 flex flex-col min-h-0">
      <AgentTurnList :turns="turns" :session-id="sessionId" :agents="agents || []" :mail-self-id="agent?.inst_id || ''"
                     :has-more="hasMore" :loading="loading"
                     @load-earlier="loadEarlier" />
    </div>

    <!-- 发送框：运行中（domain）→ 红色「停止」；其余态 发送/复活（停止后发送即恢复续跑） -->
    <div v-if="agent" class="border-t border-line p-2 shrink-0">
      <div class="flex items-end gap-2">
        <textarea v-model="draft" rows="2" :disabled="!canSend || sending"
                  :placeholder="placeholder"
                  class="flex-1 resize-none text-xs bg-page border border-line rounded px-2 py-1.5 text-ink placeholder:text-ink-3 focus:outline-none focus:border-primary disabled:opacity-60"
                  @keydown.enter.exact.prevent="handleSend" />
        <button v-if="canStop" class="px-3 py-2 rounded text-xs font-bold shrink-0 flex items-center gap-1 bg-red-500 text-white hover:bg-red-600"
                title="停止该 Agent（可选择是否连同其子 Agent）；停止后发送消息可继续"
                @click="handleStop">
          <el-icon><VideoPause /></el-icon>
          <span>停止</span>
        </button>
        <button v-else class="px-3 py-2 rounded text-xs font-bold shrink-0 flex items-center gap-1"
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
