<template>
  <div class="overflow-x-auto">
    <table class="text-sm border-separate" style="border-spacing: 0 4px">
      <thead>
        <tr class="text-xs text-ink-2">
          <th class="text-left pr-4 font-normal sticky left-0 bg-card">件</th>
          <th v-for="s in stages" :key="s" class="text-left px-3 font-normal font-mono">{{ s }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="item in items" :key="item.id">
          <td class="pr-4 whitespace-nowrap sticky left-0 bg-card">
            <div class="text-sm">{{ item.name || item.id }}</div>
            <div class="text-xs text-ink-2 font-mono">{{ item.id }}</div>
          </td>
          <td v-for="s in stages" :key="s" class="px-2">
            <button
              class="w-full min-w-[92px] flex items-center gap-2 px-2.5 py-2 rounded-card border border-line hover:border-primary transition-colors"
              :class="cellClass(item.id, s)"
              :title="cellTitle(item.id, s)"
              @click="emit('open-cell', item.id, s)"
            >
              <span class="w-2.5 h-2.5 rounded-full shrink-0" :class="dotClass(item.id, s)" />
              <span class="text-xs truncate">{{ cellText(item.id, s) }}</span>
              <span v-if="attemptsOf(item.id, s) > 1" class="text-[10px] text-ink-2 ml-auto shrink-0">×{{ attemptsOf(item.id, s) }}</span>
            </button>
          </td>
        </tr>
      </tbody>
    </table>
    <div class="flex items-center gap-3 text-xs text-ink-2 mt-2 flex-wrap">
      <span class="flex items-center gap-1"><span class="w-2 h-2 rounded-full bg-green-500" />已采纳</span>
      <span class="flex items-center gap-1"><span class="w-2 h-2 rounded-full bg-yellow-400" />运行中</span>
      <span class="flex items-center gap-1"><span class="w-2 h-2 rounded-full bg-orange-400" />待审</span>
      <span class="flex items-center gap-1"><span class="w-2 h-2 rounded-full bg-red-500" />失败</span>
      <span class="flex items-center gap-1"><span class="w-2 h-2 rounded-full bg-gray-400" />排队/跳过</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import type { RosterItemInfo, WorkflowUnit } from '@/api/workflow'

const props = defineProps<{
  items: RosterItemInfo[]
  stages: string[]
  units: WorkflowUnit[]
}>()

const emit = defineEmits<{ (e: 'open-cell', itemId: string, stage: string): void }>()

const latestByCell = computed(() => {
  const map = new Map<string, WorkflowUnit>()
  for (const u of props.units) {
    const key = `${u.item_id}\u0000${u.stage}`
    const cur = map.get(key)
    if (!cur || u.attempt_no > cur.attempt_no) map.set(key, u)
  }
  return map
})

function latest(itemId: string, stage: string): WorkflowUnit | undefined {
  return latestByCell.value.get(`${itemId}\u0000${stage}`)
}
function attemptsOf(itemId: string, stage: string): number {
  return props.units.filter((u) => u.item_id === itemId && u.stage === stage).length
}

/** 状态点口径（对齐 OrchMiniCanvas dotClass：done 绿 / running 黄脉冲 / failed 红）。 */
function dotClass(itemId: string, stage: string): string {
  const s = latest(itemId, stage)?.status
  switch (s) {
    case 'done': return 'bg-green-500'
    case 'running': return 'bg-yellow-400 animate-pulse'
    case 'needs_review': return 'bg-orange-400'
    case 'failed': return 'bg-red-500'
    case 'skipped': return 'bg-gray-400 opacity-50'
    default: return 'bg-gray-400'
  }
}
function cellText(itemId: string, stage: string): string {
  const map: Record<string, string> = {
    pending: '排队', running: '运行中', done: '已采纳', needs_review: '待审', failed: '失败', skipped: '跳过',
  }
  const u = latest(itemId, stage)
  if (!u) return '—'
  return map[u.status] || u.status
}
function cellClass(itemId: string, stage: string): string {
  const s = latest(itemId, stage)?.status
  if (s === 'needs_review') return 'bg-orange-500/10'
  if (s === 'failed') return 'bg-red-500/10'
  return 'bg-transparent'
}
function cellTitle(itemId: string, stage: string): string {
  const u = latest(itemId, stage)
  if (!u) return '无单元'
  const parts = [`${u.status}`, `第 ${u.attempt_no} 次尝试`]
  if (u.reject_reason) parts.push(u.reject_reason)
  if (u.artifact_path) parts.push(u.artifact_path)
  return parts.join(' · ')
}
</script>
