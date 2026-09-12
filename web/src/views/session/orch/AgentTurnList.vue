<script setup lang="ts">
// AgentTurnList.vue 单 Agent 对话的回合式渲染（多人对话效果）：
// 复用对话页（chat）的 UserBubble（真用户，右对齐）+ PeerBubble（上级派发/调度器/其他
// Agent，左对齐，按发送者配色）+ AssistantTurn，按 Turn 结构渲染思考链/工具卡/最终回答。
// 编排数据源是轮询快照（无 SSE live 帧），故 liveStreaming/liveThinking 恒传 ''，
// clarify 恒传 null（单 Agent 直连不支持澄清交互）。
import { ref, watch, nextTick, onMounted } from 'vue'
import type { AgentNode } from '@/types'
import type { Turn } from '../chat/utils/turns'
import UserBubble from '../chat/components/UserBubble.vue'
import PeerBubble from '../chat/components/PeerBubble.vue'
import AssistantTurn from '../chat/components/AssistantTurn.vue'

const props = defineProps<{
  turns: Turn[]
  sessionId: string
  /** 会话内全部 Agent 节点：inst_id → 名称映射，解析发送者显示名 */
  agents?: AgentNode[]
  /** 头部还有更早消息（before_seq 翻页）：显示"加载更早消息"入口 */
  hasMore?: boolean
  loading?: boolean
}>()

const emit = defineEmits<{ (e: 'load-earlier'): void }>()

/** 发送者标识 → 显示名：子 Agent inst_id 查名称；系统来源用固定中文名；查不到用原值。 */
function senderLabel(sender: string): string {
  if (sender === 'dispatcher') return '调度器'
  if (sender.startsWith('verifyloop')) return '验证循环'
  return props.agents?.find((a) => a.inst_id === sender)?.name || sender
}

const containerRef = ref<HTMLElement | null>(null)
const stickToBottom = ref(true)

// stick-to-bottom（对齐 MessageList.vue）：贴底时新回合自动跟随，翻历史时不打扰。
function onScroll() {
  if (!containerRef.value) return
  const el = containerRef.value
  stickToBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 80
}

function scrollToBottom() {
  if (!containerRef.value) return
  containerRef.value.scrollTop = containerRef.value.scrollHeight
}

watch(
  () => props.turns.length,
  () => {
    if (stickToBottom.value) nextTick(scrollToBottom)
  }
)

// 进入面板直接落到底部（双帧兜底：首帧布局后图片/Markdown 异步撑高再校正一次），
// 不做平滑动画——容器禁用 scroll-behavior，否则 3s 轮询追加会反复播放滚动动画。
onMounted(() => {
  nextTick(() => {
    scrollToBottom()
    requestAnimationFrame(scrollToBottom)
  })
})

defineExpose({ scrollToBottom })
</script>

<template>
  <div ref="containerRef" class="flex-1 overflow-y-auto px-4 py-3 min-h-0" @scroll="onScroll">
    <div v-if="hasMore" class="text-center mb-2">
      <button class="text-xs text-primary hover:underline" :disabled="loading" @click="emit('load-earlier')">
        加载更早消息
      </button>
    </div>
    <div v-if="!turns.length && !loading" class="text-center text-xs text-ink-3 py-8">
      暂无消息（Agent 启动后逐条热写）
    </div>
    <div v-for="turn in turns" :key="turn.id">
      <template v-if="turn.userMessage && turn.userMessage.message">
        <!-- 真用户消息（右侧"你"）；其余发送者（上级派发/调度器/其他 Agent）左侧他人气泡 -->
        <UserBubble v-if="turn.userMessage.agent === 'user'" :event="turn.userMessage" />
        <PeerBubble v-else :event="turn.userMessage"
                    :label="senderLabel(turn.userMessage.agent)" :color-key="turn.userMessage.agent" />
      </template>
      <AssistantTurn :turn="turn" :clarify="null" :session-id="sessionId"
                     live-streaming="" live-thinking="" />
    </div>
  </div>
</template>
