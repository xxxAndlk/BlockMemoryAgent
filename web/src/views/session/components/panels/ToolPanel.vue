<script setup lang="ts">
import { computed } from 'vue'
import type { SessionEvent } from '@/types'
import { isToolCallEvent, isToolExecEvent } from '@/types'

const props = defineProps<{ events: SessionEvent[] }>()

interface ToolRow {
  key: number
  tool: string
  path: string
  args: string
  output: string
  error: string
  success: boolean | null // null = 尚无结果帧
  time: string
  agent: string
}

function fmtTime(iso?: string) {
  return iso ? new Date(iso).toLocaleTimeString('zh-CN', { hour12: false }) : ''
}

const rows = computed<ToolRow[]>(() => {
  const calls = props.events.filter(isToolCallEvent)
  return calls.map((ev, i) => {
    // 结果配对：本次调用之后、下一次任意 tool_call 之前的首个同名 tool_exec 帧
    const next = calls[i + 1]
    const res = props.events.find((e) =>
      isToolExecEvent(e) &&
      e.tool === ev.tool &&
      e.timestamp >= ev.timestamp &&
      (!next || e.timestamp < next.timestamp)
    )
    return {
      key: i,
      tool: ev.tool || 'unknown',
      path: ev.tool_path || res?.tool_path || '',
      args: ev.tool_args || '',
      output: res?.tool_output || ev.tool_output || '',
      error: res?.tool_error || ev.tool_error || '',
      success: res ? res.success ?? !res.tool_error : (ev.success ?? null),
      time: fmtTime(ev.timestamp),
      agent: ev.agent || '',
    }
  })
})
</script>

<template>
  <div class="text-xs">
    <div v-if="!rows.length" class="text-ink-3 text-center py-8">暂无工具调用</div>
    <el-collapse v-else class="tool-panel">
      <el-collapse-item v-for="r in rows" :key="r.key">
        <template #title>
          <div class="flex items-center gap-2 w-full pr-2 min-w-0">
            <el-icon v-if="r.success === true" class="text-green-500 shrink-0"><CircleCheck /></el-icon>
            <el-icon v-else-if="r.success === false" class="text-red-500 shrink-0"><CircleClose /></el-icon>
            <el-icon v-else class="text-ink-3 shrink-0"><Clock /></el-icon>
            <span class="font-mono font-bold text-ink shrink-0">{{ r.tool }}</span>
            <span class="font-mono text-ink-3 truncate">{{ r.path }}</span>
            <span class="text-ink-3 ml-auto shrink-0">{{ r.time }}</span>
          </div>
        </template>
        <div class="space-y-2 pb-2">
          <div v-if="r.agent" class="text-ink-3">调用方：<span class="text-ink-2">{{ r.agent }}</span></div>
          <div v-if="r.args">
            <div class="text-ink-3 mb-1">参数</div>
            <pre class="bg-page border border-line rounded p-2 overflow-x-auto text-ink-2 whitespace-pre-wrap break-all">{{ r.args }}</pre>
          </div>
          <div v-if="r.output">
            <div class="text-ink-3 mb-1">输出</div>
            <pre class="bg-page border border-line rounded p-2 overflow-x-auto text-ink-2 whitespace-pre-wrap break-all max-h-64 overflow-y-auto">{{ r.output }}</pre>
          </div>
          <div v-if="r.error" class="text-red-500 break-all">错误：{{ r.error }}</div>
        </div>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<style scoped>
.tool-panel :deep(.el-collapse-item__header) {
  background-color: transparent;
  border-bottom: 1px solid var(--bma-border);
  height: 36px;
}
.tool-panel :deep(.el-collapse-item__wrap) {
  background-color: transparent;
  border-bottom: 1px solid var(--bma-border);
}
.tool-panel {
  border-top: none;
  border-bottom: none;
}
</style>
