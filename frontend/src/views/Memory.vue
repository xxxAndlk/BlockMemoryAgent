<script setup lang="ts">
import { ref, computed } from 'vue'
import Panel from '../components/ui/Panel.vue'
import StatCard from '../components/ui/StatCard.vue'
import StatusBadge from '../components/ui/StatusBadge.vue'
import Sparkline from '../components/ui/Sparkline.vue'
import BarChart from '../components/ui/BarChart.vue'

const agents = ref(['MetaAgent', 'OpsAgent', 'DevAgent', 'ReviewAgent'])
const selectedAgent = ref('MetaAgent')
const tab = ref('snapshot')
const query = ref('')
const levelFilter = ref('all')

const stats = ref({
  total: 1248,
  topics: 36,
  entities: 142,
  compressionRatio: 68,
})

const timeline = ref([12, 18, 25, 22, 34, 45, 38, 52, 61, 58, 72, 80])
const levels = ref([
  { label: 'Raw', value: 320, color: '#64748b' },
  { label: 'Standard', value: 540, color: '#3b82f6' },
  { label: 'Compact', value: 280, color: '#22c55e' },
  { label: 'Marker', value: 108, color: '#a855f7' },
])

const memories = ref([
  { id: 'm1', title: 'Redis 连接池告警根因', agent: 'OpsAgent', level: 'Standard', topic: 'ops/redis', score: 0.92, timestamp: '2026-06-18 14:32' },
  { id: 'm2', title: 'v3 架构四层级说明', agent: 'MetaAgent', level: 'Compact', topic: 'architecture', score: 0.88, timestamp: '2026-06-18 13:10' },
  { id: 'm3', title: 'Skill pipeline 执行步骤', agent: 'DevAgent', level: 'Raw', topic: 'skill/runtime', score: 0.85, timestamp: '2026-06-18 12:45' },
  { id: 'm4', title: 'Postgres pgvector 索引配置', agent: 'OpsAgent', level: 'Marker', topic: 'infra/db', score: 0.78, timestamp: '2026-06-18 11:20' },
  { id: 'm5', title: 'LLM 连续失败处理机制', agent: 'MetaAgent', level: 'Standard', topic: 'graph/llm', score: 0.95, timestamp: '2026-06-18 10:05' },
])

const topics = ref([
  { name: 'ops/redis', count: 42 },
  { name: 'architecture', count: 28 },
  { name: 'skill/runtime', count: 19 },
  { name: 'infra/db', count: 15 },
  { name: 'graph/llm', count: 12 },
])

const entities = ref([
  { name: 'Redis', type: 'service', relations: 12 },
  { name: 'MetaAgent', type: 'agent', relations: 8 },
  { name: 'Postgres', type: 'service', relations: 7 },
  { name: 'SkillSet', type: 'concept', relations: 5 },
  { name: 'ThreeLayerGraph', type: 'component', relations: 4 },
])

const recentAccess = ref([
  { time: '2m', action: '搜索', target: 'Redis 告警' },
  { time: '5m', action: '写入', target: 'v3 架构说明' },
  { time: '12m', action: '压缩', target: 'Raw → Standard' },
  { time: '30m', action: '检索', target: 'Skill pipeline' },
])

const filteredMemories = computed(() => {
  return memories.value.filter(m => {
    const q = query.value.trim().toLowerCase()
    const matchQ = !q || m.title.toLowerCase().includes(q) || m.topic.toLowerCase().includes(q)
    const matchLevel = levelFilter.value === 'all' || m.level === levelFilter.value
    return matchQ && matchLevel
  })
})

const compressionTotal = computed(() => levels.value.reduce((a, b) => a + b.value, 0))

function levelBadge(l: string) {
  const m: Record<string, string> = { Raw: 'idle', Standard: 'running', Compact: 'success', Marker: 'pending' }
  return m[l] || 'idle'
}
function entityTypeColor(t: string) {
  const m: Record<string, string> = { service: '#3b82f6', agent: '#22c55e', concept: '#a855f7', component: '#f97316' }
  return m[t] || '#64748b'
}
</script>

<template>
  <div class="memory">
    <!-- header -->
    <div class="memory-header">
      <div class="agent-select">
        <label>智能体</label>
        <select v-model="selectedAgent">
          <option v-for="a in agents" :key="a" :value="a">{{ a }}</option>
        </select>
      </div>
      <div class="search-box">
        <input v-model="query" placeholder="搜索记忆 (标题 / Topic / 实体)..." />
        <button class="btn">搜索</button>
      </div>
      <div class="tabs">
        <button v-for="t in ['snapshot','search','compression','timeline','graph']" :key="t" :class="{active: tab===t}" @click="tab=t">
          {{ {snapshot:'快照', search:'搜索', compression:'压缩', timeline:'时间线', graph:'图谱'}[t] }}
        </button>
      </div>
    </div>

    <!-- stats -->
    <div class="stats-row">
      <StatCard label="记忆总数" :value="stats.total.toLocaleString()" change="+5%" :up="true" />
      <StatCard label="主题数" :value="stats.topics" />
      <StatCard label="实体数" :value="stats.entities" />
      <StatCard label="压缩率" :value="`${stats.compressionRatio}%`" change="-2%" :up="false" />
    </div>

    <!-- snapshot -->
    <div v-if="tab==='snapshot'" class="tab-grid">
      <Panel title="关键摘要" class="wide">
        <ul class="summary-list">
          <li>当前系统基于 CloudWeGo Eino 构建四层智能体架构。</li>
          <li>Ops 领域近期高频话题：Redis 连接池、告警风暴、自动回滚。</li>
          <li>v3 版本重点补充了 Watchdog、Skill 库、任务看板、Mailbox 能力。</li>
        </ul>
      </Panel>

      <Panel title="未解决问题">
        <div class="issue">
          <StatusBadge status="warning" small />
          <span>LLM 工具执行连续失败阈值待调优</span>
        </div>
        <div class="issue">
          <StatusBadge status="pending" small />
          <span>Memory 跨 Topic 实体对齐不完整</span>
        </div>
      </Panel>

      <Panel title="高频主题">
        <BarChart :data="topics.map(t => ({ label: t.name, value: t.count, color: '#3b82f6' }))" height="120" />
      </Panel>

      <Panel title="最近访问">
        <div v-for="(r, i) in recentAccess" :key="i" class="access-row">
          <span class="time">{{ r.time }}</span>
          <StatusBadge :status="r.action === '写入' ? 'success' : r.action === '压缩' ? 'warning' : 'idle'" :text="r.action" small />
          <span class="target">{{ r.target }}</span>
        </div>
      </Panel>
    </div>

    <!-- search -->
    <div v-else-if="tab==='search'" class="tab-grid">
      <Panel title="搜索结果" class="wide">
        <div class="filters">
          <span class="filter-label">层级:</span>
          <button v-for="l in ['all','Raw','Standard','Compact','Marker']" :key="l" :class="{active: levelFilter===l}" @click="levelFilter=l">{{ l === 'all' ? '全部' : l }}</button>
        </div>
        <table class="mem-table">
          <thead>
            <tr>
              <th>标题</th>
              <th>Agent</th>
              <th>Topic</th>
              <th>层级</th>
              <th>相关度</th>
              <th>时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="m in filteredMemories" :key="m.id">
              <td class="title-cell">{{ m.title }}</td>
              <td>{{ m.agent }}</td>
              <td>{{ m.topic }}</td>
              <td><StatusBadge :status="levelBadge(m.level)" :text="m.level" small /></td>
              <td>
                <div class="score-bar">
                  <div class="score-fill" :style="{width: `${m.score * 100}%`}"></div>
                  <span>{{ (m.score * 100).toFixed(0) }}%</span>
                </div>
              </td>
              <td class="muted">{{ m.timestamp }}</td>
            </tr>
          </tbody>
        </table>
      </Panel>
    </div>

    <!-- compression -->
    <div v-else-if="tab==='compression'" class="tab-grid">
      <Panel title="压缩层级分布" class="wide">
        <div class="level-grid">
          <div v-for="l in levels" :key="l.label" class="level-card">
            <div class="level-name">{{ l.label }}</div>
            <div class="level-count" :style="{color: l.color}">{{ l.value }}</div>
            <div class="level-ratio">{{ ((l.value / compressionTotal) * 100).toFixed(1) }}%</div>
          </div>
        </div>
        <BarChart :data="levels" height="120" />
      </Panel>
    </div>

    <!-- timeline -->
    <div v-else-if="tab==='timeline'" class="tab-grid">
      <Panel title="记忆写入趋势" class="wide">
        <Sparkline :data="timeline" color="#3b82f6" fill height="120" />
      </Panel>
      <Panel title="最近写入">
        <div v-for="m in memories.slice(0, 4)" :key="m.id" class="access-row">
          <span class="time">{{ m.timestamp.split(' ')[1] }}</span>
          <StatusBadge :status="levelBadge(m.level)" :text="m.level" small />
          <span class="target">{{ m.title }}</span>
        </div>
      </Panel>
    </div>

    <!-- graph -->
    <div v-else-if="tab==='graph'" class="tab-grid">
      <Panel title="实体关系" class="wide">
        <div class="entity-grid">
          <div v-for="e in entities" :key="e.name" class="entity-card">
            <div class="entity-dot" :style="{background: entityTypeColor(e.type)}"></div>
            <div class="entity-info">
              <div class="entity-name">{{ e.name }}</div>
              <div class="entity-meta">{{ e.type }} · {{ e.relations }} relations</div>
            </div>
          </div>
        </div>
      </Panel>
    </div>
  </div>
</template>

<style scoped>
.memory {
  display: flex;
  flex-direction: column;
  gap: 12px;
  padding: 12px;
  height: 100%;
  overflow-y: auto;
}
.memory-header {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  padding: 10px 14px;
}
.agent-select { display: flex; align-items: center; gap: 8px; }
.agent-select label { font-size: 11px; color: var(--text-muted, #64748b); }
.agent-select select {
  background: var(--bg-tertiary, #1a2332); border: 1px solid var(--border, #243447); border-radius: 6px;
  padding: 6px 10px; color: var(--text-primary, #e2e8f0); font-size: 12px;
}
.search-box { flex: 1; display: flex; gap: 8px; min-width: 240px; }
.search-box input {
  flex: 1; background: var(--bg-tertiary, #1a2332); border: 1px solid var(--border, #243447); border-radius: 6px;
  padding: 6px 12px; color: var(--text-primary, #e2e8f0); font-size: 12px;
}
.btn {
  background: var(--accent-blue, #3b82f6); color: white; border: none; border-radius: 6px;
  padding: 6px 16px; font-size: 12px; cursor: pointer;
}
.tabs { display: flex; gap: 4px; }
.tabs button {
  background: transparent; border: none; border-bottom: 2px solid transparent;
  padding: 8px 12px; color: var(--text-muted, #64748b); font-size: 12px; cursor: pointer;
}
.tabs button.active { color: var(--accent-blue, #3b82f6); border-bottom-color: var(--accent-blue, #3b82f6); }

.stats-row { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; }

.tab-grid { display: grid; grid-template-columns: repeat(2, 1fr); gap: 12px; }
.tab-grid .wide { grid-column: 1 / -1; }

.summary-list { padding-left: 18px; color: var(--text-secondary, #94a3b8); font-size: 12px; line-height: 1.8; }
.issue { display: flex; align-items: center; gap: 8px; padding: 8px 0; border-bottom: 1px solid var(--border, #243447); font-size: 12px; color: var(--text-primary, #e2e8f0); }
.issue:last-child { border-bottom: none; }

.access-row { display: flex; align-items: center; gap: 10px; padding: 8px 0; border-bottom: 1px solid var(--border, #243447); font-size: 12px; }
.access-row:last-child { border-bottom: none; }
.access-row .time { width: 36px; color: var(--text-muted, #64748b); }
.access-row .target { color: var(--text-primary, #e2e8f0); }

.filters { display: flex; gap: 8px; align-items: center; margin-bottom: 10px; }
.filter-label { font-size: 11px; color: var(--text-muted, #64748b); }
.filters button {
  background: transparent; border: 1px solid var(--border, #243447); border-radius: 12px;
  padding: 2px 10px; font-size: 11px; color: var(--text-secondary, #94a3b8); cursor: pointer;
}
.filters button.active { background: var(--accent-blue, #3b82f6); color: white; border-color: var(--accent-blue, #3b82f6); }

.mem-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.mem-table th { text-align: left; padding: 8px; color: var(--text-muted, #64748b); border-bottom: 1px solid var(--border, #243447); }
.mem-table td { padding: 8px; border-bottom: 1px solid var(--border, #243447); color: var(--text-secondary, #94a3b8); vertical-align: middle; }
.mem-table .title-cell { color: var(--text-primary, #e2e8f0); }
.mem-table .muted { color: var(--text-muted, #64748b); }
.score-bar { position: relative; height: 6px; background: var(--bg-tertiary, #1a2332); border-radius: 3px; width: 80px; }
.score-fill { height: 100%; background: var(--accent-blue, #3b82f6); border-radius: 3px; }
.score-bar span { position: absolute; left: calc(100% + 6px); top: -5px; font-size: 10px; color: var(--text-muted, #64748b); }

.level-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin-bottom: 16px; }
.level-card { background: var(--bg-tertiary, #1a2332); border: 1px solid var(--border, #243447); border-radius: 8px; padding: 14px; text-align: center; }
.level-name { font-size: 11px; color: var(--text-muted, #64748b); margin-bottom: 6px; }
.level-count { font-size: 24px; font-weight: 700; }
.level-ratio { font-size: 11px; color: var(--text-secondary, #94a3b8); }

.entity-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 12px; }
.entity-card { display: flex; align-items: center; gap: 10px; background: var(--bg-tertiary, #1a2332); border: 1px solid var(--border, #243447); border-radius: 8px; padding: 12px; }
.entity-dot { width: 12px; height: 12px; border-radius: 50%; }
.entity-name { font-size: 12px; font-weight: 600; color: var(--text-primary, #e2e8f0); }
.entity-meta { font-size: 10px; color: var(--text-muted, #64748b); }

@media (max-width: 1100px) {
  .stats-row { grid-template-columns: repeat(2, 1fr); }
  .tab-grid { grid-template-columns: 1fr; }
  .level-grid { grid-template-columns: repeat(2, 1fr); }
  .entity-grid { grid-template-columns: repeat(2, 1fr); }
}
@media (max-width: 700px) {
  .memory-header { flex-direction: column; align-items: stretch; }
  .tabs { width: 100%; overflow-x: auto; }
}
</style>
