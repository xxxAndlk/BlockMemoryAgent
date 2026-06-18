<script setup lang="ts">
defineProps<{
  label: string
  value: string | number
  change?: string
  up?: boolean
}>()
</script>

<template>
  <div class="stat-card">
    <div class="top">
      <span class="label">{{ label }}</span>
      <span v-if="change" class="change" :class="up ? 'up' : 'down'">{{ change }}</span>
    </div>
    <div class="value">{{ value }}</div>
  </div>
</template>

<style scoped>
.stat-card {
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  padding: 14px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.top { display: flex; justify-content: space-between; align-items: center; }
.label { font-size: 11px; color: var(--text-muted, #64748b); }
.value { font-size: 24px; font-weight: 700; color: var(--text-primary, #e2e8f0); }
.change { font-size: 10px; font-weight: 700; padding: 1px 6px; border-radius: 10px; }
.change.up { background: rgba(34,197,94,0.12); color: #22c55e; }
.change.down { background: rgba(239,68,68,0.12); color: #ef4444; }
</style>
