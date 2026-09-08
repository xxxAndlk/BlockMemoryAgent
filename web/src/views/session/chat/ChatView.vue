<script setup lang="ts">
import type { Session, SessionEvent, AgentNode, ClarifyOption, WireImage } from '@/types'
import ChatHeader from './components/ChatHeader.vue'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  clarify: { options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null
  sending: boolean
  inputTokens: number
  outputTokens: number
  liveStreaming: string
  liveThinking: string
}>()

const emit = defineEmits<{
  submit: [content: string, images: WireImage[]]
  cancel: []
  stop: []
  interrupt: []
  'new-session': []
  'clarify-submitted': []
}>()
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <ChatHeader :session="session" :agents="agents"
                @cancel="emit('cancel')" @stop="emit('stop')" @interrupt="emit('interrupt')" />
    <MessageList :events="events" :verbose="false" :clarify="clarify"
                 :session-id="session?.id || ''"
                 :live-streaming="liveStreaming" :live-thinking="liveThinking"
                 @submit-clarify="emit('clarify-submitted')" />
    <ChatInput :loading="sending"
               :session-active="session?.status === 'running'"
               :input-tokens="inputTokens"
               :output-tokens="outputTokens"
               @submit="(c: string, i: WireImage[]) => emit('submit', c, i)"
               @new-session="emit('new-session')" />
  </div>
</template>
