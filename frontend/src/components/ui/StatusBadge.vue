<script setup lang="ts">
interface Props {
  status: string
  text?: string
  small?: boolean
}
const props = defineProps<Props>()

const map: Record<string, string> = {
  running: 'running',
  completed: 'success',
  success: 'success',
  done: 'success',
  error: 'error',
  failed: 'error',
  warning: 'warning',
  idle: 'idle',
  pending: 'pending',
  active: 'success',
  online: 'success',
  offline: 'error',
}

const theme = computed(() => {
  const s = (props.status || '').toLowerCase()
  return map[s] || 'idle'
})

import { computed } from 'vue'
</script>

<template>
  <span class="badge" :class="[theme, { small }]">
    <span class="dot"></span>
    <span class="txt">{{ text || status }}</span>
  </span>
</template>

<style scoped>
.badge {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 3px 8px;
  border-radius: 10px;
  font-size: 11px;
  font-weight: 700;
  text-transform: capitalize;
  background: rgba(100,116,139,0.12);
  color: #94a3b8;
}
.badge.small { padding: 1px 5px; font-size: 10px; gap: 3px; }
.dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.badge.running { background: rgba(59,130,246,0.12); color: #3b82f6; }
.badge.success { background: rgba(34,197,94,0.12); color: #22c55e; }
.badge.error { background: rgba(239,68,68,0.12); color: #ef4444; }
.badge.warning { background: rgba(234,179,8,0.12); color: #eab308; }
.badge.pending { background: rgba(168,85,247,0.12); color: #a855f7; }
</style>
