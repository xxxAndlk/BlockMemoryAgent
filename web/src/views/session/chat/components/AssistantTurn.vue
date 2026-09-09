<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { SessionEvent, ClarifyOption } from '@/types'
import type { Turn } from '../utils/turns'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import { clarifySession } from '@/api/session'
import ThinkChain from './ThinkChain.vue'
import ToolActivity from './ToolActivity.vue'

const props = defineProps<{
  turn: Turn
  verbose?: boolean
  clarify?: { options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null
  sessionId: string
  liveStreaming: string
  liveThinking: string
}>()

// 澄清选项提交成功 → 通知父级重开会话刷新事件流（turn 状态由 events 驱动）
const emit = defineEmits<{ (e: 'submit-clarify'): void }>()

const finalText = computed(() => {
  if (!props.turn.finalAnswer) return ''
  const msg = props.turn.finalAnswer.message || ''
  // "会话完成: <内容>" / "执行失败: <内容>"
  const m = msg.match(/^(?:会话完成|执行失败)[:：]?\s*([\s\S]*)$/)
  return m ? m[1].trim() : msg
})

const statusLabel = computed(() => {
  if (props.turn.status === 'running') return '处理中'
  if (props.turn.status === 'error') return '失败'
  if (props.turn.status === 'awaiting_clarify') return '待澄清'
  return '完成'
})

const statusColor = computed(() => {
  if (props.turn.status === 'running') return 'text-blue-400'
  if (props.turn.status === 'error') return 'text-red-400'
  if (props.turn.status === 'awaiting_clarify') return 'text-yellow-400'
  return 'text-green-400'
})

const primaryAgent = computed(() => props.turn.agents[0] || 'MetaAgent')

// 实时思考行只显示尾部 ~300 字符（对齐 TUI 滚动显示当前行的行为），避免长思考占满聊天区
const liveThinkingTail = computed(() => {
  const s = props.liveThinking.trim()
  if (!s) return ''
  return s.length > 300 ? '…' + s.slice(-300) : s
})

// 流式汇报文本 markdown 化并追加流式光标（对齐 TUI 正文 + ▍）
const liveStreamingHtml = computed(() => {
  if (!props.liveStreaming) return ''
  return renderMd(props.liveStreaming) + '<span class="live-cursor">▍</span>'
})

// 代码块头栏复制/下载（事件委托：md-article 内 v-html 按钮无 Vue 绑定）
function onMdAction(e: MouseEvent) {
  const el = e.target as HTMLElement
  if (!el.closest('.md-code-actions')) return
  const code = (el.closest('.md-code')?.querySelector('pre')?.textContent || '').replace(/\n$/, '')
  if (!code) return
  if (el.closest('.md-copy')) {
    navigator.clipboard.writeText(code).then(
      () => ElMessage.success('代码已复制'),
      () => ElMessage.error('复制失败')
    )
    return
  }
  if (el.closest('.md-download')) {
    const blob = new Blob([code], { type: 'text/plain;charset=utf-8' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = 'snippet.txt'
    a.click()
    URL.revokeObjectURL(a.href)
  }
}

// 思考链只展示最后一段连续 think（运行时替换而非累计）；
// 工具调用不再逐条渲染，交给 ToolActivity 单行就地替换 + 结束后折叠汇总。
const lastThinkEvents = computed<SessionEvent[]>(() => {
  let buf: SessionEvent[] = []
  let last: SessionEvent[] = []
  for (const step of props.turn.steps) {
    if (step.kind === 'think' && step.event) {
      buf.push(step.event)
      last = buf
    } else if (step.kind === 'tool') {
      buf = []
    }
  }
  return last
})

// ---- 待澄清选项交互 ----
// 多选已勾选项；提交中禁用按钮防止重复提交。
// 注意：awaiting_clarify 帧每秒推送、对象引用会变，因此按 questionId 复位而不是按对象引用。
const selectedOptions = ref<string[]>([])
const submitting = ref(false)

watch(() => props.clarify?.questionId, () => {
  selectedOptions.value = []
})

async function submitOption(optionId?: string) {
  if (submitting.value) return
  // 「其他」逃生选项：不提交，引导用户在下方输入框自由填写答案
  // （awaiting_clarify 状态下输入框内容会作为澄清答复发送到 /clarify）。
  if (optionId === 'other') {
    ElMessage.info('请在下方输入框输入你的答案，回车/发送提交')
    return
  }
  let answer = ''
  if (optionId) {
    answer = optionId // 单选：直接提交选项 ID
  } else {
    if (selectedOptions.value.length === 0) return // 多选：无选中项不提交
    answer = selectedOptions.value.join(',') // 多选：选项 ID 逗号分隔
  }
  submitting.value = true
  try {
    await clarifySession(props.sessionId, answer)
    emit('submit-clarify')
  } catch (e) {
    console.error('clarify submit failed:', e)
    ElMessage.error('提交答复失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="flex justify-start mb-6">
    <div class="max-w-[88%] w-full">
      <!-- 元信息行 -->
      <div class="text-xs text-ink-2 flex items-center gap-2 mb-1.5">
        <el-icon class="text-blue-400 text-sm"><UserFilled /></el-icon>
        <span :class="agentTextColor(primaryAgent)" class="font-medium">{{ primaryAgent }}</span>
        <span v-if="turn.agents.length > 1" class="text-ink-2">+{{ turn.agents.length - 1 }}</span>
        <span>·</span>
        <span>{{ fmtTime(turn.startedAt) }}</span>
        <span>·</span>
        <span :class="statusColor" class="flex items-center gap-1">
          <el-icon v-if="turn.status === 'running'" class="is-loading"><Loading /></el-icon>
          <el-icon v-else-if="turn.status === 'completed'"><CircleCheck /></el-icon>
          <el-icon v-else-if="turn.status === 'awaiting_clarify'"><QuestionFilled /></el-icon>
          <el-icon v-else><CircleClose /></el-icon>
          {{ statusLabel }}
        </span>
        <span v-if="turn.tokens.in || turn.tokens.out" class="text-ink-2">
          · tokens {{ turn.tokens.in }} ↑ / {{ turn.tokens.out }} ↓
        </span>
      </div>

      <!-- 思考链（仅最后一段，运行时替换）+ 工具活动（单行就地替换 / 结束后折叠汇总） -->
      <ThinkChain v-if="lastThinkEvents.length" :events="lastThinkEvents" :verbose="verbose" />
      <ToolActivity :groups="turn.toolCalls" :running="turn.status === 'running'" />

      <!-- 错误事件 -->
      <div v-for="(err, i) in turn.errors" :key="'err-' + i"
           class="bg-red-50 border border-red-200 rounded-lg px-3 py-2 my-2 text-xs text-red-700 dark:bg-red-900/20 dark:border-red-700/40 dark:text-red-300">
        <div class="flex items-center gap-2 font-medium mb-1">
          <el-icon><WarningFilled /></el-icon>
          <span>{{ err.agent }} 报告错误</span>
          <span class="text-ink-2 ml-auto">{{ fmtTime(err.timestamp) }}</span>
        </div>
        <div class="whitespace-pre-wrap">{{ err.message }}</div>
      </div>

      <!-- 最终回答（DeepSeek 文章排版：大字号宽行距，代码块带复制/下载头栏） -->
      <div v-if="finalText" class="md-article mt-2"
           @click="onMdAction"
           v-html="renderMd(finalText)"></div>

      <!-- 待澄清提示：Agent 请求用户回答，会话挂起 -->
      <div v-else-if="turn.status === 'awaiting_clarify' && turn.clarifyQuestion"
           class="bg-amber-50 border border-amber-200 rounded-lg px-3 py-2 my-2 text-xs text-amber-700 dark:bg-yellow-900/20 dark:border-yellow-700/40 dark:text-yellow-300">
        <div class="flex items-center gap-2 font-medium mb-1">
          <el-icon><QuestionFilled /></el-icon>
          <span>需要你的澄清</span>
          <span class="text-ink-2 ml-auto">{{ fmtTime(turn.clarifyQuestion.timestamp) }}</span>
        </div>
        <div class="whitespace-pre-wrap">{{ turn.clarifyQuestion.message }}</div>

        <!-- 澄清选项（来自 SSE awaiting_clarify 帧）：单选按钮 / 多选复选框 + 提交 -->
        <template v-if="clarify && clarify.options.length > 0">
          <div class="mt-2 flex flex-col gap-1.5">
            <template v-if="!clarify.multiSelect">
              <button v-for="opt in clarify.options" :key="opt.id" type="button"
                      :disabled="submitting"
                      class="text-left text-xs rounded-md border border-amber-300 bg-amber-50 hover:bg-amber-100 px-2.5 py-1.5 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200"
                      @click="submitOption(opt.id)">
                {{ opt.label }}
                <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70 ml-1.5">{{ opt.description }}</span>
              </button>
            </template>
            <template v-else>
              <label v-for="opt in clarify.options" :key="opt.id"
                     class="flex items-center gap-2 text-xs text-amber-700 cursor-pointer dark:text-yellow-200">
                <input type="checkbox" :value="opt.id" v-model="selectedOptions" :disabled="submitting"
                       class="accent-yellow-500" />
                <span>{{ opt.label }}</span>
                <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70">{{ opt.description }}</span>
              </label>
              <button type="button"
                      :disabled="submitting || selectedOptions.length === 0"
                      class="self-start text-xs rounded-md border border-amber-300 bg-amber-50 hover:bg-amber-100 px-2.5 py-1 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200"
                      @click="submitOption()">
                提交选择
              </button>
            </template>
          </div>
          <div class="mt-1.5 text-[11px] text-amber-600/70 dark:text-yellow-500/60">也可直接输入文字答复</div>
        </template>
        <!-- 无候选项的提问（ask_user 纯文本，选项区为空）：给出输入框答复引导，
             避免用户面对问题卡没有任何操作入口（2026-09-08 web 端 ask_user 修复）。 -->
        <template v-else-if="clarify">
          <div class="mt-1.5 text-[11px] text-amber-600/70 dark:text-yellow-500/60">该提问无候选项：直接在下方输入框输入答复并发送即可</div>
        </template>
      </div>

      <!-- 运行中：模型实时思考行 + 流式汇报文本（SSE live 帧，对齐 TUI 展示），两者皆空时兜底静态占位 -->
      <div v-else-if="turn.status === 'running'" class="mt-2">
        <div v-if="liveThinking" class="text-xs text-ink-2 flex items-start gap-1.5">
          <span>💭</span>
          <span class="italic break-all">{{ liveThinkingTail }}</span>
          <span class="live-cursor">▍</span>
        </div>
        <div v-if="liveStreaming"
             class="md-article mt-2"
             @click="onMdAction"
             v-html="liveStreamingHtml"></div>
        <div v-if="!liveThinking && !liveStreaming" class="text-xs text-ink-2 flex items-center gap-2">
          <el-icon class="is-loading"><Loading /></el-icon>
          <span>正在生成回答…</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.live-cursor {
  color: var(--bma-primary);
  margin-left: 2px;
  animation: live-blink 1s steps(2, start) infinite;
}
@keyframes live-blink { to { visibility: hidden; } }
</style>
