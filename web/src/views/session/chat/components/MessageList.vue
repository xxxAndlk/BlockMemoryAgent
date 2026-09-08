<script setup lang="ts">
import { ref, watch, nextTick, onMounted, computed } from 'vue'
import type { SessionEvent, ClarifyOption } from '@/types'
import { groupEventsToTurns } from '../utils/turns'
import UserBubble from './UserBubble.vue'
import AssistantTurn from './AssistantTurn.vue'

const props = defineProps<{
  events: SessionEvent[]
  verbose?: boolean
  clarify?: { options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null
  sessionId: string
  liveStreaming: string
  liveThinking: string
}>()

// 澄清选项提交成功 → 透传给父级（index.vue 重开会话刷新事件流）
const emit = defineEmits<{ (e: 'submit-clarify'): void }>()

const containerRef = ref<HTMLElement | null>(null)
const stickToBottom = ref(true)

// F9 修复：原 groupEventsToTurns(events) 在模板内直接调用，
// 每次 patch 都重新 O(n) 分组。改 computed 仅在 events 变化时重算。
const turns = computed(() => groupEventsToTurns(props.events))

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
      <div v-for="turn in turns" :key="turn.id">
        <UserBubble v-if="turn.userMessage" :event="turn.userMessage" />
        <AssistantTurn :turn="turn" :verbose="verbose" :clarify="clarify" :session-id="sessionId"
                       :live-streaming="liveStreaming" :live-thinking="liveThinking"
                       @submit-clarify="emit('submit-clarify')" />
      </div>
    </template>
  </div>
</template>
