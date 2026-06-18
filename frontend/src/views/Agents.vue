<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import type { Session, AgentNode } from '../types'

const sessions = ref<Session[]>([])
const activeSessionId = ref<string>('')
const agents = ref<AgentNode[]>([])
const selected = ref<AgentNode | null>(null)
const loading = ref(false)

onMounted(load)

async function load() {
  try {
    const r = await fetch('/api/sessions')
    const list: Session[] = await r.json()
    sessions.value = list
    // 默认选最近一个 running / 最后一个
    const running = list.find(s => s.status === 'running')
    const target = running || list[list.length - 1]
    if (target) {
      activeSessionId.value = target.id
      await loadAgents(target.id)
    }
  } catch {}
}

async function loadAgents(id: string) {
  activeSessionId.value = id
  loading.value = true
  try {
    const r = await fetch(`/api/sessions/${id}/agents`)
    const d = await r.json()
    agents.value = d.agents || []
  } catch {
    agents.value = []
  } finally {
    loading.value = false
  }
}

// 从扁平列表构建根节点（无 parent 的）+ children
const roots = computed(() => {
  const byId = new Map(agents.value.map(a => [a.inst_id, a]))
  const childMap = new Map<string, AgentNode[]>()
  agents.value.forEach(a => {
    if (!a.parent_id) return
    const arr = childMap.get(a.parent_id) || []
    arr.push(a)
    childMap.set(a.parent_id, arr)
  })
  const typeOrder = ['meta', 'domain', 'subdomain', 'fixed', 'dynamic']
  const sortFn = (a: AgentNode, b: AgentNode) =>
    typeOrder.indexOf(a.type) - typeOrder.indexOf(b.type) || a.name.localeCompare(b.name)
  const roots = agents.value.filter(a => !a.parent_id).sort(sortFn)
  const withChildren = (node: AgentNode): TreeNode => {
    const kids = (childMap.get(node.inst_id) || []).slice().sort(sortFn)
    return {
      node,
      children: kids.map(withChildren),
    }
  }
  return roots.map(withChildren)
})

interface TreeNode {
  node: AgentNode
  children: TreeNode[]
}

function statusLabel(s: string) {
  return ({ idle: '空闲', active: '运行中', waiting: '等待', calling: '调用中', done: '完成', error: '错误' } as Record<string, string>)[s] || s
}

function typeLabel(t: string) {
  return ({ meta: '主控', domain: '领域', subdomain: '子领域', fixed: '固定助手', dynamic: '动态助手' } as Record<string, string>)[t] || t
}
</script>

<template>
  <div class="layout">
    <div class="left">
      <div class="panel-header">
        <h3>智能体层级</h3>
        <select v-model="activeSessionId" @change="loadAgents(activeSessionId)" class="sess-select">
          <option v-for="s in sessions" :key="s.id" :value="s.id">
            {{ s.goal.slice(0, 24) }}{{ s.goal.length > 24 ? '…' : '' }} · {{ s.status }}
          </option>
        </select>
      </div>
      <div class="tree">
        <div v-if="loading" class="empty">加载中...</div>
        <div v-else-if="!roots.length" class="empty">无智能体实例（选择会话）</div>
        <template v-else>
          <div v-for="r in roots" :key="r.node.inst_id">
            <div
              class="node"
              :class="{ active: selected?.inst_id === r.node.inst_id }"
              @click="selected = r.node"
            >
              <span class="icon">◆</span>
              <span class="name">{{ r.node.name }}</span>
              <span :class="'tag tag-'+r.node.type">{{ typeLabel(r.node.type) }}</span>
              <span :class="'badge status-'+r.node.status">{{ statusLabel(r.node.status) }}</span>
            </div>
            <div v-for="c in r.children" :key="c.node.inst_id" class="child">
              <div
                class="node"
                :class="{ active: selected?.inst_id === c.node.inst_id }"
                @click="selected = c.node"
              >
                <span class="icon">◇</span>
                <span class="name">{{ c.node.name }}</span>
                <span v-if="c.node.domain" class="domain">{{ c.node.domain }}</span>
                <span :class="'tag tag-'+c.node.type">{{ typeLabel(c.node.type) }}</span>
                <span :class="'badge status-'+c.node.status">{{ statusLabel(c.node.status) }}</span>
              </div>
              <div v-for="cc in c.children" :key="cc.node.inst_id" class="child2">
                <div
                  class="node"
                  :class="{ active: selected?.inst_id === cc.node.inst_id }"
                  @click="selected = cc.node"
                >
                  <span class="icon">○</span>
                  <span class="name">{{ cc.node.name }}</span>
                  <span :class="'tag tag-'+cc.node.type">{{ typeLabel(cc.node.type) }}</span>
                  <span :class="'badge status-'+cc.node.status">{{ statusLabel(cc.node.status) }}</span>
                </div>
              </div>
            </div>
          </div>
        </template>
      </div>
    </div>
    <div class="right">
      <div class="panel-header"><h3>智能体详情</h3></div>
      <div v-if="!selected" class="empty">选择智能体查看详情</div>
      <div v-else class="detail">
        <div class="row"><span class="k">名称</span><span class="v">{{ selected.name }}</span></div>
        <div class="row"><span class="k">类型</span><span class="v"><span :class="'tag tag-'+selected.type">{{ typeLabel(selected.type) }}</span></span></div>
        <div class="row"><span class="k">领域</span><span class="v">{{ selected.domain || '-' }}</span></div>
        <div class="row"><span class="k">目标</span><span class="v">{{ selected.goal || '-' }}</span></div>
        <div class="row"><span class="k">状态</span><span class="v"><span :class="'badge status-'+selected.status">{{ statusLabel(selected.status) }}</span></span></div>
        <div class="row"><span class="k">实例ID</span><span class="v mono">{{ selected.inst_id }}</span></div>
        <div class="row"><span class="k">角色定义</span><span class="v mono">{{ selected.role_def_id }}</span></div>
        <div class="row"><span class="k">父实例</span><span class="v mono">{{ selected.parent_id || '-' }}</span></div>
        <div class="row"><span class="k">会话块</span><span class="v mono">{{ selected.block_id || '-' }}</span></div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 360px 1fr; gap: 16px; height: 100%; }
.left, .right { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; overflow: hidden; }
.panel-header { padding: 10px 14px; border-bottom: 1px solid #243447; display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.sess-select { background: #1a2332; border: 1px solid #243447; color: #e2e8f0; border-radius: 4px; padding: 4px 8px; font-size: 11px; max-width: 200px; }
.tree { flex: 1; overflow-y: auto; padding: 8px; }
.node { padding: 6px 8px; border-radius: 4px; margin-bottom: 2px; display: flex; align-items: center; gap: 8px; font-size: 12px; cursor: pointer; }
.node:hover { background: #1e293b; }
.node.active { background: rgba(59,130,246,0.15); border: 1px solid #3b82f6; }
.child { margin-left: 20px; border-left: 1px dashed #243447; padding-left: 4px; }
.child2 { margin-left: 20px; border-left: 1px dashed #243447; padding-left: 4px; }
.icon { font-size: 12px; color: #64748b; }
.name { flex: 1; color: #e2e8f0; }
.domain { font-size: 10px; color: #a855f7; background: rgba(168,85,247,0.12); padding: 1px 5px; border-radius: 3px; }
.tag { font-size: 9px; padding: 1px 5px; border-radius: 3px; font-weight: 600; }
.tag-meta { background: rgba(59,130,246,0.15); color: #3b82f6; }
.tag-domain { background: rgba(168,85,247,0.15); color: #a855f7; }
.tag-subdomain { background: rgba(234,179,8,0.15); color: #eab308; }
.tag-fixed { background: rgba(34,197,94,0.15); color: #22c55e; }
.tag-dynamic { background: rgba(236,72,153,0.15); color: #ec4899; }
.badge { font-size: 10px; padding: 1px 6px; border-radius: 10px; font-weight: 700; }
.status-idle { background: rgba(100,116,139,0.15); color: #64748b; }
.status-active, .status-calling { background: rgba(59,130,246,0.15); color: #3b82f6; }
.status-waiting { background: rgba(234,179,8,0.15); color: #eab308; }
.status-done { background: rgba(34,197,94,0.15); color: #22c55e; }
.status-error { background: rgba(239,68,68,0.15); color: #ef4444; }
.empty { padding: 60px 20px; text-align: center; color: #64748b; font-size: 12px; }
.detail { padding: 14px; display: flex; flex-direction: column; gap: 6px; }
.row { display: grid; grid-template-columns: 90px 1fr; gap: 12px; padding: 6px 0; border-bottom: 1px solid #1e293b; font-size: 12px; }
.k { color: #64748b; }
.v { color: #e2e8f0; word-break: break-all; }
.mono { font-family: 'JetBrains Mono', monospace; font-size: 11px; color: #94a3b8; }
</style>
