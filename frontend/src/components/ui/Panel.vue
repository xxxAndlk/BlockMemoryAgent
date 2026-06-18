<script setup lang="ts">
defineProps<{
  title?: string
  subtitle?: string
  flush?: boolean
}>()
</script>

<template>
  <div class="panel" :class="{ flush }">
    <div v-if="title" class="panel-hd">
      <div class="panel-titles">
        <span class="panel-title">{{ title }}</span>
        <span v-if="subtitle" class="panel-sub">{{ subtitle }}</span>
      </div>
      <div class="panel-actions">
        <slot name="actions"></slot>
      </div>
    </div>
    <div class="panel-bd">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.panel {
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.panel.flush .panel-bd { padding: 0; }
.panel-hd {
  padding: 12px 14px;
  border-bottom: 1px solid var(--border, #243447);
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  min-height: 44px;
}
.panel-titles { display: flex; align-items: baseline; gap: 10px; }
.panel-title { font-size: 13px; font-weight: 700; color: var(--text-primary, #e2e8f0); }
.panel-sub { font-size: 11px; color: var(--text-muted, #64748b); }
.panel-actions { display: flex; gap: 8px; align-items: center; }
.panel-bd { flex: 1; padding: 12px; overflow: hidden; display: flex; flex-direction: column; }
</style>
