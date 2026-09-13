<script setup lang="ts">
import type { Session, SessionEvent, AgentNode, ClarifyPending, WireImage } from '@/types'
import ChatHeader from './components/ChatHeader.vue'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'
import AgentChatPanel from '../orch/AgentChatPanel.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  /** 选中的非 meta Agent（?agent=）：有值时消息区+输入框换成该 Agent 的对话面板；
   *  未选中/选中 MetaAgent 时为 null，维持主会话对话（渲染逻辑不变） */
  agent?: AgentNode | null
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
  /** 接替回合的流式文本快照（key=接替用 user_message 事件时间戳），透传 MessageList → groupEventsToTurns */
  priorReplies?: Record<string, string>
  /** 是否已绑定会话（有 activeSession）：决定底部栏标签是本会话目录还是新会话默认目录 */
  sessionBound?: boolean
  /** 本会话工作目录（无会话时为新会话默认目录），底部栏展示 + 更改入口 */
  workDir?: string
  /** 工作目录保存中（按钮转圈） */
  workDirSaving?: boolean
  /** 最近使用过的目录（多会话共用同目录的快捷入口） */
  recentDirs?: string[]
}>()

const emit = defineEmits<{
  submit: [content: string, images: WireImage[]]
  cancel: []
  stop: []
  interrupt: []
  'new-session': []
  'clarify-submitted': []
  'update-clarify-drafts': [drafts: string[]]
  'update-workdir': [dir: string]
  refresh: []
}>()
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <ChatHeader :session="session" :agents="agents"
                @cancel="emit('cancel')" @stop="emit('stop')" @interrupt="emit('interrupt')" />
    <!-- 选中非 meta Agent：消息区+输入框换成该 Agent 的对话面板（多人对话 + 三态发送框） -->
    <AgentChatPanel v-if="agent" class="flex-1 min-h-0"
                    :session-id="session?.id || ''" :agent="agent" :agents="agents"
                    @refresh="emit('refresh')" />
    <template v-else>
      <MessageList :events="events" :verbose="false" :clarify="clarify" :agents="agents"
                   :clarify-ack="clarifyAck" :clarify-drafts="clarifyDrafts"
                   :session-id="session?.id || ''"
                   :session-status="session?.status"
                   :live-streaming="liveStreaming" :live-thinking="liveThinking"
                   :prior-replies="priorReplies"
                   @submit-clarify="emit('clarify-submitted')"
                   @update-clarify-drafts="(d: string[]) => emit('update-clarify-drafts', d)" />
      <ChatInput :loading="sending"
                 :session-active="session?.status === 'running' || session?.status === 'awaiting_clarify' || session?.status === 'awaiting_child'"
                 :inject-hint="session?.status === 'running'"
                 :input-tokens="inputTokens"
                 :output-tokens="outputTokens"
                 :session-bound="sessionBound"
                 :work-dir="workDir || ''"
                 :work-dir-saving="workDirSaving"
                 :recent-dirs="recentDirs"
                 @submit="(c: string, i: WireImage[]) => emit('submit', c, i)"
                 @new-session="emit('new-session')"
                 @update-workdir="(d: string) => emit('update-workdir', d)" />
    </template>
  </div>
</template>
