<script setup lang="ts">
import { ref, watch, nextTick, onMounted, computed } from 'vue'
import type { SessionEvent, ClarifyPending } from '@/types'
import { groupEventsToTurns } from '../utils/turns'
import UserBubble from './UserBubble.vue'
import AssistantTurn from './AssistantTurn.vue'

const props = defineProps<{
  events: SessionEvent[]
  verbose?: boolean
  clarify?: ClarifyPending | null
  /** 问题②确认条（任务 140）：澄清答复已提交，底部展示「已收到，正在思考中…」 */
  clarifyAck?: boolean
  /** 批量问答逐题草稿（任务 140，下标对齐 clarify.questions；单题态为空数组） */
  clarifyDrafts?: string[]
  sessionId: string
  liveStreaming: string
  liveThinking: string
  /** 接替回合的流式文本快照（key=接替用 user_message 事件时间戳），见 groupEventsToTurns。 */
  priorReplies?: Record<string, string>
}>()

// 澄清选项提交成功 → 透传给父级（index.vue 置 running + ack，不再全量重载）
const emit = defineEmits<{
  (e: 'submit-clarify'): void
  (e: 'update-clarify-drafts', drafts: string[]): void
}>()

const containerRef = ref<HTMLElement | null>(null)
const stickToBottom = ref(true)

// F9 修复：原 groupEventsToTurns(events) 在模板内直接调用，
// 每次 patch 都重新 O(n) 分组。改 computed 仅在 events 变化时重算。
const turns = computed(() => groupEventsToTurns(props.events, props.priorReplies))

function onScroll() {
  if (!containerRef.value) return
  const el = containerRef.value
  stickToBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight < 80
}

function scrollToBottom() {
  if (!containerRef.value) return
  containerRef.value.scrollTop = containerRef.value.scrollHeight
}

watch(() => props.events.length, () => {
  if (stickToBottom.value) {
    nextTick(scrollToBottom)
  }
})

// 确认条出现/消失也维持吸底（条在内容流末尾，不跟会错过）
watch(() => props.clarifyAck, () => {
  if (stickToBottom.value) {
    nextTick(scrollToBottom)
  }
})

onMounted(() => {
  nextTick(scrollToBottom)
})

// 暴露给父组件，让用户从外部触发"跳到底部"
defineExpose({ scrollToBottom })
</script>

<template>
  <div ref="containerRef"
       class="flex-1 overflow-y-auto px-6 py-4 min-h-0 scroll-smooth"
       @scroll="onScroll">
    <template v-if="events.length === 0">
      <div class="h-full flex flex-col items-center justify-center text-ink-2 text-sm gap-3">
        <el-icon class="text-5xl text-ink-3"><ChatLineRound /></el-icon>
        <div>选择一个会话开始查看，或在下方输入框直接下达新命令。</div>
      </div>
    </template>
    <template v-else>
      <div v-for="(turn, ti) in turns" :key="turn.id">
        <UserBubble v-if="turn.userMessage" :event="turn.userMessage" />
        <AssistantTurn :turn="turn" :verbose="verbose" :clarify="clarify"
                       :clarify-drafts="clarifyDrafts" :session-id="sessionId"
                       :live-streaming="ti === turns.length - 1 ? liveStreaming : ''"
                       :live-thinking="ti === turns.length - 1 ? liveThinking : ''"
                       @submit-clarify="emit('submit-clarify')"
                       @update-clarify-drafts="(d: string[]) => emit('update-clarify-drafts', d)" />
      </div>
      <!-- 问题②确认条：澄清答复已提交，Agent 正在恢复执行（首个真实事件到达后由父级清除） -->
      <div v-if="clarifyAck" class="flex justify-center py-1.5">
        <span class="text-[11px] text-ink-2 flex items-center gap-1.5">
          <el-icon class="is-loading"><Loading /></el-icon>
          已收到答复，正在思考中…
        </span>
      </div>
    </template>
  </div>
</template>
