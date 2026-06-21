<script setup lang="ts">
import { ref, watch, nextTick, onMounted } from 'vue'
import type { SessionEvent } from '@/types'
import { groupEventsToTurns } from '../utils/turns'
import UserBubble from './UserBubble.vue'
import AssistantTurn from './AssistantTurn.vue'

const props = defineProps<{
  events: SessionEvent[]
  verbose?: boolean
}>()

const containerRef = ref<HTMLElement | null>(null)
const stickToBottom = ref(true)

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
      <div class="h-full flex flex-col items-center justify-center text-gray-500 text-sm gap-3">
        <el-icon class="text-5xl text-gray-700"><ChatLineRound /></el-icon>
        <div>选择一个会话开始查看，或在下方输入框直接下达新命令。</div>
      </div>
    </template>
    <template v-else>
      <div v-for="turn in groupEventsToTurns(events)" :key="turn.id">
        <UserBubble v-if="turn.userMessage" :event="turn.userMessage" />
        <AssistantTurn :turn="turn" :verbose="verbose" />
      </div>
    </template>
  </div>
</template>
