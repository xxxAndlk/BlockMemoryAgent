<script setup lang="ts">
import type { SessionEvent } from '@/types'
import { fmtTime } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'

defineProps<{ event: SessionEvent }>()
</script>

<template>
  <div class="flex justify-end mb-4">
    <div class="max-w-[75%] flex flex-col items-end gap-1">
      <div class="text-xs text-gray-500 flex items-center gap-2">
        <span>{{ fmtTime(event.timestamp) }}</span>
        <span class="flex items-center gap-1 px-1.5 py-0.5 rounded text-sky-200 bg-sky-900/40 border border-sky-700/40 font-medium">
          <el-icon class="text-xs"><UserFilled /></el-icon> 你
        </span>
      </div>
      <div class="bg-[#1e3a8a] text-gray-100 rounded-2xl rounded-tr-md px-4 py-2.5 text-sm leading-relaxed shadow markdown-body" v-html="renderMd(event.message)"></div>
    </div>
  </div>
</template>

<style scoped>
.markdown-body :deep(p) { margin: 0; }
.markdown-body :deep(pre) {
  background: #0f1115;
  padding: 8px;
  border-radius: 4px;
  margin-top: 4px;
  overflow-x: auto;
  color: #e5e7eb;
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(a) { color: #93c5fd; }
</style>
