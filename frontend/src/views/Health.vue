<script setup lang="ts">
const services = [
  { name: 'PostgreSQL', icon: '🗄', online: true, detail: 'agent_private_memory: active' },
  { name: 'Redis', icon: '⚡', online: true, detail: 'snapshots: active' },
  { name: 'LLM API', icon: '🧠', online: true, detail: 'avg latency: ~4s' },
]
</script>

<template>
  <div class="grid">
    <div v-for="s in services" :key="s.name" class="card">
      <div class="header">
        <span class="icon">{{ s.icon }}</span>
        <span class="name">{{ s.name }}</span>
      </div>
      <div :class="'status ' + (s.online?'online':'offline')">{{ s.online?'已连接':'离线' }}</div>
      <div class="detail">{{ s.detail }}</div>
    </div>
  </div>
</template>

<style scoped>
.grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 16px; max-width: 700px; }
.card { background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 20px; text-align: center; }
.header { display: flex; align-items: center; justify-content: center; gap: 8px; margin-bottom: 12px; }
.icon { font-size: 24px; }
.name { font-size: 14px; font-weight: 700; }
.status { font-size: 13px; font-weight: 600; padding: 4px 12px; border-radius: 12px; display: inline-block; margin-bottom: 8px; }
.status.online { background: rgba(34,197,94,0.15); color: #22c55e; }
.status.offline { background: rgba(239,68,68,0.15); color: #ef4444; }
.detail { font-size: 12px; color: #64748b; }
</style>
