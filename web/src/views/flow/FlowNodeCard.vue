<template>
  <div class="flow-node relative min-w-[120px] max-w-[220px] rounded-lg border bg-card px-3 py-2 text-left transition-colors"
       :class="[selected ? 'border-primary shadow-md' : 'border-line', statusClass]">
    <!-- 入边锚点：start 无入边 -->
    <Handle v-if="def.type !== 'start'" type="target" :position="Position.Left" />
    <div class="flex items-center gap-1.5">
      <span class="w-2 h-2 rounded-full shrink-0" :style="{ background: color }"></span>
      <span class="text-[13px] font-bold text-ink truncate">{{ def.name || def.id }}</span>
      <span v-if="data.status" class="text-[10px] shrink-0" :class="statusTextClass">{{ statusText }}</span>
    </div>
    <div class="text-[10px] text-ink-3 font-mono mt-0.5">{{ typeLabel }}</div>
    <!-- 选中时的删除键（start 节点不可删） -->
    <button v-if="selected && def.type !== 'start'" type="button" title="删除节点"
            class="absolute -top-2 -right-2 w-[18px] h-[18px] rounded-full bg-red-500 text-white flex items-center justify-center text-[10px] leading-none hover:bg-red-600"
            @click.stop="emit('remove', def.id)">
      ×
    </button>
    <!-- 出边锚点：answer 无出边 -->
    <Handle v-if="def.type !== 'answer'" type="source" :position="Position.Right" />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import type { FlowNodeDef } from '@/api/flow'

/** 画布节点 data 载荷：定义 + 运行状态 + 删除回调。 */
export interface FlowGraphData extends Record<string, unknown> {
  def: FlowNodeDef
  /** node_run 状态（success/failed/waiting/running/skipped/pending，空=无运行）。 */
  status: string
  onRemove: (id: string) => void
}

const props = defineProps<{ data: FlowGraphData; selected: boolean }>()
const emit = defineEmits<{ (e: 'remove', id: string): void }>()

const TYPE_META: Record<string, { label: string; color: string }> = {
  start: { label: '开始 start', color: '#22c55e' },
  llm: { label: 'LLM', color: '#3b82f6' },
  tool: { label: '工具 tool', color: '#8b5cf6' },
  branch: { label: '分支 branch', color: '#f59e0b' },
  approval: { label: '审批 approval', color: '#ec4899' },
  answer: { label: '回答 answer', color: '#10b981' },
}

const def = computed(() => props.data.def)
const typeLabel = computed(() => TYPE_META[def.value.type]?.label || def.value.type)
const color = computed(() => TYPE_META[def.value.type]?.color || '#64748b')
const statusText = computed(() => {
  const map: Record<string, string> = {
    success: '成功', failed: '失败', waiting: '待审批', running: '运行中', skipped: '跳过', pending: '等待',
  }
  return map[props.data.status] || ''
})

/** 节点着色：运行状态映射左边框颜色。 */
const statusClass = computed(() => {
  switch (props.data.status) {
    case 'success': return '!border-l-4 !border-l-green-500'
    case 'failed': return '!border-l-4 !border-l-red-500'
    case 'waiting': return '!border-l-4 !border-l-orange-400'
    case 'running': return '!border-l-4 !border-l-blue-500'
    case 'skipped': return '!border-l-4 !border-l-gray-400'
    default: return ''
  }
})
const statusTextClass = computed(() => {
  switch (props.data.status) {
    case 'success': return 'text-green-500'
    case 'failed': return 'text-red-500'
    case 'waiting': return 'text-orange-400'
    case 'running': return 'text-blue-500'
    case 'skipped': return 'text-gray-400'
    default: return 'text-ink-3'
  }
})
</script>

<style scoped>
.flow-node {
  pointer-events: all;
}
</style>
