<script setup lang="ts">
defineProps<{
  icon?: string
  color?: string
  time?: string
  title?: string
  subtitle?: string
  active?: boolean
}>()
</script>

<template>
  <div class="timeline-item" :class="{ active }">
    <div class="line">
      <div class="dot" :style="{ background: color || '#3b82f6' }">
        <span v-if="icon">{{ icon }}</span>
      </div>
    </div>
    <div class="body">
      <div class="meta">
        <span v-if="time" class="time">{{ time }}</span>
        <span v-if="title" class="title">{{ title }}</span>
      </div>
      <div v-if="subtitle" class="subtitle">{{ subtitle }}</div>
      <slot />
    </div>
  </div>
</template>

<style scoped>
.timeline-item { display: flex; gap: 10px; padding: 6px 0; }
.line { position: relative; width: 20px; display: flex; justify-content: center; }
.line::before {
  content: '';
  position: absolute; top: 0; bottom: 0; left: 50%; width: 1px; background: var(--border, #243447);
}
.timeline-item:first-child .line::before { top: 10px; }
.timeline-item:last-child .line::before { bottom: auto; height: 10px; }
.dot {
  width: 16px; height: 16px; border-radius: 50%; z-index: 1;
  display: flex; align-items: center; justify-content: center;
  font-size: 9px; color: white; flex-shrink: 0; margin-top: 2px;
}
.body { flex: 1; min-width: 0; }
.meta { display: flex; align-items: center; gap: 8px; margin-bottom: 2px; }
.time { font-size: 10px; color: var(--text-muted, #64748b); }
.title { font-size: 12px; font-weight: 600; color: var(--text-primary, #e2e8f0); }
.subtitle { font-size: 11px; color: var(--text-secondary, #94a3b8); }
</style>
