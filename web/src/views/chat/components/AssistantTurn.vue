<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { SessionEvent, ClarifyOption } from '@/types'
import type { Turn, ToolCallGroup } from '../utils/turns'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import { clarifySession } from '@/api/session'
import ThinkChain from './ThinkChain.vue'
import ToolCallCard from './ToolCallCard.vue'

const props = defineProps<{
  turn: Turn
  verbose?: boolean
  clarify?: { options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null
  sessionId: string
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

// 把按时间交错的 steps 切成渲染块：连续的 think 合并成一段 ThinkChain，
// 每个 tool 单独一张 ToolCallCard。这样保留 ReAct 时序
// （思考 → 工具调用 → 结果 → 下一轮思考），而非原先工具与思考分离两块。
interface ThinkBlock { type: 'think'; events: SessionEvent[] }
interface ToolBlock { type: 'tool'; group: ToolCallGroup }
type RenderBlock = ThinkBlock | ToolBlock

const blocks = computed<RenderBlock[]>(() => {
  const out: RenderBlock[] = []
  let buf: SessionEvent[] = []
  const flush = () => {
    if (buf.length) {
      out.push({ type: 'think', events: buf })
      buf = []
    }
  }
  for (const step of props.turn.steps) {
    if (step.kind === 'think' && step.event) {
      buf.push(step.event)
    } else if (step.kind === 'tool' && step.group) {
      flush()
      out.push({ type: 'tool', group: step.group })
    }
  }
  flush()
  return out
})

// 最后一条 think block 的索引：运行时只展示最新思考，替换而非累计
const lastThinkIndex = computed(() => {
  const bs = blocks.value
  for (let i = bs.length - 1; i >= 0; i--) {
    if (bs[i].type === 'think') return i
  }
  return -1
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
      <div class="text-xs text-gray-500 flex items-center gap-2 mb-1.5">
        <el-icon class="text-blue-400 text-sm"><UserFilled /></el-icon>
        <span :class="agentTextColor(primaryAgent)" class="font-medium">{{ primaryAgent }}</span>
        <span v-if="turn.agents.length > 1" class="text-gray-500">+{{ turn.agents.length - 1 }}</span>
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
        <span v-if="turn.tokens.in || turn.tokens.out" class="text-gray-500">
          · tokens {{ turn.tokens.in }} ↑ / {{ turn.tokens.out }} ↓
        </span>
      </div>

      <!-- ReAct 步骤：按时间交错渲染思考链与工具调用，保留时序。
           思考步骤仅展示最新一条（运行时替换），工具调用全部保留。 -->
      <template v-for="(b, i) in blocks" :key="i">
        <ThinkChain v-if="b.type === 'think' && i === lastThinkIndex" :events="b.events" :verbose="verbose" />
        <ToolCallCard v-else-if="b.type === 'tool'" :group="b.group" />
      </template>

      <!-- 错误事件 -->
      <div v-for="(err, i) in turn.errors" :key="'err-' + i"
           class="bg-red-900/20 border border-red-700/40 rounded-lg px-3 py-2 my-2 text-xs text-red-300">
        <div class="flex items-center gap-2 font-medium mb-1">
          <el-icon><WarningFilled /></el-icon>
          <span>{{ err.agent }} 报告错误</span>
          <span class="text-gray-500 ml-auto">{{ fmtTime(err.timestamp) }}</span>
        </div>
        <div class="whitespace-pre-wrap">{{ err.message }}</div>
      </div>

      <!-- 最终回答 -->
      <div v-if="finalText" class="bg-[#1a1d24] border border-[#2a2d35] rounded-lg px-4 py-3 mt-2 text-sm text-gray-200 leading-relaxed markdown-body"
           v-html="renderMd(finalText)"></div>

      <!-- 待澄清提示：Agent 请求用户回答，会话挂起 -->
      <div v-else-if="turn.status === 'awaiting_clarify' && turn.clarifyQuestion"
           class="bg-yellow-900/20 border border-yellow-700/40 rounded-lg px-3 py-2 my-2 text-xs text-yellow-300">
        <div class="flex items-center gap-2 font-medium mb-1">
          <el-icon><QuestionFilled /></el-icon>
          <span>需要你的澄清</span>
          <span class="text-gray-500 ml-auto">{{ fmtTime(turn.clarifyQuestion.timestamp) }}</span>
        </div>
        <div class="whitespace-pre-wrap">{{ turn.clarifyQuestion.message }}</div>

        <!-- 澄清选项（来自 SSE awaiting_clarify 帧）：单选按钮 / 多选复选框 + 提交 -->
        <template v-if="clarify && clarify.options.length > 0">
          <div class="mt-2 flex flex-col gap-1.5">
            <template v-if="!clarify.multiSelect">
              <button v-for="opt in clarify.options" :key="opt.id" type="button"
                      :disabled="submitting"
                      class="text-left text-xs rounded-md border border-yellow-700/40 bg-yellow-900/30 hover:bg-yellow-900/50 px-2.5 py-1.5 text-yellow-200 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                      @click="submitOption(opt.id)">
                {{ opt.label }}
                <span v-if="opt.description" class="text-yellow-400/70 ml-1.5">{{ opt.description }}</span>
              </button>
            </template>
            <template v-else>
              <label v-for="opt in clarify.options" :key="opt.id"
                     class="flex items-center gap-2 text-xs text-yellow-200 cursor-pointer">
                <input type="checkbox" :value="opt.id" v-model="selectedOptions" :disabled="submitting"
                       class="accent-yellow-500" />
                <span>{{ opt.label }}</span>
                <span v-if="opt.description" class="text-yellow-400/70">{{ opt.description }}</span>
              </label>
              <button type="button"
                      :disabled="submitting || selectedOptions.length === 0"
                      class="self-start text-xs rounded-md border border-yellow-700/40 bg-yellow-900/30 hover:bg-yellow-900/50 px-2.5 py-1 text-yellow-200 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                      @click="submitOption()">
                提交选择
              </button>
            </template>
          </div>
          <div class="mt-1.5 text-[11px] text-yellow-500/60">也可直接输入文字答复</div>
        </template>
      </div>

      <!-- 运行中提示 -->
      <div v-else-if="turn.status === 'running'" class="text-xs text-gray-500 mt-2 flex items-center gap-2">
        <el-icon class="is-loading"><Loading /></el-icon>
        <span>正在生成回答…</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.markdown-body :deep(p) { margin: 0 0 0.5em 0; }
.markdown-body :deep(p:last-child) { margin-bottom: 0; }
.markdown-body :deep(pre) {
  background: #0f1115;
  padding: 10px;
  border-radius: 6px;
  margin: 6px 0;
  overflow-x: auto;
  color: #e5e7eb;
  font-size: 12px;
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(a) { color: #93c5fd; }
.markdown-body :deep(ul), .markdown-body :deep(ol) { margin: 0.4em 0 0.4em 1.2em; }
.markdown-body :deep(h1), .markdown-body :deep(h2), .markdown-body :deep(h3) {
  color: #e5e7eb;
  margin: 0.6em 0 0.3em;
  font-weight: 600;
}
</style>
