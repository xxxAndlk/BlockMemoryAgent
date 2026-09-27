<script setup lang="ts">
import { computed } from 'vue'
import type { Session, SessionEvent, AgentNode, ClarifyPending, WireImage, SessionGear, SessionThinking } from '@/types'
import type { AgentMailItem } from '@/api/session'
import ChatHeader from './components/ChatHeader.vue'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'
import AgentChatPanel from '../orch/AgentChatPanel.vue'
import { latestEventPhrase } from './utils/eventStyles'

const props = defineProps<{
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
  /** 会话级邮件留痕（全部 Agent 收发）：对话栏里把"上级 ↔ 下级"的往来显示出来 */
  mails?: AgentMailItem[]
  /** 是否已绑定会话（有 activeSession）：决定底部栏标签是本会话目录还是新会话默认目录 */
  sessionBound?: boolean
  /** 本会话工作目录（无会话时为新会话默认目录），底部栏展示 + 更改入口 */
  workDir?: string
  /** 工作目录保存中（按钮转圈） */
  workDirSaving?: boolean
  /** 已有工作目录（多会话共用同目录的快捷入口） */
  knownDirs?: string[]
}>()

const emit = defineEmits<{
  submit: [content: string, images: WireImage[], gear: SessionGear, thinking: SessionThinking]
  cancel: []
  stop: []
  interrupt: []
  'new-session': []
  'clarify-submitted': []
  'update-clarify-drafts': [drafts: string[]]
  'update-workdir': [dir: string]
  refresh: []
}>()

// 常驻最新状态行（TODO #15 翻译层① T10）：运行中在消息区下方展示最近事件的
// 人话短语（"正在执行工具操作"），比"运行中"标签更可感知。流式思考优先展示。
const latestStatus = computed(() => {
  if (props.liveThinking) return '正在思考…'
  return latestEventPhrase(props.events)
})

const showStatusLine = computed(() =>
  (props.session?.status === 'running' || props.session?.status === 'awaiting_child') && !!latestStatus.value,
)
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
                   :first-goal="session?.goal || ''"
                   :session-status="session?.status"
                   :live-streaming="liveStreaming" :live-thinking="liveThinking"
                   :prior-replies="priorReplies" :mails="mails"
                   @submit-clarify="emit('clarify-submitted')"
                   @update-clarify-drafts="(d: string[]) => emit('update-clarify-drafts', d)" />
      <!-- 常驻最新状态行（T10）：运行中展示最近事件的人话短语 -->
      <div v-if="showStatusLine"
           class="px-6 py-1 text-xs text-ink-3 border-t border-line bg-page/60 truncate shrink-0">
        {{ latestStatus }}
      </div>
      <ChatInput :loading="sending"
                 :session="session"
                 :session-active="session?.status === 'running' || session?.status === 'awaiting_clarify' || session?.status === 'awaiting_child'"
                 :inject-hint="session?.status === 'running'"
                 :input-tokens="inputTokens"
                 :output-tokens="outputTokens"
                 :session-bound="sessionBound"
                 :work-dir="workDir || ''"
                 :work-dir-saving="workDirSaving"
                 :known-dirs="knownDirs"
                 @submit="(c: string, i: WireImage[], g: SessionGear, t: SessionThinking) => emit('submit', c, i, g, t)"
                 @new-session="emit('new-session')"
                 @update-workdir="(d: string) => emit('update-workdir', d)" />
    </template>
  </div>
</template>
