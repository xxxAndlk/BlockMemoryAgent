<script setup lang="ts">
import { ref, computed } from 'vue'
import type { ToolCallGroup } from '../utils/turns'
import { fmtTime } from '../utils/eventStyles'

const props = defineProps<{ group: ToolCallGroup }>()

const expanded = ref(false)

const statusLabel = computed(() => {
  if (props.group.pending) return '执行中…'
  return props.group.success ? '执行成功' : '执行失败'
})

const statusColor = computed(() => {
  if (props.group.pending) return 'text-blue-400'
  return props.group.success ? 'text-green-400' : 'text-red-400'
})

const callArgs = computed(() => props.group.call?.detail_json || props.group.call?.tool_args || '')
const resultText = computed(() => props.group.result?.tool_output || props.group.result?.message || '')
// 后端 truncate 在 500 字处截断 (session.go persistHistory)，命中阈值时提示用户
const resultTruncated = computed(() => (props.group.result?.tool_output || '').length >= 500)
const errorText = computed(() => props.group.result?.tool_error || '')
const path = computed(() => props.group.result?.tool_path || '')
const startedAt = computed(() => props.group.call?.timestamp || props.group.result?.timestamp || '')

// 从调用参数 JSON 中提取一行关键入参摘要，折叠态直接展示，
// 让用户不展开也能看出"读了哪个文件 / 跑了什么命令 / 请求哪个 URL"。
const argSummary = computed(() => {
  const raw = callArgs.value
  if (!raw) return ''
  let args: Record<string, unknown>
  try {
    args = JSON.parse(raw)
  } catch {
    return ''
  }
  switch (props.group.tool) {
    case 'ReadFile':
    case 'WriteFile':
    case 'EditFile':
    case 'ListDir':
      return String(args.path || '')
    case 'RunCommand':
      return String(args.command || '')
    case 'HTTPGet':
    case 'HTTPPost':
      return String(args.url || '')
    case 'SearchInFiles':
      return String(args.pattern || '')
    default:
      return ''
  }
})
// 折叠态标题行：优先展示具体入参，其次回退到工具 path
const headline = computed(() => argSummary.value || path.value)
</script>

<template>
  <div class="rounded-lg border border-line bg-page my-2 overflow-hidden">
    <button class="w-full text-left px-3 py-2 flex items-center justify-between hover:bg-card transition-colors"
            @click="expanded = !expanded">
      <span class="flex items-center gap-2 text-xs">
        <el-icon class="text-blue-400 text-base"><Tools /></el-icon>
        <span class="font-mono text-ink">{{ group.tool }}</span>
        <el-tag size="small" effect="plain" class="!bg-transparent !border-line scale-90"
                :class="statusColor">
          <el-icon v-if="group.pending" class="is-loading text-[10px] mr-0.5"><Loading /></el-icon>
          <el-icon v-else-if="group.success" class="text-[10px] mr-0.5"><Check /></el-icon>
          <el-icon v-else class="text-[10px] mr-0.5"><Close /></el-icon>
          {{ statusLabel }}
        </el-tag>
        <span class="text-ink-2 text-[10px]">{{ group.agent }}</span>
        <span v-if="headline" class="text-ink-2 text-[10px] truncate max-w-[280px]" :title="headline">→ {{ headline }}</span>
      </span>
      <span class="flex items-center gap-2">
        <span class="text-[10px] text-ink-2">{{ fmtTime(startedAt) }}</span>
        <el-icon class="text-ink-2 transition-transform" :class="{'rotate-180': expanded}"><ArrowDown /></el-icon>
      </span>
    </button>

    <div v-show="expanded" class="px-3 pb-3 space-y-2 border-t border-line pt-2">
      <div v-if="callArgs">
        <div class="text-[10px] text-ink-2 mb-1">调用参数</div>
        <pre class="bg-page p-2 rounded text-[11px] text-ink whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ callArgs }}</pre>
      </div>
      <div v-if="resultText">
        <div class="text-[10px] text-ink-2 mb-1">执行结果</div>
        <pre class="bg-page p-2 rounded text-[11px] text-ink whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ resultText }}</pre>
        <div v-if="resultTruncated" class="text-[10px] text-yellow-500 mt-1">⚠ 结果已截断，仅显示前 500 字</div>
      </div>
      <div v-if="errorText">
        <div class="text-[10px] text-ink-2 mb-1">错误信息</div>
        <pre class="bg-page p-2 rounded text-[11px] text-red-400 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ errorText }}</pre>
      </div>
      <div v-if="!callArgs && !resultText && !errorText" class="text-[11px] text-ink-2">
        暂无详细信息
      </div>
    </div>
  </div>
</template>
