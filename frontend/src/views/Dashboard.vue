<script setup lang="ts">
import { ref, onMounted } from 'vue'
import type { Session } from '../types'

const sessions = ref<Session[]>([])
const stats = ref({ total: 0, completed: 0, llmCalls: 0, avgTime: '0s' })

onMounted(async () => {
  try {
    const r = await fetch('/api/sessions')
    sessions.value = await r.json()
    stats.value.total = sessions.value.length
    stats.value.completed = sessions.value.filter(s => s.status === 'completed').length
  } catch {}
})

function quick(g: string) {
  fetch('/api/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ goal: g })
  })
}
</script>

<template>
  <div class="grid">
    <div class="card stats">
      <div class="card-header"><h3>今日统计</h3></div>
      <div class="stat-grid">
        <div class="stat"><div class="val">{{ stats.total }}</div><div class="lab">会话数</div></div>
        <div class="stat"><div class="val">{{ stats.completed }}</div><div class="lab">已完成</div></div>
        <div class="stat"><div class="val">{{ stats.llmCalls }}</div><div class="lab">LLM调用</div></div>
        <div class="stat"><div class="val">{{ stats.avgTime }}</div><div class="lab">平均耗时</div></div>
      </div>
    </div>
    <div class="card">
      <div class="card-header"><h3>快捷启动</h3></div>
      <div class="quick">
        <button @click="quick('分析代码结构')">🔍 分析代码</button>
        <button @click="quick('编写新模块')">✏️ 写文件</button>
        <button @click="quick('运行测试')">▶️ 执行命令</button>
        <button @click="quick('搜索知识库')">📚 搜索知识</button>
      </div>
    </div>
    <div class="card wide">
      <div class="card-header"><h3>最近会话</h3></div>
      <div class="list">
        <router-link
          v-for="s in sessions.slice().reverse().slice(0,5)"
          :key="s.id"
          :to="{ name: 'sessions' }"
          class="row"
        >
          <span class="goal">{{ s.goal }}</span>
          <span :class="'badge status-'+s.status">{{ s.status }}</span>
        </router-link>
      </div>
    </div>
  </div>
</template>

<style scoped>
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; max-width: 900px; }
.card { background: #161f2e; border: 1px solid #243447; border-radius: 8px; overflow: hidden; }
.card.wide { grid-column: 1 / -1; }
.card-header { padding: 12px 14px; border-bottom: 1px solid #243447; }
.card-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.stat-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 1px; background: #243447; }
.stat { background: #161f2e; padding: 16px; text-align: center; }
.val { font-size: 24px; font-weight: 700; color: #3b82f6; }
.lab { font-size: 11px; color: #64748b; margin-top: 4px; }
.quick { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; padding: 12px; }
.quick button {
  background: #1a2332; border: 1px solid #243447; border-radius: 4px;
  padding: 10px; color: #94a3b8; font-size: 12px; cursor: pointer; text-align: left;
}
.quick button:hover { border-color: #3b82f6; color: #e2e8f0; }
.list { padding: 8px; }
.row {
  display: flex; justify-content: space-between; align-items: center;
  padding: 8px 10px; border-radius: 4px; text-decoration: none; color: inherit;
}
.row:hover { background: #1e293b; }
.goal { font-size: 12px; color: #e2e8f0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.badge { font-size: 10px; padding: 1px 8px; border-radius: 10px; font-weight: 700; }
.status-running { background: rgba(59,130,246,0.15); color: #3b82f6; }
.status-completed { background: rgba(34,197,94,0.15); color: #22c55e; }
.status-error { background: rgba(239,68,68,0.15); color: #ef4444; }
</style>
