<script setup lang="ts">
import type { Session, SessionEvent, AgentNode, ClarifyPending, WireImage } from '@/types'
import ChatHeader from './components/ChatHeader.vue'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  clarify: ClarifyPending | null
  /** 问题②确认条：澄清答复已提交，MessageList 底部展示「已收到，正在思考中…」 */
  clarifyAck?: boolean
  /** 批量问答逐题草稿（任务 140，下标对齐 clarify.questions；单题态为空数组） */
  clarifyDrafts?: string[]
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
  'update-clarify-drafts': [drafts: string[]]
}>()
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <ChatHeader :session="session" :agents="agents"
                @cancel="emit('cancel')" @stop="emit('stop')" @interrupt="emit('interrupt')" />
    <MessageList :events="events" :verbose="false" :clarify="clarify"
                 :clarify-ack="clarifyAck" :clarify-drafts="clarifyDrafts"
                 :session-id="session?.id || ''"
                 :live-streaming="liveStreaming" :live-thinking="liveThinking"
                 @submit-clarify="emit('clarify-submitted')"
                 @update-clarify-drafts="(d: string[]) => emit('update-clarify-drafts', d)" />
    <ChatInput :loading="sending"
               :session-active="session?.status === 'running' || session?.status === 'awaiting_clarify'"
               :input-tokens="inputTokens"
               :output-tokens="outputTokens"
               @submit="(c: string, i: WireImage[]) => emit('submit', c, i)"
               @new-session="emit('new-session')" />
  </div>
</template>
