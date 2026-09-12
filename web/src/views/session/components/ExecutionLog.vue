<script setup lang="ts">
import { ref, computed } from 'vue'
import type { SessionEvent } from '@/types'
import MarkdownRenderer from '@/components/MarkdownRenderer.vue'
import { kindTagType, agentTextColor, fmtTime, hasDetail } from '@/views/session/chat/utils/eventStyles'

const props = defineProps<{
  events: SessionEvent[]
  /** 锁定过滤为该 Agent 名（选中 Agent 的监控态）：有值时下拉替换为徽标不可改 */
  forceAgent?: string
}>()

const filterAgent = ref('all')
const filterKind = ref('all')
const searchLog = ref('')

const agents = computed(() => ['all', ...Array.from(new Set(props.events.map(e => e.agent)))]
)
const kinds = computed(() => ['all', ...Array.from(new Set(props.events.map(e => e.kind || e.type)))]
)

const filtered = computed(() => {
  return props.events.filter(ev => {
    // forceAgent 锁定优先于本地下拉（选中 Agent 的监控只看该 Agent）
    if (props.forceAgent) {
      if (ev.agent !== props.forceAgent) return false
    } else if (filterAgent.value !== 'all' && ev.agent !== filterAgent.value) return false
    if (filterKind.value !== 'all' && (ev.kind || ev.type) !== filterKind.value) return false
    const q = searchLog.value.trim().toLowerCase()
    if (q && !ev.message.toLowerCase().includes(q)) return false
    return true
  })
})

const expanded = ref<Set<number>>(new Set())
function toggle(i: number) {
  // 重新赋值新 Set 触发 ref 响应性（F6 修复，同 F5）
  const next = new Set(expanded.value)
  if (next.has(i)) next.delete(i)
  else next.add(i)
  expanded.value = next
}

const progress = computed(() => {
  const total = props.events.length
  if (!total) return 0
  const events = props.events
  // 终态判定：会话完成 / 执行失败 / 错误 / 待澄清 → 100%
  const hasFinish = events.some(e => e.type === 'system' && e.message.startsWith('会话完成'))
  const hasError = events.some(e => e.type === 'error' || e.message.includes('执行失败'))
  const hasClarify = events.some(e => e.type === 'clarify' || e.kind === 'clarify')
  if (hasFinish || hasError || hasClarify) return 100
  // 运行中：基于步数渐进，避免失败会话永远卡在 95%
  return Math.min(95, Math.round(total / (total + 5) * 100))
})

const progressStatus = computed(() => {
  const events = props.events
  if (events.some(e => e.type === 'error' || e.message.includes('执行失败'))) return 'exception'
  if (events.some(e => e.type === 'clarify' || e.kind === 'clarify')) return 'warning'
  if (events.some(e => e.type === 'system' && e.message.startsWith('会话完成'))) return 'success'
  return undefined
})
</script>

<template>
  <div class="flex-1 flex flex-col h-full bg-card">
    <div class="p-3 border-b border-line flex items-center gap-4 text-xs shrink-0">
      <div class="flex items-center gap-2">
        <span class="text-ink-2">Agent:</span>
        <span v-if="forceAgent" class="px-2 py-1 rounded bg-primary-soft text-primary font-bold shrink-0">
          当前 Agent：{{ forceAgent }}
        </span>
        <el-select v-else v-model="filterAgent" size="small" class="w-32 !bg-transparent filter-select">
          <el-option v-for="a in agents" :key="a" :label="a === 'all' ? 'All' : a" :value="a" />
        </el-select>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-ink-2">Kind:</span>
        <el-select v-model="filterKind" size="small" class="w-32 !bg-transparent filter-select">
          <el-option v-for="k in kinds" :key="k" :label="k === 'all' ? 'All' : k" :value="k" />
        </el-select>
      </div>
      <el-input v-model="searchLog" size="small" placeholder="搜索日志..." class="w-64 ml-auto !bg-page search-input">
        <template #suffix>
          <el-icon class="text-ink-2 hover:text-ink cursor-pointer mr-2"><Search /></el-icon>
          <el-icon class="text-ink-2 hover:text-ink cursor-pointer"><Filter /></el-icon>
        </template>
      </el-input>
    </div>

    <div class="flex-1 overflow-y-auto p-4 space-y-4 min-h-0">
      <div v-for="(ev, index) in filtered" :key="index" class="flex text-xs items-start gap-4">
        <div class="text-ink-2 w-16 shrink-0 pt-0.5">{{ fmtTime(ev.timestamp) }}</div>
        <div class="w-32 shrink-0 pt-0.5" :class="agentTextColor(ev.agent)">{{ ev.agent }}</div>
        <div class="w-20 shrink-0 pt-0.5 flex justify-center">
          <el-tag size="small" :type="kindTagType(ev.kind, ev.type)" effect="plain" class="!bg-transparent !border-line scale-90">{{ ev.kind || ev.type }}</el-tag>
        </div>
        <div class="flex-1 min-w-0">
          <MarkdownRenderer :content="ev.message" class="text-ink break-words leading-relaxed pt-0.5" />

          <div v-if="hasDetail(ev)" class="mt-2">
            <div class="text-ink-2 mb-1 flex items-center gap-1 cursor-pointer hover:text-ink" @click="toggle(index)"
            >
              <el-icon><component :is="expanded.has(index) ? 'ArrowUp' : 'ArrowDown'" /></el-icon> 详情
            </div>
            <div v-if="expanded.has(index)" class="ml-2 pl-3 border-l-2 border-line space-y-2">
              <pre v-if="ev.tool_output" class="bg-page p-2 rounded text-ink-2 whitespace-pre-wrap">{{ ev.tool_output }}</pre>
              <pre v-if="ev.tool_error" class="bg-page p-2 rounded text-red-400 whitespace-pre-wrap">{{ ev.tool_error }}</pre>
              <pre v-if="ev.detail_json" class="bg-page p-2 rounded text-ink-2 whitespace-pre-wrap">{{ ev.detail_json }}</pre>
              <pre v-if="ev.prompt" class="bg-page p-2 rounded text-ink-2 whitespace-pre-wrap">{{ ev.prompt }}</pre>
            </div>
          </div>
        </div>
      </div>
    </div>

    <div class="h-12 border-t border-line flex items-center px-6 gap-4 shrink-0 bg-card">
      <span class="text-xs text-ink-2 whitespace-nowrap">整体进度</span>
      <el-progress :percentage="progress" :status="progressStatus" :show-text="false" class="flex-1 custom-progress" />
      <span class="text-xs text-ink-2 whitespace-nowrap">{{ progress }}% ({{ events.length }} 事件)</span>
    </div>
  </div>
</template>

<style scoped>
:deep(.filter-select .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid var(--bma-border);
}
:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid var(--bma-border);
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}
:deep(.custom-progress .el-progress-bar__outer) {
  background-color: var(--bma-border);
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: var(--bma-primary);
}
.markdown-body :deep(p) { margin: 0; }
.markdown-body :deep(pre) { background: var(--bma-page); padding: 8px; border-radius: 4px; margin-top: 4px; }
.markdown-body :deep(code) { font-family: monospace; }
</style>
