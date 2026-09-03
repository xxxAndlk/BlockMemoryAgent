<script setup lang="ts">
import type { SessionEvent } from '@/types'
import { fmtTime } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'

defineProps<{ event: SessionEvent }>()
</script>

<template>
  <div class="flex justify-end mb-4">
    <div class="max-w-[75%] flex flex-col items-end gap-1">
      <div class="text-xs text-ink-2 flex items-center gap-2">
        <span>{{ fmtTime(event.timestamp) }}</span>
        <span class="flex items-center gap-1 px-1.5 py-0.5 rounded text-sky-700 bg-sky-100 border border-sky-300 font-medium dark:text-sky-200 dark:bg-sky-900/40 dark:border-sky-700/40">
          <el-icon class="text-xs"><UserFilled /></el-icon> 你
        </span>
      </div>
      <div class="bg-primary text-white rounded-2xl rounded-tr-md px-4 py-2.5 text-sm leading-relaxed shadow markdown-body" v-html="renderMd(event.message)"></div>
    </div>
  </div>
</template>

<style scoped>
.markdown-body :deep(p) { margin: 0; }
.markdown-body :deep(pre) {
  background: var(--bma-page);
  padding: 8px;
  border-radius: 4px;
  margin-top: 4px;
  overflow-x: auto;
  color: var(--bma-text);
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(a) { color: var(--bma-primary); }
</style>
