<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute } from 'vue-router'

const route = useRoute()
const sessionCount = ref(0)

const navItems = [
  { section: '概览', items: [
    { name: 'dashboard', label: '总览', icon: '▤' },
    { name: 'sessions', label: '会话', icon: '◈' },
  ]},
  { section: '探索', items: [
    { name: 'agents', label: '智能体树', icon: '⚇' },
    { name: 'board', label: '任务看板', icon: '▦' },
    { name: 'memory', label: '记忆', icon: '◉' },
    { name: 'skills', label: '技能', icon: '⚙' },
    { name: 'files', label: '文件', icon: '▣' },
  ]},
  { section: '系统', items: [
    { name: 'knowledge', label: '知识库', icon: '◐' },
    { name: 'history', label: '历史', icon: '◫' },
    { name: 'health', label: '健康', icon: '◍' },
    { name: 'settings', label: '设置', icon: '⚙' },
  ]},
]

onMounted(async () => {
  try {
    const r = await fetch('/api/sessions')
    const data = await r.json()
    sessionCount.value = data.length
  } catch {}
})
</script>

<template>
  <aside class="sidebar">
    <div class="brand">
      <div class="brand-icon">◆</div>
      <div class="brand-text">
        <div class="brand-title">BlockMemory</div>
        <div class="brand-sub">智能体控制台</div>
      </div>
    </div>
    <nav class="nav">
      <div v-for="group in navItems" :key="group.section" class="nav-group">
        <div class="nav-label">{{ group.section }}</div>
        <router-link
          v-for="item in group.items"
          :key="item.name"
          :to="{ name: item.name }"
          class="nav-item"
          :class="{ active: route.name === item.name }"
        >
          <span class="nav-icon">{{ item.icon }}</span>
          <span class="nav-text">{{ item.label }}</span>
          <span v-if="item.name === 'sessions' && sessionCount > 0" class="nav-badge">{{ sessionCount }}</span>
        </router-link>
      </div>
    </nav>
    <div class="footer">
      <div class="health">
        <span class="dot on"></span> PG
        <span class="dot on"></span> Redis
        <span class="dot on"></span> LLM
      </div>
      <div class="version">v3.0</div>
    </div>
  </aside>
</template>

<style scoped>
.sidebar {
  width: 220px; background: #111827; border-right: 1px solid #243447;
  display: flex; flex-direction: column; flex-shrink: 0;
}
.brand {
  display: flex; align-items: center; gap: 10px;
  padding: 16px 14px; border-bottom: 1px solid #243447;
}
.brand-icon {
  width: 32px; height: 32px; background: #3b82f6; border-radius: 4px;
  display: flex; align-items: center; justify-content: center;
  font-size: 16px; color: white; font-weight: 700;
}
.brand-title { font-size: 13px; font-weight: 700; color: #e2e8f0; }
.brand-sub { font-size: 10px; color: #64748b; }

.nav { flex: 1; overflow-y: auto; padding: 8px 0; }
.nav-group { margin-bottom: 8px; }
.nav-label {
  padding: 6px 14px; font-size: 10px; font-weight: 700;
  color: #64748b; text-transform: uppercase; letter-spacing: 0.8px;
}
.nav-item {
  display: flex; align-items: center; gap: 10px;
  padding: 7px 14px; margin: 0 6px; border-radius: 4px;
  color: #94a3b8; text-decoration: none; font-size: 12px;
  transition: all 0.15s;
}
.nav-item:hover { background: #1e293b; color: #e2e8f0; }
.nav-item.active { background: #2563eb; color: white; }
.nav-icon { font-size: 14px; width: 18px; text-align: center; }
.nav-text { flex: 1; }
.nav-badge {
  font-size: 10px; padding: 1px 6px; background: #1a2332;
  border-radius: 10px; color: #64748b; font-weight: 600;
}

.footer { padding: 10px 14px; border-top: 1px solid #243447; }
.health { display: flex; gap: 10px; margin-bottom: 6px; font-size: 10px; color: #64748b; }
.dot { width: 6px; height: 6px; border-radius: 50%; background: #ef4444; display: inline-block; margin-right: 3px; }
.dot.on { background: #22c55e; }
.version { font-size: 10px; color: #64748b; text-align: right; }
</style>
