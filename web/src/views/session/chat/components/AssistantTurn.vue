<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { SessionEvent, ClarifyPending, ClarifyQuestionItem } from '@/types'
import type { Turn } from '../utils/turns'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import { clarifySession, clarifySessionBatch } from '@/api/session'
import ThinkChain from './ThinkChain.vue'
import ToolActivity from './ToolActivity.vue'

const props = defineProps<{
  turn: Turn
  verbose?: boolean
  clarify?: ClarifyPending | null
  /** 批量问答逐题草稿（任务 140，下标对齐 clarify.questions；父级持有，抗帧重推/重连） */
  clarifyDrafts?: string[]
  sessionId: string
  liveStreaming: string
  liveThinking: string
}>()

// 澄清提交成功 → 通知父级置 running + 确认条（不再全量重载，任务 140 问题④）
const emit = defineEmits<{
  (e: 'submit-clarify'): void
  (e: 'update-clarify-drafts', drafts: string[]): void
}>()

const finalText = computed(() => {
  if (props.turn.finalAnswer) {
    const msg = props.turn.finalAnswer.message || ''
    // "会话完成: <内容>" / "执行失败: <内容>"
    const m = msg.match(/^(?:会话完成|执行失败)[:：]?\s*([\s\S]*)$/)
    return m ? m[1].trim() : msg
  }
  // 被新用户消息接替的回合：答复 = 接替时刻收编的流式汇报文本
  return props.turn.finalText || ''
})

const statusLabel = computed(() => {
  if (props.turn.status === 'running') return '处理中'
  if (props.turn.status === 'error') return '失败'
  if (props.turn.status === 'cancelled') return '已终止'
  if (props.turn.status === 'awaiting_clarify') return '待澄清'
  return '完成'
})

const statusColor = computed(() => {
  if (props.turn.status === 'running') return 'text-blue-400'
  if (props.turn.status === 'error') return 'text-red-400'
  if (props.turn.status === 'cancelled') return 'text-ink-3'
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

// ---- 待澄清：问题文本与长上下文 ----
// 问题文本剥掉后端事件前缀（任务 140 起事件为 "Agent 提问: <短问题>"，长上下文拆分为
// 独立 clarify_detail 事件渲染在上方；旧会话消息为前缀+detail拼接，仅剥前缀兜底）。
const questionText = computed(() => {
  const m = props.turn.clarifyQuestion?.message || ''
  return m.replace(/^Agent 提问[:：]\s*/, '')
})

// ---- 单题模式选项交互（现状标记） ----
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

// ---- 批量模式问答卡（任务 140 问题③）：同屏分页、可回退改选、全部作答后统一提交 ----
const batchQuestions = computed<ClarifyQuestionItem[]>(() => {
  const qs = props.clarify?.questions || []
  return qs.length > 1 ? qs : []
})
const isBatch = computed(() => batchQuestions.value.length > 1)

const pagerIndex = ref(0)
// 「其他」逃生选项点击后进入自由文本模式的题下标（-1 = 未进入；仅单选页会出现）
const otherModeIndex = ref(-1)
const submittingBatch = ref(false)

const currentQuestion = computed(() => batchQuestions.value[pagerIndex.value] || null)

function draftOf(i: number): string {
  return (props.clarifyDrafts && props.clarifyDrafts[i]) || ''
}

const answeredCount = computed(() =>
  batchQuestions.value.reduce((n, _q, i) => (draftOf(i).trim() ? n + 1 : n), 0)
)
// 必须全部作答才能提交（用户确认规则）
const allAnswered = computed(
  () => batchQuestions.value.length > 0 && answeredCount.value === batchQuestions.value.length
)

function setDraft(i: number, value: string) {
  const base =
    props.clarifyDrafts && props.clarifyDrafts.length === batchQuestions.value.length
      ? [...props.clarifyDrafts]
      : batchQuestions.value.map(() => '')
  base[i] = value
  emit('update-clarify-drafts', base)
}

// 翻页：恢复该页多选勾选状态（草稿逗号分隔回解），退出自由文本模式
function goPage(i: number) {
  if (i < 0 || i >= batchQuestions.value.length) return
  pagerIndex.value = i
  otherModeIndex.value = -1
  selectedOptions.value = currentQuestion.value?.multi_select && draftOf(i) ? draftOf(i).split(',') : []
}

// 单选作答后自动跳到下一个未答题；全部已答则停在当前页
function advanceToUnanswered() {
  const n = batchQuestions.value.length
  for (let k = 1; k <= n; k++) {
    const idx = (pagerIndex.value + k) % n
    if (!draftOf(idx).trim()) {
      goPage(idx)
      return
    }
  }
}

function pickBatchOption(i: number, optId: string) {
  if (optId === 'other') {
    // 「其他」逃生：进入自由文本模式，草稿由输入内容落定（预填已输入内容便于改写）
    otherModeIndex.value = i
    return
  }
  otherModeIndex.value = -1
  setDraft(i, optId)
  advanceToUnanswered()
}

// 内置自由文本逃生口：部分提问的选项集没有 id=other 逃生项（如「我会在补充说明里
// 写具体意见」），选完选项后用户没有输入补充内容的地方。进入时若当前草稿是已选项
// 的 id 则清空（预填 "o2" 这类选项 id 对自由文本毫无意义），从零输入。
function enterOtherMode(i: number) {
  otherModeIndex.value = i
  const cur = draftOf(i)
  if (currentQuestion.value?.options?.some((o) => o.id === cur)) {
    setDraft(i, '')
  }
}

// 多选「确定」：勾选 id 逗号分隔落草稿
function confirmBatchMulti(i: number) {
  if (!selectedOptions.value.length) return
  setDraft(i, selectedOptions.value.join(','))
  otherModeIndex.value = -1
  advanceToUnanswered()
}

// 自由文本（其他逃生 / 无选项文本题）输入落草稿
function onDraftInput(i: number, e: Event) {
  setDraft(i, (e.target as HTMLTextAreaElement).value)
}

async function submitBatch() {
  if (!allAnswered.value || submittingBatch.value || !props.clarify) return
  submittingBatch.value = true
  try {
    await clarifySessionBatch(props.sessionId, batchQuestions.value.map((_, i) => draftOf(i).trim()))
    emit('submit-clarify')
  } catch (e) {
    console.error('batch clarify submit failed:', e)
    ElMessage.error('提交答复失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    submittingBatch.value = false
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
          <span>{{ isBatch ? '需要你的澄清（批量提问）' : '需要你的澄清' }}</span>
          <span class="text-ink-2 ml-auto">{{ fmtTime(turn.clarifyQuestion.timestamp) }}</span>
        </div>

        <!-- 长上下文（任务 140 问题①）：clarify_detail 事件承载，先于问题展示；限高滚动防长盘点刷屏 -->
        <div v-for="(d, i) in turn.clarifyDetails" :key="'cd-' + i"
             class="mb-2 rounded-md border border-amber-200/80 bg-white/60 dark:bg-yellow-900/20 px-2.5 py-2">
          <div class="text-[11px] font-medium text-amber-600/80 dark:text-yellow-400/70 mb-1 flex items-center gap-1">
            <el-icon><Document /></el-icon> 上下文
          </div>
          <div class="whitespace-pre-wrap max-h-44 overflow-y-auto">{{ d.message }}</div>
        </div>

        <!-- 批量模式：分页问答卡（‹ n/N › 翻页 + 已答圆点 + 全部作答后统一提交） -->
        <template v-if="isBatch && clarify">
          <div class="flex items-center gap-2 mb-1.5">
            <button type="button" :disabled="pagerIndex <= 0 || submittingBatch"
                    class="w-[22px] h-[22px] rounded-md border border-amber-300 dark:border-yellow-700/40 flex items-center justify-center hover:bg-amber-100 dark:hover:bg-yellow-900/50 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                    @click="goPage(pagerIndex - 1)">‹</button>
            <span class="font-medium">题 {{ pagerIndex + 1 }} / {{ batchQuestions.length }}</span>
            <button type="button" :disabled="pagerIndex >= batchQuestions.length - 1 || submittingBatch"
                    class="w-[22px] h-[22px] rounded-md border border-amber-300 dark:border-yellow-700/40 flex items-center justify-center hover:bg-amber-100 dark:hover:bg-yellow-900/50 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
                    @click="goPage(pagerIndex + 1)">›</button>
            <div class="flex items-center gap-1 ml-2" :title="`已答 ${answeredCount}/${batchQuestions.length} 题`">
              <span v-for="(_q, i) in batchQuestions" :key="'dot-' + i" class="w-1.5 h-1.5 rounded-full"
                    :class="draftOf(i).trim() ? 'bg-amber-500 dark:bg-yellow-400' : 'bg-amber-200 dark:bg-yellow-800'"></span>
            </div>
          </div>

          <template v-if="currentQuestion">
            <div class="whitespace-pre-wrap">{{ currentQuestion.question }}</div>

            <!-- 结构化选项：单选点击即记草稿并自动跳未答题 / 多选勾选后确定 -->
            <div v-if="currentQuestion.options && currentQuestion.options.length" class="mt-2 flex flex-col gap-1.5">
              <template v-if="!currentQuestion.multi_select">
                <button v-for="opt in currentQuestion.options" :key="opt.id" type="button"
                        :disabled="submittingBatch"
                        class="text-left text-xs rounded-md border px-2.5 py-1.5 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                        :class="draftOf(pagerIndex) === opt.id
                          ? 'border-amber-400 bg-amber-200/80 text-amber-800 dark:border-yellow-500 dark:bg-yellow-800/60 dark:text-yellow-100'
                          : 'border-amber-300 bg-amber-50 hover:bg-amber-100 text-amber-700 dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200'"
                        @click="pickBatchOption(pagerIndex, opt.id)">
                  {{ opt.label }}
                  <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70 ml-1.5">{{ opt.description }}</span>
                </button>
                <!-- 内置自由文本逃生口：选项集没有 other 项时也能补充说明 -->
                <button v-if="otherModeIndex !== pagerIndex" type="button" :disabled="submittingBatch"
                        class="self-start text-xs rounded-md border border-dashed border-amber-300 bg-amber-50/60 hover:bg-amber-100 px-2.5 py-1.5 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/20 dark:hover:bg-yellow-900/40 dark:text-yellow-200"
                        @click="enterOtherMode(pagerIndex)">
                  其他：自行输入答复
                </button>
                <!-- 「其他」逃生：自由文本输入 -->
                <div v-if="otherModeIndex === pagerIndex">
                  <textarea rows="2" :value="draftOf(pagerIndex)" :disabled="submittingBatch"
                            placeholder="请输入你的答复…"
                            class="w-full text-xs rounded-md border border-amber-300 dark:border-yellow-700/40 bg-white/80 dark:bg-yellow-900/30 px-2 py-1.5 text-amber-800 dark:text-yellow-200 outline-none focus:border-amber-400 dark:focus:border-yellow-500"
                            @input="onDraftInput(pagerIndex, $event)"></textarea>
                </div>
              </template>
              <template v-else>
                <label v-for="opt in currentQuestion.options" :key="opt.id"
                       class="flex items-center gap-2 text-xs text-amber-700 cursor-pointer dark:text-yellow-200">
                  <input type="checkbox" :value="opt.id" v-model="selectedOptions" :disabled="submittingBatch"
                         class="accent-yellow-500" />
                  <span>{{ opt.label }}</span>
                  <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70">{{ opt.description }}</span>
                </label>
                <button type="button"
                        :disabled="submittingBatch || selectedOptions.length === 0"
                        class="self-start text-xs rounded-md border border-amber-300 bg-amber-50 hover:bg-amber-100 px-2.5 py-1 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200"
                        @click="confirmBatchMulti(pagerIndex)">
                  确定本题选择
                </button>
              </template>
            </div>
            <!-- 无选项文本题：内联文本域 -->
            <div v-else class="mt-2">
              <textarea rows="3" :value="draftOf(pagerIndex)" :disabled="submittingBatch"
                        placeholder="请输入你的答复…"
                        class="w-full text-xs rounded-md border border-amber-300 dark:border-yellow-700/40 bg-white/80 dark:bg-yellow-900/30 px-2 py-1.5 text-amber-800 dark:text-yellow-200 outline-none focus:border-amber-400 dark:focus:border-yellow-500"
                        @input="onDraftInput(pagerIndex, $event)"></textarea>
            </div>
          </template>

          <!-- 统一提交：必须全部作答才可用 -->
          <div class="mt-2.5 flex items-center gap-2 flex-wrap">
            <button type="button" :disabled="!allAnswered || submittingBatch"
                    class="text-xs rounded-md px-3 py-1.5 font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    :class="allAnswered
                      ? 'bg-amber-500 hover:bg-amber-600 text-white dark:bg-yellow-500 dark:hover:bg-yellow-600'
                      : 'border border-amber-300 text-amber-600 dark:border-yellow-700/40 dark:text-yellow-500/70'"
                    @click="submitBatch">
              {{ submittingBatch ? '提交中…' : (!allAnswered ? `提交全部答案（已答 ${answeredCount}/${batchQuestions.length}）` : '提交全部答案') }}
            </button>
            <span class="text-[11px] text-amber-600/70 dark:text-yellow-500/60">全部作答后才能提交；可 ‹ › 翻页回改</span>
          </div>
        </template>

        <!-- 单题模式（现状标记）：问题 + 选项 + 提交 -->
        <template v-else>
          <div class="whitespace-pre-wrap">{{ questionText }}</div>

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
