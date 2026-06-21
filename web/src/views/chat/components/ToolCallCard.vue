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

const callArgs = computed(() => props.group.call?.detail_json || '')
const resultText = computed(() => props.group.result?.tool_output || props.group.result?.message || '')
const errorText = computed(() => props.group.result?.tool_error || '')
const path = computed(() => props.group.result?.tool_path || '')
const startedAt = computed(() => props.group.call?.timestamp || props.group.result?.timestamp || '')
</script>

<template>
  <div class="rounded-lg border border-[#2a2d35] bg-[#0f1115] my-2 overflow-hidden">
    <button class="w-full text-left px-3 py-2 flex items-center justify-between hover:bg-[#14161a] transition-colors"
            @click="expanded = !expanded">
      <span class="flex items-center gap-2 text-xs">
        <el-icon class="text-blue-400 text-base"><Tools /></el-icon>
        <span class="font-mono text-gray-200">{{ group.tool }}</span>
        <el-tag size="small" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90"
                :class="statusColor">
          <el-icon v-if="group.pending" class="is-loading text-[10px] mr-0.5"><Loading /></el-icon>
          <el-icon v-else-if="group.success" class="text-[10px] mr-0.5"><Check /></el-icon>
          <el-icon v-else class="text-[10px] mr-0.5"><Close /></el-icon>
          {{ statusLabel }}
        </el-tag>
        <span class="text-gray-500 text-[10px]">{{ group.agent }}</span>
        <span v-if="path" class="text-gray-500 text-[10px] truncate max-w-[280px]" :title="path">→ {{ path }}</span>
      </span>
      <span class="flex items-center gap-2">
        <span class="text-[10px] text-gray-500">{{ fmtTime(startedAt) }}</span>
        <el-icon class="text-gray-500 transition-transform" :class="{'rotate-180': expanded}"><ArrowDown /></el-icon>
      </span>
    </button>

    <div v-show="expanded" class="px-3 pb-3 space-y-2 border-t border-[#2a2d35] pt-2">
      <div v-if="callArgs">
        <div class="text-[10px] text-gray-500 mb-1">调用参数</div>
        <pre class="bg-[#0a0c10] p-2 rounded text-[11px] text-gray-300 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ callArgs }}</pre>
      </div>
      <div v-if="resultText">
        <div class="text-[10px] text-gray-500 mb-1">执行结果</div>
        <pre class="bg-[#0a0c10] p-2 rounded text-[11px] text-gray-300 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ resultText }}</pre>
      </div>
      <div v-if="errorText">
        <div class="text-[10px] text-gray-500 mb-1">错误信息</div>
        <pre class="bg-[#0a0c10] p-2 rounded text-[11px] text-red-400 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ errorText }}</pre>
      </div>
      <div v-if="!callArgs && !resultText && !errorText" class="text-[11px] text-gray-500">
        暂无详细信息
      </div>
    </div>
  </div>
</template>
