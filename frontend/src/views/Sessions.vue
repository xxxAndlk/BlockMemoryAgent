<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import type { Session, SessionEvent } from '../types'

const sessions = ref<Session[]>([])
const active = ref<Session | null>(null)
const tab = ref('log')
const evtSource = ref<EventSource | null>(null)
const expanded = ref<Set<string>>(new Set())

onMounted(load)

async function load() {
  const r = await fetch('/api/sessions')
  sessions.value = await r.json()
}

async function select(s: Session) {
  active.value = s
  tab.value = 'log'
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

function esc(s: string) {
  const d = document.createElement('div'); d.textContent = s; return d.innerHTML
}

function isDebugKind(ev: SessionEvent) {
  return ev.kind === 'prompt' || ev.kind === 'agent_created' || ev.kind === 'token_usage' || ev.kind === 'graph_step'
}

function toggle(id: string) {
  const s = expanded.value
  if (s.has(id)) s.delete(id)
  else s.add(id)
}

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

function parseStep(msg: string) {
  const m = msg.match(/Step (\d+): (.+?) → (.+?) \(action=(.+)\)/)
  if (!m) return null
  return { step: m[1], from: m[2], to: m[3], action: m[4] }
}
</script>

<template>
  <div class="layout">
    <div class="list">
      <div class="panel-header"><h3>会话列表</h3></div>
      <div class="cards">
        <div
          v-for="s in sessions.slice().reverse()"
          :key="s.id"
          class="card"
          :class="[s.status, active?.id===s.id?'active':'']"
          @click="select(s)"
        >
          <div class="goal">{{ s.goal }}</div>
          <div class="meta">
            <span class="time">{{ new Date(s.started_at).toLocaleTimeString() }}</span>
            <span :class="'badge status-'+s.status">{{ s.status }}</span>
          </div>
        </div>
      </div>
    </div>
    <div class="detail" v-if="active">
      <div class="panel-header">
        <h3>{{ active.goal }}</h3>
        <span :class="'badge status-'+active.status">{{ active.status }}</span>
      </div>
      <div class="tabs">
        <button v-for="t in ['log','agents','board','trace','metrics']" :key="t"
          :class="{active:tab===t}" @click="tab=t"
        >{{ {log:'执行日志',agents:'智能体',board:'看板',trace:'追踪',metrics:'指标'}[t] }}</button>
      </div>
      <div class="tab-content" v-if="tab==='log'">
        <div v-for="(ev,i) in active.events" :key="i" class="event" :class="eventCls(ev)">
          <div class="hdr">
            <span class="time">{{ new Date(ev.timestamp).toLocaleTimeString() }}</span>
            <span class="agent">{{ ev.agent }}</span>
            <span v-if="ev.kind" :class="'kind kind-'+ev.kind">{{ ev.kind }}</span>
            <button v-if="isDebugKind(ev) && (ev.detail_json || ev.prompt)" class="toggle-btn" @click="toggle('detail-'+i)">{{ expanded.has('detail-'+i) ? 'hide' : 'show' }}</button>
          </div>
          <div class="msg" v-html="esc(ev.message)"></div>
          <pre v-if="ev.tool_output" class="out">{{ ev.tool_output }}</pre>
          <pre v-if="isDebugKind(ev) && expanded.has('detail-'+i)" class="out">{{ ev.detail_json || ev.prompt || '' }}</pre>
        </div>
      </div>
      <div class="tab-content" v-else-if="tab==='agents'">
        <div v-for="(ev,i) in active.events.filter(e=>e.type==='agent_done')" :key="i" class="event">
          {{ ev.message }}
        </div>
      </div>
      <div class="tab-content" v-else-if="tab==='board'"><div class="empty">看板 API 待实现</div></div>
      <div class="tab-content" v-else-if="tab==='trace'">
        <div class="trace-layout">
          <div class="trace-section">
            <div class="trace-section-header">Token 消耗统计</div>
            <div class="trace-section-body">
              <table class="token-table" v-if="tokenStats.length">
                <thead><tr><th>调用者</th><th>次数</th><th>输入Token</th><th>输出Token</th><th>合计</th></tr></thead>
                <tbody>
                  <tr v-for="s in tokenStats" :key="s.caller">
                    <td>{{ s.caller }}</td><td class="num">{{ s.calls }}</td><td class="num">{{ s.input }}</td><td class="num">{{ s.output }}</td><td class="num">{{ s.input + s.output }}</td>
                  </tr>
                </tbody>
              </table>
              <div class="empty" v-else>暂无 Token 消耗记录</div>
            </div>
          </div>
          <div class="trace-section">
            <div class="trace-section-header">Prompt 查看器 ({{ promptEvents.length }})</div>
            <div class="trace-section-body">
              <div v-for="(ev,i) in promptEvents" :key="i" class="prompt-item">
                <div class="prompt-header" @click="toggle('trace-prompt-'+i)">
                  <span>{{ ev.message }}</span>
                  <button class="toggle-btn">{{ expanded.has('trace-prompt-'+i) ? 'hide' : 'show' }}</button>
                </div>
                <div v-if="expanded.has('trace-prompt-'+i)" class="prompt-body">{{ ev.prompt || ev.detail_json || '' }}</div>
              </div>
              <div class="empty" v-if="!promptEvents.length">暂无 Prompt 记录</div>
            </div>
          </div>
          <div class="trace-section">
            <div class="trace-section-header">Agent 创建历史 ({{ agentEvents.length }})</div>
            <div class="trace-section-body">
              <div v-for="(ev,i) in agentEvents" :key="i" class="agent-create-card">
                <div class="agent-create-title">{{ ev.message }}</div>
                <div class="agent-create-meta">{{ ev.detail_json || '' }}</div>
              </div>
              <div class="empty" v-if="!agentEvents.length">暂无 Agent 创建记录</div>
            </div>
          </div>
          <div class="trace-section">
            <div class="trace-section-header">图执行步骤流 ({{ stepEvents.length }})</div>
            <div class="trace-section-body">
              <div class="step-flow">
                <div v-for="(ev,i) in stepEvents" :key="i" class="step-flow-item">
                  <template v-if="parseStep(ev.message)">
                    <span class="step-flow-num">#{{ parseStep(ev.message)!.step }}</span>
                    <span>{{ parseStep(ev.message)!.from }}</span>
                    <span class="step-flow-arrow">→</span>
                    <span>{{ parseStep(ev.message)!.to }}</span>
                    <span class="step-flow-action">{{ parseStep(ev.message)!.action }}</span>
                  </template>
                  <template v-else>
                    <span class="step-flow-num">#{{ i+1 }}</span>
                    <span>{{ ev.message }}</span>
                  </template>
                </div>
              </div>
              <div class="empty" v-if="!stepEvents.length">暂无图执行步骤记录</div>
            </div>
          </div>
        </div>
      </div>
      <div class="tab-content" v-else-if="tab==='metrics'">
        <div v-for="(ev,i) in active.events.filter(e=>e.type==='stats')" :key="i" class="event">{{ ev.message }}</div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 300px 1fr; gap: 16px; height: 100%; }
.list, .detail { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; overflow: hidden; }
.panel-header { padding: 10px 14px; border-bottom: 1px solid #243447; display: flex; justify-content: space-between; align-items: center; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.cards { flex: 1; overflow-y: auto; padding: 8px; }
.card { padding: 10px; border-radius: 4px; margin-bottom: 6px; background: #1a2332; border: 1px solid #243447; cursor: pointer; }
.card:hover { border-color: #3b82f6; }
.card.active { border-color: #3b82f6; background: rgba(59,130,246,0.1); }
.card.running { border-left: 3px solid #3b82f6; }
.card.completed { border-left: 3px solid #22c55e; }
.card.error { border-left: 3px solid #ef4444; }
.goal { font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-bottom: 4px; }
.meta { display: flex; justify-content: space-between; font-size: 11px; }
.time { color: #64748b; }

.tabs { display: flex; gap: 2px; padding: 0 14px; border-bottom: 1px solid #243447; flex-shrink: 0; }
.tabs button { background: none; border: none; border-bottom: 2px solid transparent; padding: 8px 14px; color: #64748b; font-size: 12px; cursor: pointer; font-family: inherit; }
.tabs button.active { color: #3b82f6; border-bottom-color: #3b82f6; }
.tab-content { flex: 1; overflow-y: auto; padding: 8px; }

.event { padding: 6px 10px; border-radius: 4px; margin-bottom: 3px; font-size: 12px; border-left: 3px solid #243447; background: #1a2332; }
.hdr { display: flex; gap: 8px; align-items: center; margin-bottom: 2px; }
.time { color: #64748b; font-size: 11px; }
.agent { color: #3b82f6; font-weight: 600; font-size: 11px; }
.msg { color: #94a3b8; }
.kind { font-size: 9px; padding: 1px 5px; border-radius: 3px; font-weight: 700; text-transform: uppercase; }
.kind-think { background: rgba(234,179,8,0.15); color: #eab308; }
.kind-intend { background: rgba(59,130,246,0.15); color: #3b82f6; }
.kind-llm { background: rgba(168,85,247,0.15); color: #a855f7; }
.kind-tool_call { background: rgba(34,197,94,0.15); color: #22c55e; }
.kind-tool_result { background: rgba(6,182,212,0.15); color: #06b6d4; }
.kind-wait { background: rgba(100,116,139,0.15); color: #64748b; }
.kind-error { background: rgba(239,68,68,0.15); color: #ef4444; }
.kind-prompt { background: rgba(139,92,246,0.15); color: #a78bfa; }
.kind-agent_created { background: rgba(99,102,241,0.15); color: #818cf8; }
.kind-token_usage { background: rgba(100,116,139,0.1); color: #64748b; font-size: 9px; }
.kind-graph_step { background: rgba(59,130,246,0.15); color: #3b82f6; }
.event-kind-think { border-left-color: #eab308; }
.event-kind-intend { border-left-color: #3b82f6; }
.event-kind-llm { border-left-color: #a855f7; }
.event-kind-tool_call { border-left-color: #22c55e; }
.event-kind-tool_result { border-left-color: #06b6d4; }
.event-kind-wait { border-left-color: #64748b; }
.event-kind-error { border-left-color: #ef4444; }
.event-kind-prompt { border-left-color: #a78bfa; }
.event-kind-agent_created { border-left-color: #818cf8; }
.event-kind-token_usage { border-left-color: #243447; }
.event-kind-graph_step { border-left-color: #3b82f6; }
.event-tool_exec { border-left-color: #f97316; background: rgba(249,115,22,0.05); }
.event-system { border-left-color: #3b82f6; }
.event-error { border-left-color: #ef4444; }
.out { margin-top: 4px; padding: 6px; background: #0a0e17; border: 1px solid #243447; border-radius: 4px; font-size: 11px; white-space: pre-wrap; overflow-x: auto; }
.empty { padding: 40px; text-align: center; color: #64748b; }
.badge { font-size: 10px; padding: 1px 8px; border-radius: 10px; font-weight: 700; }
.toggle-btn { background: none; border: none; color: #3b82f6; font-size: 11px; cursor: pointer; margin-left: auto; }

/* Trace panel */
.trace-layout { display: flex; flex-direction: column; gap: 16px; padding: 4px; }
.trace-section { background: #161f2e; border: 1px solid #243447; border-radius: 8px; overflow: hidden; }
.trace-section-header { padding: 10px 14px; background: #1a2332; border-bottom: 1px solid #243447; font-size: 13px; font-weight: 700; color: #e2e8f0; }
.trace-section-body { padding: 12px; max-height: 400px; overflow-y: auto; }
.token-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.token-table th { text-align: left; padding: 8px 10px; background: #1a2332; color: #94a3b8; font-weight: 600; border-bottom: 1px solid #243447; }
.token-table td { padding: 8px 10px; border-bottom: 1px solid #243447; color: #94a3b8; }
.token-table tr:hover td { background: #1e293b; }
.token-table .num { text-align: right; font-variant-numeric: tabular-nums; }
.prompt-item { background: #1a2332; border: 1px solid #243447; border-radius: 4px; margin-bottom: 8px; overflow: hidden; }
.prompt-header { display: flex; align-items: center; justify-content: space-between; padding: 8px 12px; cursor: pointer; font-size: 12px; color: #e2e8f0; }
.prompt-header:hover { background: #1e293b; }
.prompt-body { padding: 10px 12px; background: #0a0e17; font-family: 'JetBrains Mono', monospace; font-size: 11px; color: #94a3b8; white-space: pre-wrap; word-break: break-all; max-height: 300px; overflow-y: auto; }
.step-flow { display: flex; flex-direction: column; gap: 4px; }
.step-flow-item { display: flex; align-items: center; gap: 8px; padding: 6px 10px; background: #1a2332; border-radius: 4px; font-size: 12px; }
.step-flow-num { font-size: 10px; color: #64748b; min-width: 30px; }
.step-flow-arrow { color: #3b82f6; font-weight: 700; }
.step-flow-action { font-size: 10px; padding: 1px 6px; background: rgba(59,130,246,0.15); color: #3b82f6; border-radius: 3px; font-weight: 600; }
.agent-create-card { background: #1a2332; border: 1px solid #243447; border-radius: 4px; padding: 10px 12px; margin-bottom: 8px; font-size: 12px; }
.agent-create-title { font-weight: 600; color: #e2e8f0; margin-bottom: 4px; }
.agent-create-meta { font-size: 11px; color: #64748b; }
</style>
