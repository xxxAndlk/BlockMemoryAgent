<script setup lang="ts">
import { ref, onMounted } from 'vue'
import type { Session } from '../types'

const sessions = ref<Session[]>([])
const activeSession = ref<Session | null>(null)

onMounted(async () => {
  const r = await fetch('/api/sessions')
  sessions.value = await r.json()
  activeSession.value = sessions.value[sessions.value.length - 1]
})

const agents = ref([
  { name: 'MetaAgent', type: 'meta', status: 'done', children: [
    { name: 'DomainAgent[通用]', type: 'domain', status: 'done', children: [
      { name: '贪吃蛇游戏运行助手', type: 'dynamic', status: 'done' },
    ]},
  ]},
])
</script>

<template>
  <div class="layout">
    <div class="left">
      <div class="panel-header"><h3>智能体层级</h3></div>
      <div class="tree">
        <div v-for="a in agents" :key="a.name" class="node">
          <span class="icon">◆</span><span class="name">{{ a.name }}</span>
          <span :class="'tag tag-'+a.type">{{ a.type }}</span>
          <span :class="'badge status-'+a.status">{{ a.status }}</span>
          <div v-for="c in a.children" :key="c.name" class="node child">
            <span class="icon">◆</span><span class="name">{{ c.name }}</span>
            <span :class="'tag tag-'+c.type">{{ c.type }}</span>
            <div v-for="cc in c.children" :key="cc.name" class="node child2">
              <span class="icon">◆</span><span class="name">{{ cc.name }}</span>
              <span :class="'tag tag-'+cc.type">{{ cc.type }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
    <div class="right">
      <div class="panel-header"><h3>智能体详情</h3></div>
      <div class="empty">选择智能体查看详情</div>
    </div>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 300px 1fr; gap: 16px; height: 100%; }
.left, .right { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; overflow: hidden; }
.panel-header { padding: 10px 14px; border-bottom: 1px solid #243447; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.tree { flex: 1; overflow-y: auto; padding: 8px; }
.node { padding: 6px 8px; border-radius: 4px; margin-bottom: 2px; display: flex; align-items: center; gap: 8px; font-size: 12px; }
.child { margin-left: 20px; }
.child2 { margin-left: 20px; }
.icon { font-size: 12px; }
.name { flex: 1; }
.tag { font-size: 9px; padding: 1px 5px; border-radius: 3px; font-weight: 600; }
.tag-meta { background: rgba(59,130,246,0.15); color: #3b82f6; }
.tag-domain { background: rgba(168,85,247,0.15); color: #a855f7; }
.tag-dynamic { background: rgba(234,179,8,0.15); color: #eab308; }
.empty { padding: 60px 20px; text-align: center; color: #64748b; }
</style>
