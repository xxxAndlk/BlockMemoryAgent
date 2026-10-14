<script setup lang="ts">
import { computed } from 'vue'
import type { SessionEvent } from '@/types'
import { attachmentsFromDetail } from '@/types'
import { fmtTime } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'
import ArtifactCard from './ArtifactCard.vue'

const props = defineProps<{
  event: SessionEvent
  /** 会话 ID（文件卡片的预览/下载/定位按会话工作区解析）；复用方未提供时卡片降级为纯展示 */
  sessionId?: string
}>()

/** 用户上传的文件附件（2026-10-14）：服务端落盘后挂 user_message 事件 detail_json。 */
const attachments = computed(() => attachmentsFromDetail(props.event.detail_json))

/**
 * 显示文本：有结构化附件时剥掉服务端追加在尾部的 [附件 N: ...] 引用行（卡片已含
 * 文件名/大小/操作，正文区重复且难看）；无附件时原文照旧。仅剥"尾部连续引用行"，
 * 防误伤用户正文（引用行格式由服务端单点生成）。
 */
const displayMessage = computed(() => {
  const msg = props.event.message
  if (!attachments.value.length) return msg
  const lines = msg.split('\n')
  while (lines.length && (lines[lines.length - 1].trim() === '' || /^\[附件 \d+: .*\]$/.test(lines[lines.length - 1].trim()))) {
    lines.pop()
  }
  return lines.join('\n')
})
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
      <div v-if="displayMessage" class="bg-primary text-white rounded-2xl rounded-tr-md px-4 py-2.5 text-sm leading-relaxed shadow markdown-body" v-html="renderMd(displayMessage)"></div>
      <!-- 文件附件卡片（用户粘贴/上传）：预览/下载/在文件夹中显示复用 ArtifactCard -->
      <div v-if="attachments.length" class="w-full flex flex-col gap-1.5">
        <ArtifactCard v-for="(a, i) in attachments" :key="i" :artifact="a" :session-id="sessionId || ''" />
      </div>
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
