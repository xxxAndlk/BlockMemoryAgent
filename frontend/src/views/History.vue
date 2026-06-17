<script setup lang="ts">
import { ref, onMounted } from 'vue'
import type { Session } from '../types'
const sessions = ref<Session[]>([])
onMounted(async () => {
  const r = await fetch('/api/sessions')
  sessions.value = await r.json()
})
</script>

<template>
  <div class="layout">
    <div class="panel-header"><h3>会话历史</h3></div>
    <div class="timeline">
      <div v-for="s in sessions.slice().reverse()" :key="s.id" class="item">
        <div class="goal">{{ s.goal }}</div>
        <div class="summary">{{ s.result || '无结果' }}</div>
        <div class="meta">
          <span>{{ s.id }}</span>
          <span :class="'badge status-'+s.status">{{ s.status }}</span>
          <span>{{ new Date(s.started_at).toLocaleTimeString() }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.layout { max-width: 800px; }
.panel-header { padding: 10px 14px; background: #161f2e; border: 1px solid #243447; border-radius: 8px 8px 0 0; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.timeline { display: flex; flex-direction: column; gap: 8px; margin-top: 12px; }
.item { background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 14px; position: relative; }
.item::before { content: ''; position: absolute; left: 0; top: 0; bottom: 0; width: 3px; background: #3b82f6; border-radius: 8px 0 0 8px; }
.goal { font-size: 13px; font-weight: 600; color: #e2e8f0; margin-bottom: 6px; }
.summary { font-size: 12px; color: #94a3b8; margin-bottom: 8px; }
.meta { display: flex; gap: 12px; font-size: 11px; color: #64748b; }
.badge { font-size: 10px; padding: 1px 8px; border-radius: 10px; font-weight: 700; }
</style>
