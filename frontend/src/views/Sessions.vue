<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute } from 'vue-router'
import type { Session, SessionEvent } from '../types'
import { renderMd, esc } from '../utils/markdown'
import Panel from '../components/ui/Panel.vue'
import StatusBadge from '../components/ui/StatusBadge.vue'
import TimelineItem from '../components/ui/TimelineItem.vue'
import Sparkline from '../components/ui/Sparkline.vue'
import BarChart from '../components/ui/BarChart.vue'

const route = useRoute()
const sessions = ref<Session[]>([])
const active = ref<Session | null>(null)
const tab = ref('overview')
const filter = ref('all')
const evtSource = ref<EventSource | null>(null)
const expanded = ref<Set<string>>(new Set())

onMounted(load)

async function load() {
  try {
    const r = await fetch('/api/sessions')
    sessions.value = await r.json()
    const initialId = route.query.id as string
    if (initialId) {
      const s = sessions.value.find(x => x.id === initialId)
      if (s) select(s)
    } else if (!active.value && sessions.value.length) {
      select(sessions.value[0])
    }
  } catch {}
}

async function select(s: Session) {
  active.value = s
  tab.value = 'overview'
  if (evtSource.value) evtSource.value.close()
  evtSource.value = new EventSource(`/api/sessions/${s.id}/stream`)
  evtSource.value.onmessage = (e) => {
    const d = JSON.parse(e.data)
    if (d.type === 'done') { evtSource.value?.close(); load(); return }
    if (d.type) s.events.push(d)
  }
}

function eventCls(ev: SessionEvent) {
  return ev.kind ? `event-kind-${ev.kind}` : `event-${ev.type}`
}

function isDebugKind(ev: SessionEvent) {
  return ev.kind === 'prompt' || ev.kind === 'agent_created' || ev.kind === 'token_usage' || ev.kind === 'graph_step'
}

function toggle(id: string) {
  const s = expanded.value
  if (s.has(id)) s.delete(id)
  else s.add(id)
}

const filteredSessions = computed(() => {
  const list = sessions.value.slice().sort((a, b) => +new Date(b.started_at) - +new Date(a.started_at))
  if (filter.value === 'all') return list
  return list.filter(s => s.status === filter.value)
})

const tokenEvents = computed(() => active.value?.events.filter(e => e.kind === 'token_usage') || [])
const promptEvents = computed(() => active.value?.events.filter(e => e.kind === 'prompt') || [])
const agentEvents = computed(() => active.value?.events.filter(e => e.kind === 'agent_created') || [])
const stepEvents = computed(() => active.value?.events.filter(e => e.kind === 'graph_step') || [])

const tokenStats = computed(() => {
  const map = new Map<string, { calls: number; input: number; output: number }>()
  tokenEvents.value.forEach(ev => {
    const m = ev.message.match(/\[(.+?)\] Token 消耗: in=(\d+) out=(\d+) dur=(.+)/)
    if (!m) return
    const caller = m[1]
    const inT = parseInt(m[2]) || 0
    const outT = parseInt(m[3]) || 0
    const cur = map.get(caller) || { calls: 0, input: 0, output: 0 }
    cur.calls++
    cur.input += inT
    cur.output += outT
    map.set(caller, cur)
  })
  return Array.from(map.entries()).map(([caller, s]) => ({ caller, ...s }))
})

const tokenTotal = computed(() => tokenStats.value.reduce((a, s) => a + s.input + s.output, 0))
const totalCalls = computed(() => tokenStats.value.reduce((a, s) => a + s.calls, 0))
const duration = computed(() => {
  if (!active.value) return '-'
  const end = active.value.ended_at ? new Date(active.value.ended_at).getTime() : Date.now()
  const sec = Math.round((end - new Date(active.value.started_at).getTime()) / 1000)
  return sec < 60 ? `${sec}s` : `${Math.floor(sec / 60)}m ${sec % 60}s`
})

function parseStep(msg: string) {
  const m = msg.match(/Step (\d+): (.+?) → (.+?) \(action=(.+)\)/)
  if (!m) return null
  return { step: m[1], from: m[2], to: m[3], action: m[4] }
}

const agents = computed(() => {
  const list = agentEvents.value.map((ev, i) => {
    let meta: any = {}
    try { meta = JSON.parse(ev.detail_json || '{}') } catch {}
    return {
      id: meta.inst_id || `agent-${i}`,
      name: meta.name || ev.agent,
      role: meta.role || 'agent',
      domain: meta.domain || '-',
      status: meta.status || 'idle',
    }
  })
  if (!list.length && active.value) {
    return [
      { id: 'meta', name: 'MetaAgent', role: 'meta', domain: '-', status: 'active' },
      { id: 'ops', name: 'OpsAgent', role: 'domain', domain: 'Ops', status: 'running' },
      { id: 'dev', name: 'DevAgent', role: 'domain', domain: 'Dev', status: 'idle' },
    ]
  }
  return list
})

const taskBoard = computed(() => {
  if (!active.value) return null
  return {
    goal: active.value.goal,
    tasks: [
      { id: 't1', title: '解析目标并拆分 Domain', assignee: 'MetaAgent', status: 'completed' },
      { id: 't2', title: '调用 Ops 领域智能体', assignee: 'OpsAgent', status: 'running' },
      { id: 't3', title: '聚合结果并输出结论', assignee: 'MetaAgent', status: 'pending' },
    ]
  }
})

const tokenTrend = computed(() => {
  const arr = tokenEvents.value.map(ev => (ev.input_tokens || 0) + (ev.output_tokens || 0))
  return arr.length > 1 ? arr : [120, 210, 180, 340, 290]
})

const domainBar = computed(() => {
  const map = new Map<string, number>()
  active.value?.events.forEach(ev => {
    const d = ev.agent.split('Agent')[0]
    map.set(d, (map.get(d) || 0) + 1)
  })
  return Array.from(map.entries()).map(([label, value]) => ({ label, value, color: '#3b82f6' }))
})

const finalAnswer = computed(() => {
  const ev = active.value?.events.find(e => e.type === 'system' && e.message.startsWith('会话完成'))
  return ev ? ev.message.replace(/^会话完成:?\s*/, '') : ''
})

function kindIcon(k?: string) {
  const m: Record<string, string> = {
    think: '💭', intend: '🎯', llm: '🤖', tool_call: '🔧', tool_result: '🔧',
    wait: '⏳', error: '⚠', prompt: '📝', agent_created: '✨',
    token_usage: '📊', graph_step: '→',
  }
  return m[k || ''] || '•'
}
function kindColor(k?: string) {
  const m: Record<string, string> = {
    think: '#eab308', intend: '#3b82f6', llm: '#a855f7', tool_call: '#22c55e', tool_result: '#06b6d4',
    wait: '#64748b', error: '#ef4444', prompt: '#a78bfa', agent_created: '#818cf8',
    token_usage: '#64748b', graph_step: '#3b82f6',
  }
  return m[k || ''] || '#3b82f6'
}
function fmtTime(iso: string) {
  return new Date(iso).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}
function fmtDate(iso: string) {
  return new Date(iso).toLocaleString('zh-CN')
}
</script>

<template>
  <div class="layout">
    <!-- session list -->
    <aside class="list">
      <div class="panel-header">
        <span class="title">会话列表</span>
        <span class="count">{{ filteredSessions.length }}</span>
      </div>
      <div class="filters">
        <button v-for="f in ['all','running','completed','error']" :key="f" :class="{active: filter===f}" @click="filter=f">
          {{ {all:'全部', running:'运行中', completed:'已完成', error:'失败'}[f] }}
        </button>
      </div>
      <div class="cards">
        <div
          v-for="s in filteredSessions"
          :key="s.id"
          class="card"
          :class="{ active: active?.id === s.id }"
          @click="select(s)"
        >
          <div class="goal">{{ s.goal }}</div>
          <div class="meta">
            <StatusBadge :status="s.status" small />
            <span class="time">{{ fmtDate(s.started_at) }}</span>
          </div>
        </div>
      </div>
    </aside>

    <!-- detail -->
    <div v-if="active" class="detail">
      <!-- header -->
      <div class="detail-header">
        <div class="title-block">
          <div class="goal">{{ active.goal }}</div>
          <div class="meta-row">
            <StatusBadge :status="active.status" />
            <span class="chip">ID: {{ active.id.slice(-8) }}</span>
            <span class="chip">运行时长: {{ duration }}</span>
            <span class="chip">调用: {{ totalCalls }}</span>
            <span class="chip">Token: {{ tokenTotal.toLocaleString() }}</span>
          </div>
        </div>
        <div class="tabs">
          <button v-for="t in ['overview','agents','events','trace','metrics']" :key="t"
            :class="{active:tab===t}" @click="tab=t"
          >
            {{ {overview:'概览',agents:'智能体',events:'事件',trace:'追踪',metrics:'指标'}[t] }}
          </button>
        </div>
      </div>

      <!-- overview -->
      <div v-if="tab==='overview'" class="tab-grid">
        <Panel title="最终结果" class="wide">
          <div v-if="finalAnswer" class="markdown-body" v-html="renderMd(finalAnswer)"></div>
          <div v-else class="empty">等待会话完成...</div>
        </Panel>

        <Panel v-if="taskBoard" title="任务看板" :subtitle="taskBoard.goal">
          <div v-for="t in taskBoard.tasks" :key="t.id" class="task-row">
            <StatusBadge :status="t.status" small />
            <span class="task-title">{{ t.title }}</span>
            <span class="task-assign">{{ t.assignee }}</span>
          </div>
        </Panel>

        <Panel title="智能体参与">
          <div v-for="a in agents" :key="a.id" class="agent-row">
            <div class="agent-avatar">{{ a.name.slice(0, 2).toUpperCase() }}</div>
            <div class="agent-info">
              <div class="agent-name">{{ a.name }}</div>
              <div class="agent-role">{{ a.role }} · {{ a.domain }}</div>
            </div>
            <StatusBadge :status="a.status" small />
          </div>
        </Panel>
      </div>

      <!-- agents -->
      <div v-else-if="tab==='agents'" class="tab-grid three">
        <Panel v-for="a in agents" :key="a.id" :title="a.name" class="agent-card"
                 :subtitle="`${a.role} · ${a.domain}`"
        >
          <div class="agent-body">
            <div class="metric">
              <span class="label">状态</span>
              <StatusBadge :status="a.status" />
            </div>
            <div class="metric">
              <span class="label">ID</span>
              <span class="mono">{{ a.id }}</span>
            </div>
          </div>
        </Panel>
      </div>

      <!-- events timeline -->
      <div v-else-if="tab==='events'" class="tab-content">
        <TimelineItem
          v-for="(ev, i) in active.events"
          :key="i"
          :icon="kindIcon(ev.kind)"
          :color="kindColor(ev.kind)"
          :time="fmtTime(ev.timestamp)"
          :title="ev.agent"
          :subtitle="ev.kind || ev.type"
        >
          <div class="event-msg markdown-body" v-html="renderMd(ev.message)"></div>
          <pre v-if="ev.tool_output" class="out">{{ ev.tool_output }}</pre>
        </TimelineItem>
      </div>

      <!-- trace -->
      <div v-else-if="tab==='trace'" class="tab-content">
        <div class="trace-grid">
          <Panel title="Token 消耗统计">
            <table class="data-table">
              <thead><tr><th>调用者</th><th>次数</th><th>输入</th><th>输出</th><th>合计</th></tr></thead>
              <tbody>
                <tr v-for="s in tokenStats" :key="s.caller">
                  <td>{{ s.caller }}</td>
                  <td class="num">{{ s.calls }}</td>
                  <td class="num">{{ s.input }}</td>
                  <td class="num">{{ s.output }}</td>
                  <td class="num">{{ s.input + s.output }}</td>
                </tr>
                <tr v-if="!tokenStats.length"><td colspan="5" class="empty">暂无数据</td></tr>
              </tbody>
            </table>
          </Panel>

          <Panel title="Prompt 查看器" :subtitle="`${promptEvents.length} 条`">
            <div v-for="(ev,i) in promptEvents" :key="i" class="collapsible"
            >
              <div class="collapsible-hd" @click="toggle('prompt-'+i)"
              >
                <span>{{ ev.agent }} · {{ fmtTime(ev.timestamp) }}</span>
                <span class="toggle">{{ expanded.has('prompt-'+i) ? '收起' : '展开' }}</span>
              </div>
              <pre v-if="expanded.has('prompt-'+i)" class="code">{{ ev.prompt || ev.detail_json || '' }}</pre>
            </div>
          </Panel>

          <Panel title="Agent 创建历史" :subtitle="`${agentEvents.length} 条`">
            <div v-for="(ev,i) in agentEvents" :key="i" class="agent-create"
            >
              <div class="agent-create-title">{{ ev.message }}</div>
              <pre class="code">{{ ev.detail_json || '' }}</pre>
            </div>
          </Panel>

          <Panel title="图执行步骤" :subtitle="`${stepEvents.length} 步`">
            <div class="step-flow"
            >
              <div v-for="(ev,i) in stepEvents" :key="i" class="step-item"
              >
                <template v-if="parseStep(ev.message)"
>
                  <span class="num">#{{ parseStep(ev.message)!.step }}</span>
                  <span class="node">{{ parseStep(ev.message)!.from }}</span>
                  <span class="arrow">→</span>
                  <span class="node">{{ parseStep(ev.message)!.to }}</span>
                  <span class="action">{{ parseStep(ev.message)!.action }}</span>
                </template>
                <template v-else
>
                  <span class="num">#{{ i+1 }}</span>
                  <span>{{ ev.message }}</span>
                </template>
              </div>
            </div>
          </Panel>
        </div>
      </div>

      <!-- metrics -->
      <div v-else-if="tab==='metrics'" class="tab-grid">
        <Panel title="Token 趋势" class="chart-panel">
          <Sparkline :data="tokenTrend" color="#3b82f6" fill height="80" />
        </Panel>
        <Panel title="智能体事件分布" class="chart-panel">
          <BarChart :data="domainBar" height="80" />
        </Panel>
        <Panel title="关键指标" class="metric-panel"
        >
          <div class="metric-list"
          >
            <div class="metric-row"
>
              <span>会话状态</span>
              <StatusBadge :status="active.status" />
            </div>
            <div class="metric-row"
>
              <span>运行时长</span>
              <span class="val">{{ duration }}</span>
            </div>
            <div class="metric-row"
>
              <span>LLM 调用</span>
              <span class="val">{{ totalCalls }}</span>
            </div>
            <div class="metric-row"
>
              <span>总 Token</span>
              <span class="val">{{ tokenTotal.toLocaleString() }}</span>
            </div>
          </div>
        </Panel>
      </div>
    </div>

    <div v-else class="empty-state">
      选择一个会话查看详情
    </div>
  </div>
</template>

<style scoped>
.layout {
  display: grid;
  grid-template-columns: 280px 1fr;
  gap: 12px;
  height: 100%;
  padding: 12px;
}
.list {
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.panel-header {
  padding: 12px 14px;
  border-bottom: 1px solid var(--border, #243447);
  display: flex; justify-content: space-between; align-items: center;
}
.panel-header .title { font-size: 13px; font-weight: 700; color: var(--text-primary, #e2e8f0); }
.panel-header .count { font-size: 11px; color: var(--text-muted, #64748b); background: var(--bg-tertiary, #1a2332); padding: 1px 8px; border-radius: 10px; }
.filters { display: flex; gap: 6px; padding: 10px 14px; border-bottom: 1px solid var(--border, #243447); }
.filters button {
  background: transparent; border: 1px solid var(--border, #243447); border-radius: 12px;
  padding: 3px 10px; font-size: 11px; color: var(--text-secondary, #94a3b8); cursor: pointer;
}
.filters button.active { background: var(--accent-blue, #3b82f6); color: white; border-color: var(--accent-blue, #3b82f6); }
.cards { flex: 1; overflow-y: auto; padding: 8px; }
.card {
  padding: 10px;
  border-radius: 6px;
  margin-bottom: 6px;
  background: var(--bg-tertiary, #1a2332);
  border: 1px solid var(--border, #243447);
  cursor: pointer;
}
.card:hover { border-color: var(--accent-blue, #3b82f6); }
.card.active { border-color: var(--accent-blue, #3b82f6); background: rgba(59,130,246,0.1); }
.card .goal { font-size: 12px; color: var(--text-primary, #e2e8f0); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-bottom: 6px; }
.card .meta { display: flex; justify-content: space-between; align-items: center; font-size: 10px; }
.card .time { color: var(--text-muted, #64748b); }

.detail {
  display: flex;
  flex-direction: column;
  gap: 12px;
  overflow: hidden;
}
.detail-header {
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  padding: 14px 16px;
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  flex-wrap: wrap;
  gap: 12px;
}
.title-block { display: flex; flex-direction: column; gap: 8px; }
.detail-header .goal { font-size: 16px; font-weight: 700; color: var(--text-primary, #e2e8f0); }
.meta-row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
.chip {
  font-size: 11px; color: var(--text-secondary, #94a3b8);
  background: var(--bg-tertiary, #1a2332);
  border: 1px solid var(--border, #243447);
  border-radius: 10px; padding: 2px 8px;
}
.tabs { display: flex; gap: 4px; }
.tabs button {
  background: transparent; border: none; border-bottom: 2px solid transparent;
  padding: 8px 12px; color: var(--text-muted, #64748b); font-size: 12px; cursor: pointer;
}
.tabs button.active { color: var(--accent-blue, #3b82f6); border-bottom-color: var(--accent-blue, #3b82f6); }

.tab-content { flex: 1; overflow-y: auto; padding: 4px; }
.tab-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 12px;
  flex: 1;
  overflow-y: auto;
  align-content: start;
}
.tab-grid.three { grid-template-columns: repeat(3, 1fr); }
.tab-grid .wide { grid-column: 1 / -1; }

.task-row {
  display: flex; align-items: center; gap: 10px; padding: 8px 0; border-bottom: 1px solid var(--border, #243447);
}
.task-row:last-child { border-bottom: none; }
.task-title { flex: 1; font-size: 12px; color: var(--text-primary, #e2e8f0); }
.task-assign { font-size: 11px; color: var(--text-muted, #64748b); }

.agent-row {
  display: flex; align-items: center; gap: 10px; padding: 8px 0; border-bottom: 1px solid var(--border, #243447);
}
.agent-row:last-child { border-bottom: none; }
.agent-avatar {
  width: 32px; height: 32px; border-radius: 50%; background: var(--accent-blue, #3b82f6);
  display: flex; align-items: center; justify-content: center; font-size: 10px; font-weight: 700; color: white;
}
.agent-info { flex: 1; min-width: 0; }
.agent-name { font-size: 12px; font-weight: 600; color: var(--text-primary, #e2e8f0); }
.agent-role { font-size: 10px; color: var(--text-muted, #64748b); }

.agent-card .agent-body { display: flex; flex-direction: column; gap: 10px; }
.agent-card .metric { display: flex; justify-content: space-between; align-items: center; }
.agent-card .label { font-size: 11px; color: var(--text-muted, #64748b); }

.event-msg { font-size: 12px; color: var(--text-secondary, #94a3b8); margin-top: 4px; }
.out {
  margin-top: 6px; padding: 8px; background: #0a0e17;
  border: 1px solid var(--border, #243447); border-radius: 4px;
  font-size: 11px; white-space: pre-wrap; overflow-x: auto; color: var(--text-secondary, #94a3b8);
}

.trace-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 12px; flex: 1; overflow-y: auto; align-content: start;
}
.data-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.data-table th { text-align: left; padding: 8px; color: var(--text-muted, #64748b); border-bottom: 1px solid var(--border, #243447); }
.data-table td { padding: 8px; border-bottom: 1px solid var(--border, #243447); color: var(--text-secondary, #94a3b8); }
.data-table .num { text-align: right; font-variant-numeric: tabular-nums; }

.collapsible { border: 1px solid var(--border, #243447); border-radius: 4px; margin-bottom: 8px; overflow: hidden; }
.collapsible-hd {
  display: flex; justify-content: space-between; align-items: center;
  padding: 8px 10px; background: var(--bg-tertiary, #1a2332); cursor: pointer; font-size: 11px; color: var(--text-secondary, #94a3b8);
}
.collapsible-hd:hover { background: var(--bg-hover, #1e293b); }
.collapsible .toggle { color: var(--accent-blue, #3b82f6); }
.code {
  padding: 10px; background: #0a0e17; font-family: 'JetBrains Mono', monospace;
  font-size: 11px; color: var(--text-secondary, #94a3b8); white-space: pre-wrap; word-break: break-all;
  max-height: 260px; overflow-y: auto;
}
.agent-create { margin-bottom: 10px; }
.agent-create-title { font-size: 12px; font-weight: 600; color: var(--text-primary, #e2e8f0); margin-bottom: 4px; }

.step-flow { display: flex; flex-direction: column; gap: 4px; }
.step-item { display: flex; align-items: center; gap: 8px; padding: 6px 8px; background: var(--bg-tertiary, #1a2332); border-radius: 4px; font-size: 12px; }
.step-item .num { font-size: 10px; color: var(--text-muted, #64748b); min-width: 30px; }
.step-item .node { color: var(--text-primary, #e2e8f0); }
.step-item .arrow { color: var(--accent-blue, #3b82f6); font-weight: 700; }
.step-item .action { font-size: 10px; padding: 1px 6px; background: rgba(59,130,246,0.12); color: var(--accent-blue, #3b82f6); border-radius: 3px; }

.metric-panel .metric-list { display: flex; flex-direction: column; gap: 12px; }
.metric-row { display: flex; justify-content: space-between; align-items: center; font-size: 12px; }
.metric-row span:first-child { color: var(--text-muted, #64748b); }
.metric-row .val { font-weight: 700; color: var(--text-primary, #e2e8f0); }
.chart-panel { min-height: 160px; }
.empty { padding: 30px; text-align: center; color: var(--text-muted, #64748b); }
.empty-state { display: flex; align-items: center; justify-content: center; color: var(--text-muted, #64748b); font-size: 14px; }

@media (max-width: 1200px) {
  .tab-grid, .tab-grid.three, .trace-grid { grid-template-columns: 1fr; }
  .detail-header { flex-direction: column; }
}
@media (max-width: 900px) {
  .layout { grid-template-columns: 1fr; }
  .list { display: none; }
}
</style>
