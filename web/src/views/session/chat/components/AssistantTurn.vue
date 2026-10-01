<script setup lang="ts">
import { computed, ref, watch, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import type { AgentNode, SessionEvent, ClarifyPending, ClarifyQuestionItem } from '@/types'
import type { Turn } from '../utils/turns'
import { turnArtifacts } from '../utils/turns'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import { useFileOpener } from '@/composables/fileOpener'
import { submitClarify } from '../utils/clarifySubmit'
import ThinkChain from './ThinkChain.vue'
import ToolActivity from './ToolActivity.vue'
import PlanCard from './PlanCard.vue'
import MailBubble from './MailBubble.vue'
import ArtifactCard from './ArtifactCard.vue'
import SubAgentList from './SubAgentList.vue'

const props = defineProps<{
  turn: Turn
  verbose?: boolean
  clarify?: ClarifyPending | null
  /** 批量问答逐题草稿（任务 140，下标对齐 clarify.questions；父级持有，抗帧重推/重连） */
  clarifyDrafts?: string[]
  sessionId: string
  liveStreaming: string
  liveThinking: string
  /** 会话内全部 Agent 实例：子 Agent 列表取实时状态（每 3s 轮询刷新） */
  agents?: AgentNode[]
  /** 本视图"我"的实例 ID（邮件气泡判方向用）：主对话栏 = 会话主 Agent（传 sessionId），
   *  子 Agent 面板 = 该实例 ID；不传则邮件一律按"来自"渲染 */
  mailSelfId?: string
}>()

/** 邮件气泡的实例 ID → 展示名：会话主 Agent（inst_id == sessionId）显示为「主 Agent」，
 *  子 Agent 查领域名，user/dispatcher 是系统侧来源，其余回退原值。 */
function agentNameOf(id: string): string {
  if (!id) return '(未知)'
  if (id === props.sessionId || id === 'meta') return '主 Agent'
  if (id === 'user') return '用户'
  if (id === 'dispatcher') return '调度器'
  return props.agents?.find((a) => a.inst_id === id)?.name || id
}

// 澄清提交成功 → 通知父级置 running + 确认条（不再全量重载，任务 140 问题④）
const emit = defineEmits<{
  (e: 'submit-clarify'): void
  (e: 'update-clarify-drafts', drafts: string[]): void
}>()

/** 本轮产出的可视成果（效果图/视频/HTML 原型）：对话栏直接渲染成媒体卡片。 */
const artifacts = computed(() => turnArtifacts(props.turn))

/** 澄清问答卡附带的产物（演示视频等，SSE awaiting_clarify 帧 artifacts 字段）。 */
const clarifyArtifacts = computed(() => props.clarify?.artifacts ?? [])

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
  if (props.turn.status === 'awaiting_child') return '挂起等子'
  return '完成'
})

const statusColor = computed(() => {
  if (props.turn.status === 'running') return 'text-blue-400'
  if (props.turn.status === 'error') return 'text-red-400'
  if (props.turn.status === 'cancelled') return 'text-ink-3'
  if (props.turn.status === 'awaiting_clarify') return 'text-yellow-400'
  if (props.turn.status === 'awaiting_child') return 'text-sky-400'
  return 'text-green-400'
})

const primaryAgent = computed(() => props.turn.agents[0] || 'MetaAgent')

/** 待澄清问答卡要渲染的提问事件：已给出最终答复、或回合没有提问时为 null。
 *  返回对象而非布尔值——模板内 `v-if="clarifyCard"` 才能把事件类型收窄。 */
const clarifyCard = computed(() => {
  if (finalText.value) return null
  if (props.turn.status !== 'awaiting_clarify') return null
  return props.turn.clarifyQuestion || null
})
/** 运行中占位是否渲染（与问答卡/最终答复互斥）。 */
const showRunning = computed(
  () => !finalText.value && !clarifyCard.value && props.turn.status === 'running',
)

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

// 本地文件链接（TODO #26 A）：data-bma-file = 工作区文件入口，点击在右侧文件树打开。
const fileOpener = useFileOpener()

// 代码块头栏复制/下载 + 本地文件链接（事件委托：md-article 内 v-html 无 Vue 绑定）
function onMdAction(e: MouseEvent) {
  const el = e.target as HTMLElement
  const fileLink = el.closest('a[data-bma-file]')
  if (fileLink) {
    e.preventDefault()
    const p = fileLink.getAttribute('data-bma-file') || ''
    if (fileOpener) fileOpener.openInTree(p)
    else ElMessage.info('文件：' + p)
    return
  }
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

// 思考链累计展示（2026-09-13 用户诉求）：此前只保留"最后一段连续 think"，一到工具调用
// 前一段推理就整段消失。现在全部推理段落都留着；只在一段内部去重——思考文本是累积快照
// （LLMDelta 与 ToolCall 各落一次），后一条是前一条的超集时前一条不再渲染，避免同段重复。
// 工具调用本身仍不逐条渲染，交给 ToolActivity 单行就地替换 + 结束后折叠汇总。
const thinkEvents = computed<SessionEvent[]>(() => {
  const out: SessionEvent[] = []
  let run: SessionEvent[] = []
  const flush = () => {
    for (let i = 0; i < run.length; i++) {
      const next = run[i + 1]?.message || ''
      if (next && next.startsWith(run[i].message || '')) continue // 被后一条累积快照取代
      out.push(run[i])
    }
    run = []
  }
  for (const step of props.turn.steps) {
    if (step.kind === 'think' && step.event) run.push(step.event)
    else if (step.kind === 'tool' || step.kind === 'narrate') flush()
  }
  flush()
  return out
})

// 中间正文块：模型"口播一句 → 调工具"的那段话（后端在工具调用边界落 assistant_text 事件）。
// 提问前的那段已由问答卡上方的 clarifyReport 呈现（后端两处存的是同一段文本），这里去重，
// 免同一段话在对话栏出现两次（审批走 approval hook 快照，后端跳不掉的那部分靠这条兜住）。
const narrations = computed<SessionEvent[]>(() => {
  const report = (props.turn.clarifyReport || '').trim()
  if (!report) return props.turn.narrations
  return props.turn.narrations.filter((n) => (n.message || '').trim() !== report)
})

// ---- 待澄清：问题文本与长上下文 ----
// 问题文本剥掉后端事件前缀（任务 140 起事件为 "Agent 提问: <短问题>"，长上下文拆分为
// 独立 clarify_detail 事件渲染在上方；旧会话消息为前缀+detail拼接，仅剥前缀兜底）。
const questionText = computed(() => {
  const m = props.turn.clarifyQuestion?.message || ''
  return m.replace(/^Agent 提问[:：]\s*/, '')
})

// ---- 单题模式选项交互（2026-09-20 重做）----
// 点选项=选中态（不再即提交，防误触即答无法反悔）；「提交答复」把选中项的中文 label
//（而非 id——回显/事件流里用户不应看到自己答了 "pg"/"revise" 这类字符串）与卡内补充
// 说明组合成答复文本。补充说明框常驻卡内：选选项可补充两句，不选则直接作为文字答复，
// 视线不再需要在卡片与页面底部输入框之间来回跳。
const selectedOptionId = ref('')
const singleDraft = ref('')
const singleDraftRef = ref<HTMLTextAreaElement | null>(null)
const selectedOptions = ref<string[]>([]) // 多选勾选集（内部仍按 id，提交时 label 化）
const submitting = ref(false)

// 计划审批（submit_plan 顶层走 ask_user 澄清通道，问题带【计划确认】前缀）：专属卡面 +
// 批准二次确认 + 修改意见内联输入。修 bug：旧实现点"需要修改"直接提交选项 id "revise"，
// 后端 parsePlanAnswer 关键词不命中，计划被驳回且修改意见 = 英文单词 "revise"。
const isPlanApproval = computed(() => questionText.value.startsWith('【计划确认】'))
const approveArmed = ref(false) // 批准二次确认：首点进入武装态，再点才真正提交
let approveArmTimer: ReturnType<typeof setTimeout> | undefined
const reviseOpen = ref(false)
const reviseDraft = ref('')

watch(() => props.clarify?.questionId, () => {
  selectedOptionId.value = ''
  singleDraft.value = ''
  selectedOptions.value = []
  approveArmed.value = false
  reviseOpen.value = false
  reviseDraft.value = ''
})

/** id → label：选项 id 只作内部状态，答复文本提交中文 label。 */
function labelOf(optionId: string): string {
  return props.clarify?.options.find((o) => o.id === optionId)?.label || optionId
}

function selectSingle(id: string) {
  if (submitting.value) return
  // 「其他」逃生项：不选中，引导卡内补充说明输入
  if (id === 'other') {
    singleDraftRef.value?.focus()
    return
  }
  selectedOptionId.value = selectedOptionId.value === id ? '' : id
}

/** 单题提交：选中选项 + 可选补充组合（"label：补充"）；未选选项时提交纯文本。 */
async function submitSingle() {
  if (submitting.value) return
  const opt = selectedOptionId.value
  const draft = singleDraft.value.trim()
  const answer = opt ? (draft ? `${labelOf(opt)}：${draft}` : labelOf(opt)) : draft
  if (!answer) return
  submitting.value = true
  try {
    await runClarifySubmit({ answer })
  } finally {
    submitting.value = false
  }
}

/** 多选提交：勾选项中文 label 全角逗号拼接。 */
async function submitMulti() {
  if (submitting.value || selectedOptions.value.length === 0) return
  submitting.value = true
  try {
    await runClarifySubmit({ answer: selectedOptions.value.map(labelOf).join('，') })
  } finally {
    submitting.value = false
  }
}

/** 多选全选/清空（不含「其他」逃生项）。 */
function toggleSelectAll() {
  const all = (props.clarify?.options || []).filter((o) => o.id !== 'other').map((o) => o.id)
  selectedOptions.value = selectedOptions.value.length === all.length ? [] : all
}

// ---- 计划审批操作 ----
function armApprove() {
  if (submitting.value) return
  if (approveArmed.value) {
    void submitApprove()
    return
  }
  approveArmed.value = true
  clearTimeout(approveArmTimer)
  approveArmTimer = setTimeout(() => {
    approveArmed.value = false
  }, 4000)
}
async function submitApprove() {
  clearTimeout(approveArmTimer)
  approveArmed.value = false
  if (submitting.value) return
  submitting.value = true
  try {
    // "批准开工"命中后端 parsePlanAnswer 批准关键词（plan_confirm.go）
    await runClarifySubmit({ answer: '批准开工' })
  } finally {
    submitting.value = false
  }
}
async function submitRevise() {
  const fb = reviseDraft.value.trim()
  if (!fb || submitting.value) return
  submitting.value = true
  try {
    // "需要修改：xxx"命中驳回关键词，意见随答复直达提交计划的 Agent
    await runClarifySubmit({ answer: `需要修改：${fb}` })
  } finally {
    submitting.value = false
  }
}

// ---- 答复倒计时（2026-09-20）：timeout_sec 由服务端 awaiting_clarify 帧每拍现算
//（ask_user 的 timeout_sec 截止；帧在待澄清期间每 150ms 重推），ref 刷新天然驱动
// 倒计时跳动，无需本地 interval；归零后的状态翻转由 SSE session_status 驱动。
const countdownText = computed(() => {
  const sec = props.clarify?.timeoutSec
  if (typeof sec !== 'number' || sec <= 0) return ''
  const m = Math.floor(sec / 60)
  const s = sec % 60
  return `${m}:${String(s).padStart(2, '0')}`
})

// ---- 提交韧性：后端重启/连接中断时答复不能一发就丢（2026-09-12 实证） ----
// 此前网络层失败只弹一句 "Failed to fetch"，用户不知道答案有没有送出去；现在网络层
// 失败自动重试（见 utils/clarifySubmit），重试进度与最终失败原因就地显示在问答卡里，
// 草稿原样保留——后端恢复后直接再点一次提交即可，不用重答三题。
const submitNotice = ref('') // 重试进度（"正在重连…"）
const submitError = ref('') // 最终失败原因（常驻，不随 toast 消失）
const disposed = ref(false)
onUnmounted(() => {
  disposed.value = true
  clearTimeout(approveArmTimer)
})

/** 统一提交入口：成功 → 通知父级置 running + 确认条；失败 → 卡片内保留错误与草稿。 */
async function runClarifySubmit(payload: { answer?: string; answers?: string[] }) {
  submitError.value = ''
  submitNotice.value = ''
  try {
    const r = await submitClarify(props.sessionId, payload, {
      onRetry: (n, total) => {
        submitNotice.value = `后端连接中断，正在重试（${n}/${total}）…你的答复已保留`
      },
      aborted: () => disposed.value,
    })
    submitNotice.value = ''
    if (r === 'confirmed') {
      ElMessage.success('答复已提交（响应中断，已按服务端状态确认）')
    }
    emit('submit-clarify')
  } catch (e) {
    submitNotice.value = ''
    submitError.value = e instanceof Error ? e.message : String(e)
    console.error('clarify submit failed:', e)
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

/** 批量单题草稿 → 答复文本（2026-09-20）：草稿是机器态（选项 id 或自由文本），提交时
 *  把纯 id / 逗号拼接的 id 组合翻译成中文 label（与单题同策略），自由文本草稿原样提交。 */
function draftToAnswer(i: number): string {
  const draft = draftOf(i).trim()
  if (!draft) return ''
  const opts = batchQuestions.value[i]?.options || []
  const byId = new Map(opts.map((o) => [o.id, o.label]))
  const parts = draft.split(',')
  if (parts.length && parts.every((p) => byId.has(p))) {
    return parts.map((p) => byId.get(p)!).join('，')
  }
  return draft
}

async function submitBatch() {
  if (!allAnswered.value || submittingBatch.value || !props.clarify) return
  submittingBatch.value = true
  try {
    await runClarifySubmit({ answers: batchQuestions.value.map((_, i) => draftToAnswer(i)) })
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
          <el-icon v-else-if="turn.status === 'awaiting_child'"><VideoPause /></el-icon>
          <el-icon v-else><CircleClose /></el-icon>
          {{ statusLabel }}
        </span>
        <span v-if="turn.tokens.in || turn.tokens.out" class="text-ink-2">
          · tokens {{ turn.tokens.in }} ↑ / {{ turn.tokens.out }} ↓
        </span>
      </div>

      <!-- 思考链（全部推理段落，可折叠）+ 工具活动（单行就地替换 / 结束后折叠汇总） -->
      <ThinkChain v-if="thinkEvents.length" :events="thinkEvents" :verbose="verbose" />

      <!-- Agent 发言块（工具调用之间的中间正文 / 子 Agent 结果摘要 / Meta 中继文本）：
           这是模型**说给用户的话**，按正文展示，与最终答复同档——市面做法（DeepSeek 把
           推理折进「深度思考」、Claude Code 的中间消息照常显示）都是"只有推理算思考"。
           此前把这段当成了思考：先压成灰色小字、再整组装进「过程文本」定高窗，等于把模型
           的发言当噪声藏起来（2026-09-13 用户实证）。 -->
      <div v-for="(n, i) in narrations" :key="'narr-' + i" class="mt-2">
        <div v-if="n.agent && n.agent !== primaryAgent" class="text-[11px] text-ink-3 mb-0.5">{{ n.agent }}</div>
        <div class="md-article" @click="onMdAction" v-html="renderMd(n.message)"></div>
      </div>

      <!-- 计划确认（submit_plan / review_plan）：单列成卡，不随工具活动折叠——提交计划是
           里程碑（阻塞等审批、决定后续要不要动手），折进「已执行 N 次工具」等于看不见 -->
      <PlanCard v-for="p in turn.plans" :key="'plan-' + p.id" :plan="p" />

      <!-- Agent 间邮件（mailbox 留痕，按时间归属本回合）：外发此前完全不可见——
           外发只是一次被折叠的工具调用，「我怎么回上级的」在界面上无从查起（2026-09-17 实证） -->
      <MailBubble v-for="(m, i) in turn.mails" :key="'mail-' + i + '-' + m.at"
                  :mail="m" :self-id="mailSelfId" :name-of="agentNameOf" />

      <ToolActivity :groups="turn.toolCalls" :running="turn.status === 'running'" :session-id="sessionId" />

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

      <!-- 本轮产出：可视成果直接渲染（效果图/视频/音频/HTML 预览） -->
      <div v-if="artifacts.length" class="mt-2 space-y-2">
        <div class="text-[11px] text-ink-3">本轮产出（{{ artifacts.length }}）</div>
        <ArtifactCard v-for="(a, i) in artifacts" :key="'art-' + i + '-' + a.path"
                      :artifact="a" :session-id="sessionId" />
      </div>

      <!-- 提问前的正文：模型「先输出正文、再调 ask_user」时，正文只活在流式缓冲里
           （不落事件），待澄清态一切换就被整段吞掉，只剩思考链。后端已把它快照进
           提问事件（detail_json.report_text），这里按正文原样渲染在问答卡上方。
           同为模型发言（与某段中间正文是同一段文本，本组件还按内容去重），按正文展示。 -->
      <div v-if="!finalText && turn.clarifyReport" class="md-article mt-2"
           @click="onMdAction"
           v-html="renderMd(turn.clarifyReport)"></div>

      <!-- 最终回答（DeepSeek 文章排版：大字号宽行距，代码块带复制/下载头栏） -->
      <div v-if="finalText" class="md-article mt-2"
           @click="onMdAction"
           v-html="renderMd(finalText)"></div>

      <!-- 待澄清提示：Agent 请求用户回答，会话挂起 -->
      <div v-if="clarifyCard"
           class="bg-amber-50 border border-amber-200 rounded-lg px-3 py-2 my-2 text-xs text-amber-700 dark:bg-yellow-900/20 dark:border-yellow-700/40 dark:text-yellow-300">
        <div class="flex items-center gap-2 font-medium mb-1">
          <el-icon><QuestionFilled /></el-icon>
          <span>{{ isPlanApproval ? '📋 计划待批准' : (isBatch ? '需要你的澄清（批量提问）' : '需要你的澄清') }}</span>
          <span class="text-ink-2 ml-auto">{{ fmtTime(clarifyCard.timestamp) }}</span>
        </div>

        <!-- 答复倒计时（2026-09-20，ask_user timeout_sec）：服务端每帧现算剩余秒，
             到点工具侧兜底"用户未答复，自行决策"，会话自动恢复运行。 -->
        <div v-if="countdownText"
             class="mb-2 flex items-center gap-1.5 rounded-md border border-amber-200/80 bg-white/60 dark:bg-yellow-900/20 px-2.5 py-1 text-[11px] text-amber-700 dark:text-yellow-300">
          <el-icon class="is-loading"><Timer /></el-icon>
          <span>剩余 {{ countdownText }}，超时将自行决策</span>
        </div>

        <!-- 提交韧性（2026-09-12）：重试进度 / 最终失败原因就地常驻显示——
             toast 一闪而过的 "Failed to fetch" 看不出答复到底有没有送出去。 -->
        <div v-if="submitNotice"
             class="mb-2 rounded-md border border-sky-200 bg-sky-50/80 px-2.5 py-1.5 text-[11px] text-sky-700 flex items-center gap-1.5 dark:border-sky-700/40 dark:bg-sky-900/20 dark:text-sky-300">
          <el-icon class="is-loading"><Loading /></el-icon>{{ submitNotice }}
        </div>
        <div v-if="submitError"
             class="mb-2 rounded-md border border-red-200 bg-red-50/80 px-2.5 py-1.5 text-[11px] text-red-700 dark:border-red-700/40 dark:bg-red-900/20 dark:text-red-300">
          <div class="flex items-center gap-1.5 font-medium">
            <el-icon><WarningFilled /></el-icon>提交答复失败
          </div>
          <div class="mt-0.5 break-all">{{ submitError }}</div>
          <div class="mt-0.5 text-red-500/80 dark:text-red-400/70">
            你的答复仍保留在卡片里：后端恢复后直接再点一次提交即可。
          </div>
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

        <!-- 单题模式：问题 + 作答区（计划审批走专属卡面） -->
        <template v-else>
          <div class="whitespace-pre-wrap">{{ questionText }}</div>

          <!-- 澄清附带的产物（演示视频等）：内嵌在选项区上方，便于看完演示再作答；
               批量模式不展示（artifacts 只挂在顶层 clarify，镜像第一题）。 -->
          <div v-if="clarifyArtifacts.length" class="mt-2 mb-2 space-y-2">
            <ArtifactCard v-for="(a, i) in clarifyArtifacts" :key="'clarify-art-' + i + '-' + a.path"
                          :artifact="a" :session-id="sessionId" />
          </div>

          <!-- 计划审批专属卡面（2026-09-20）：批准二次确认防误触开工；驳回必须给出
               具体修改意见（内联输入，修掉旧版提交 "revise" 字面量当意见的 bug）。
               计划正文由上方 clarifyDetails 完整展示。 -->
          <template v-if="isPlanApproval && clarify">
            <div class="mt-2 flex flex-col gap-2">
              <div class="flex items-center gap-2 flex-wrap">
                <button type="button" :disabled="submitting"
                        class="text-xs rounded-md px-3 py-1.5 font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                        :class="approveArmed
                          ? 'bg-green-600 hover:bg-green-700 text-white'
                          : 'bg-amber-500 hover:bg-amber-600 text-white dark:bg-yellow-500 dark:hover:bg-yellow-600'"
                        @click="armApprove">
                  {{ submitting ? '提交中…' : (approveArmed ? '确认批准？再点一次' : '✅ 批准开工') }}
                </button>
                <button type="button" :disabled="submitting"
                        class="text-xs rounded-md border px-3 py-1.5 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                        :class="reviseOpen
                          ? 'border-amber-500 text-amber-800 bg-amber-100 dark:border-yellow-500 dark:text-yellow-100 dark:bg-yellow-900/50'
                          : 'border-amber-300 text-amber-700 hover:bg-amber-100 dark:border-yellow-700/40 dark:text-yellow-200 dark:hover:bg-yellow-900/50'"
                        @click="reviseOpen = !reviseOpen">
                  ✏️ 需要修改
                </button>
                <span class="text-[11px] text-amber-600/70 dark:text-yellow-500/60">批准后立即开工；驳回请给出具体修改意见</span>
              </div>
              <div v-if="reviseOpen">
                <textarea v-model="reviseDraft" rows="3" :disabled="submitting"
                          placeholder="请给出具体修改意见（必填），提交后 Agent 将按意见修订计划重提…"
                          class="w-full text-xs rounded-md border border-amber-300 dark:border-yellow-700/40 bg-white/80 dark:bg-yellow-900/30 px-2 py-1.5 text-amber-800 dark:text-yellow-200 outline-none focus:border-amber-400 dark:focus:border-yellow-500"></textarea>
                <button type="button"
                        :disabled="submitting || !reviseDraft.trim()"
                        class="mt-1.5 text-xs rounded-md px-3 py-1.5 font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                        :class="'bg-amber-500 hover:bg-amber-600 text-white dark:bg-yellow-500 dark:hover:bg-yellow-600'"
                        @click="submitRevise">
                  {{ submitting ? '提交中…' : '提交修改意见' }}
                </button>
              </div>
            </div>
          </template>

          <!-- 通用单题问答（2026-09-20 重做）：点选项=选中态 + 卡内补充说明，
               「提交答复」统一提交；无候选项的纯文本提问也有卡内输入框——
               答复入口就在问题旁，不再依赖页面底部输入框。 -->
          <template v-else-if="clarify">
            <div v-if="clarify.options.length > 0 && !clarify.multiSelect"
                 class="mt-2 flex flex-col gap-1.5">
              <button v-for="opt in clarify.options" :key="opt.id" type="button"
                      :disabled="submitting"
                      class="text-left text-xs rounded-md border px-2.5 py-1.5 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                      :class="selectedOptionId === opt.id
                        ? 'border-amber-400 bg-amber-200/80 text-amber-800 dark:border-yellow-500 dark:bg-yellow-800/60 dark:text-yellow-100'
                        : 'border-amber-300 bg-amber-50 hover:bg-amber-100 text-amber-700 dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200'"
                      @click="selectSingle(opt.id)">
                {{ opt.label }}
                <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70 ml-1.5">{{ opt.description }}</span>
              </button>
            </div>
            <div v-else-if="clarify.options.length > 0" class="mt-2 flex flex-col gap-1.5">
              <label v-for="opt in clarify.options" :key="opt.id"
                     class="flex items-center gap-2 text-xs text-amber-700 cursor-pointer dark:text-yellow-200">
                <input type="checkbox" :value="opt.id" v-model="selectedOptions" :disabled="submitting"
                       class="accent-yellow-500" />
                <span>{{ opt.label }}</span>
                <span v-if="opt.description" class="text-amber-600/80 dark:text-yellow-400/70">{{ opt.description }}</span>
              </label>
              <div class="flex items-center gap-2">
                <button type="button" :disabled="submitting"
                        class="self-start text-xs rounded-md border border-amber-300 bg-amber-50 hover:bg-amber-100 px-2.5 py-1 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200"
                        @click="toggleSelectAll">
                  {{ selectedOptions.length === (clarify.options.filter(o => o.id !== 'other')).length && selectedOptions.length > 0 ? '清空' : '全选' }}
                </button>
                <button type="button"
                        :disabled="submitting || selectedOptions.length === 0"
                        class="self-start text-xs rounded-md border border-amber-300 bg-amber-50 hover:bg-amber-100 px-2.5 py-1 text-amber-700 transition-colors disabled:opacity-50 disabled:cursor-not-allowed dark:border-yellow-700/40 dark:bg-yellow-900/30 dark:hover:bg-yellow-900/50 dark:text-yellow-200"
                        @click="submitMulti">
                  提交选择
                </button>
              </div>
            </div>
            <div class="mt-2">
              <textarea ref="singleDraftRef" v-model="singleDraft" rows="2" :disabled="submitting"
                        :placeholder="clarify.options.length ? '补充说明（可选）：选中选项后可补充两句；点「其他」在此填写自定义答复' : '请输入你的答复…'"
                        class="w-full text-xs rounded-md border border-amber-300 dark:border-yellow-700/40 bg-white/80 dark:bg-yellow-900/30 px-2 py-1.5 text-amber-800 dark:text-yellow-200 outline-none focus:border-amber-400 dark:focus:border-yellow-500"></textarea>
              <button type="button"
                      :disabled="submitting || (!selectedOptionId && !singleDraft.trim())"
                      class="mt-1.5 text-xs rounded-md px-3 py-1.5 font-medium transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                      :class="selectedOptionId || singleDraft.trim()
                        ? 'bg-amber-500 hover:bg-amber-600 text-white dark:bg-yellow-500 dark:hover:bg-yellow-600'
                        : 'border border-amber-300 text-amber-600 dark:border-yellow-700/40 dark:text-yellow-500/70'"
                      @click="submitSingle">
                {{ submitting ? '提交中…' : '提交答复' }}
              </button>
            </div>
          </template>
        </template>
      </div>

      <!-- 运行中：模型实时思考行 + 流式汇报文本（SSE live 帧，对齐 TUI 展示），两者皆空时兜底静态占位 -->
      <div v-if="showRunning" class="mt-2">
        <div v-if="liveThinking" class="text-xs text-ink-2 flex items-start gap-1.5">
          <span>💭</span>
          <span class="italic break-all">{{ liveThinkingTail }}</span>
          <span class="live-cursor">▍</span>
        </div>
        <!-- 流式口播：模型正在说的那句话，按正文展示（与落盘后的发言块同字号，
             中途不会跳字号） -->
        <div v-if="liveStreaming"
             class="md-article mt-2"
             @click="onMdAction"
             v-html="liveStreamingHtml"></div>
        <div v-if="!liveThinking && !liveStreaming" class="text-xs text-ink-2 flex items-center gap-2">
          <el-icon class="is-loading"><Loading /></el-icon>
          <span>正在生成回答…</span>
        </div>
      </div>

      <!-- 子 Agent 列表放回合最下方（用户定调 2026-09-17）：它是本轮的"执行单元状态"，
           压在最前面会把回复正文挤到下面——先看答复、再看谁在跑 -->
      <SubAgentList :items="turn.subAgents" :agents="agents || []" :running="turn.status === 'running'" />
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
