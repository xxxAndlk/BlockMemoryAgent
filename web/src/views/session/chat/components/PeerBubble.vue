<script setup lang="ts">
// PeerBubble.vue Agent 对话里的"他人"气泡（左对齐）：区别于 UserBubble（右对齐"你"），
// 用于渲染上级派发的任务、调度器通知、其他 Agent 回报等非用户消息（多人对话效果）。
// 名称行文字色复用 agentTextColor（与监控页事件流口径一致）；气泡底色按 colorKey
// 哈希取模浅色系，同一发送者颜色稳定、不同发送者颜色不同。
import { computed } from 'vue'
import type { SessionEvent } from '@/types'
import { fmtTime, agentTextColor } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'

const props = defineProps<{
  event: SessionEvent
  /** 显示名（上级名称 / 调度器 / 验证循环等） */
  label: string
  /** 配色与文字色的哈希键（发送者 inst_id 或标识，保证同一发送者颜色稳定） */
  colorKey: string
}>()

// 浅色系气泡配色组（bg-50 / border-200 / text-700，对齐 UserBubble 的圆角内边距）
const PALETTES = [
  'bg-emerald-50 border-emerald-200 text-emerald-700',
  'bg-amber-50 border-amber-200 text-amber-700',
  'bg-violet-50 border-violet-200 text-violet-700',
  'bg-rose-50 border-rose-200 text-rose-700',
  'bg-cyan-50 border-cyan-200 text-cyan-700',
  'bg-indigo-50 border-indigo-200 text-indigo-700',
]

/** 简单稳定哈希：同一 colorKey 恒定命中同一组配色。 */
function hashKey(s: string): number {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0
  return h
}

const bubbleClass = computed(() => PALETTES[hashKey(props.colorKey || props.label) % PALETTES.length])
const labelColor = computed(() => agentTextColor(props.colorKey))
</script>

<template>
  <div class="flex justify-start mb-4">
    <div class="max-w-[75%] flex flex-col items-start gap-1">
      <div class="text-xs text-ink-2 flex items-center gap-2">
        <span class="flex items-center gap-1 px-1.5 py-0.5 rounded bg-page border border-line font-medium"
              :class="labelColor">
          <el-icon class="text-xs"><User /></el-icon> {{ label }}
        </span>
        <span>{{ fmtTime(event.timestamp) }}</span>
      </div>
      <div class="border rounded-2xl rounded-tl-md px-4 py-2.5 text-sm leading-relaxed shadow-sm markdown-body"
           :class="bubbleClass" v-html="renderMd(event.message)"></div>
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
