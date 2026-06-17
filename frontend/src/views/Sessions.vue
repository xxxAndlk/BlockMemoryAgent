<script setup lang="ts">
import { ref, onMounted } from 'vue'
import type { Session, SessionEvent } from '../types'

const sessions = ref<Session[]>([])
const active = ref<Session | null>(null)
const tab = ref('log')
const evtSource = ref<EventSource | null>(null)

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
        <button v-for="t in ['log','agents','board','metrics']" :key="t"
          :class="{active:tab===t}" @click="tab=t"
        >{{ {log:'执行日志',agents:'智能体',board:'看板',metrics:'指标'}[t] }}</button>
      </div>
      <div class="tab-content" v-if="tab==='log'">
        <div v-for="(ev,i) in active.events" :key="i" class="event" :class="eventCls(ev)">
          <div class="hdr">
            <span class="time">{{ new Date(ev.timestamp).toLocaleTimeString() }}</span>
            <span class="agent">{{ ev.agent }}</span>
            <span v-if="ev.kind" :class="'kind kind-'+ev.kind">{{ ev.kind }}</span>
          </div>
          <div class="msg" v-html="esc(ev.message)"></div>
          <pre v-if="ev.tool_output" class="out">{{ ev.tool_output }}</pre>
        </div>
      </div>
      <div class="tab-content" v-else-if="tab==='agents'">
        <div v-for="(ev,i) in active.events.filter(e=>e.type==='agent_done')" :key="i" class="event">
          {{ ev.message }}
        </div>
      </div>
      <div class="tab-content" v-else-if="tab==='board'"><div class="empty">看板 API 待实现</div></div>
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
.event-tool_exec { border-left-color: #f97316; }
.event-system { border-left-color: #3b82f6; }
.event-error { border-left-color: #ef4444; }
.out { margin-top: 4px; padding: 6px; background: #0a0e17; border: 1px solid #243447; border-radius: 4px; font-size: 11px; white-space: pre-wrap; overflow-x: auto; }
.empty { padding: 40px; text-align: center; color: #64748b; }
.badge { font-size: 10px; padding: 1px 8px; border-radius: 10px; font-weight: 700; }
</style>
