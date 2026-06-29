<script setup lang="ts">
import { computed } from 'vue'
import type { SessionEvent } from '@/types'
import type { Turn, ToolCallGroup } from '../utils/turns'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import ThinkChain from './ThinkChain.vue'
import ToolCallCard from './ToolCallCard.vue'

const props = defineProps<{
  turn: Turn
  verbose?: boolean
}>()

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
          <el-icon v-else><CircleClose /></el-icon>
          {{ statusLabel }}
        </span>
        <span v-if="turn.tokens.in || turn.tokens.out" class="text-gray-500">
          · tokens {{ turn.tokens.in }} ↑ / {{ turn.tokens.out }} ↓
        </span>
      </div>

      <!-- ReAct 步骤：按时间交错渲染思考链与工具调用，保留时序 -->
      <template v-for="(b, i) in blocks" :key="i">
        <ThinkChain v-if="b.type === 'think'" :events="b.events" :verbose="verbose" />
        <ToolCallCard v-else :group="b.group" />
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
