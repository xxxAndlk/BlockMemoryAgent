<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import type { Session } from '../types'
import Panel from '../components/ui/Panel.vue'
import StatusBadge from '../components/ui/StatusBadge.vue'
import Sparkline from '../components/ui/Sparkline.vue'
import TimelineItem from '../components/ui/TimelineItem.vue'

const router = useRouter()
const sessions = ref<Session[]>([])
const loading = ref(true)
const goal = ref('')
const processFilter = ref('all')

const program = ref('MetaAgent v3')
const mode = ref('Auto')
const llmApi = ref('Online')
const health = ref('Healthy')

const trend = ref([820, 760, 880, 920, 890, 980, 1050, 1120, 1080, 1200, 1180, 1250])

const recentActivity = ref([
  { time: '2m', title: '会话 #9a7f 完成', subtitle: 'Root cause: Redis 连接池耗尽', status: 'success' },
  { time: '12m', title: 'MetaAgent 拆分子任务', subtitle: 'Ops / 监控 / Rollback 三个 Domain', status: 'running' },
  { time: '34m', title: 'Memory 压缩触发', subtitle: 'Raw → Standard: 12 条', status: 'idle' },
  { time: '1h', title: 'Watchdog 上下文告警', subtitle: 'Token 使用接近阈值', status: 'warning' },
])

const quickTags = ['系统架构设计', '代码审查', '接口测试', '混沌演练', '根因分析', '生成周报']

onMounted(async () => {
  try {
    const r = await fetch('/api/sessions')
    sessions.value = (await r.json()) as Session[]
  } catch {
    sessions.value = mockSessions()
  } finally {
    loading.value = false
  }
})

const allProcesses = computed(() => {
  const list = sessions.value.length ? sessions.value : mockSessions()
  const mapped = list.slice().sort((a, b) => +new Date(b.started_at) - +new Date(a.started_at)).map((s, i) => ({
    ...s,
    progress: s.status === 'completed' ? 100 : s.status === 'error' ? 0 : 30 + ((i * 17) % 50),
    category: ['Ops', 'Dev', 'Review', 'Doc'][i % 4],
  }))
  if (processFilter.value === 'all') return mapped
  const f = processFilter.value
  return mapped.filter(p => {
    if (f === 'running') return p.status === 'running'
    if (f === 'completed') return p.status === 'completed'
    if (f === 'paused') return false
    if (f === 'attention') return p.status === 'error'
    return true
  })
})

const summary = computed(() => {
  const total = allProcesses.value.length
  const completed = allProcesses.value.filter(s => s.status === 'completed').length
  const rate = total > 0 ? Math.round((completed / total) * 100) : 0
  let tokens = 0
  allProcesses.value.forEach(s => {
    (s.events || []).forEach(ev => {
      if (ev.kind === 'token_usage') {
        tokens += (ev.input_tokens || 0) + (ev.output_tokens || 0)
      }
    })
  })
  return {
    tasks: 28,
    rate: 92,
    tokens: 1203,
    boost: 2.4,
  }
})

function mockSessions(): Session[] {
  return [
    { id: 's-1', goal: '生成项目架构设计文档', status: 'running', started_at: new Date(Date.now() - 3600000).toISOString(), events: [], messages: [] },
    { id: 's-2', goal: '排查 Redis 连接池告警', status: 'completed', started_at: new Date(Date.now() - 1800000).toISOString(), events: [], messages: [] },
    { id: 's-3', goal: '编写 v3 接口测试用例', status: 'completed', started_at: new Date(Date.now() - 7200000).toISOString(), events: [], messages: [] },
    { id: 's-4', goal: '重构 Skill 注册逻辑', status: 'error', started_at: new Date(Date.now() - 10800000).toISOString(), events: [], messages: [] },
    { id: 's-5', goal: '演练 Chaos 自动回滚', status: 'completed', started_at: new Date(Date.now() - 14400000).toISOString(), events: [], messages: [] },
    { id: 's-6', goal: '分析日志异常模式', status: 'running', started_at: new Date(Date.now() - 15000000).toISOString(), events: [], messages: [] },
  ]
}

function createSession() {
  if (!goal.value.trim()) return
  fetch('/api/sessions', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ goal: goal.value.trim() }),
  }).then(() => router.push('/chat'))
}

function quick(g: string) {
  goal.value = g
  createSession()
}

function fmtDate(iso: string) {
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function statusColor(s: string) {
  return s === 'completed' ? '#22c55e' : s === 'running' ? '#3b82f6' : s === 'error' ? '#ef4444' : '#64748b'
}
function statusText(s: string) {
  const m: Record<string, string> = { completed: '已完成', running: '运行中', error: '失败', idle: '待处理' }
  return m[s] || s
}
</script>

<template>
  <div class="dashboard">
    <!-- top status row -->
    <div class="topbar">
      <div class="status-chips">
        <div class="chip"><span class="chip-label">Program</span><span class="chip-value">{{ program }}</span></div>
        <div class="chip"><span class="chip-label">Mode</span><span class="chip-value">{{ mode }}</span></div>
        <div class="chip"><span class="chip-label">LLM API</span><span class="chip-value online">{{ llmApi }}</span></div>
        <div class="chip"><span class="chip-label">Health</span><span class="chip-value healthy">{{ health }}</span></div>
      </div>
      <button class="run-btn" :disabled="!goal.trim() || loading" @click="createSession">Run Session</button>
    </div>

    <!-- hero -->
    <Panel class="hero" flush>
      <div class="hero-inner">
        <div class="hero-left">
          <div class="hero-title">创建新会话</div>
          <div class="hero-sub">输入目标并启动 MetaAgent 多智能体协作</div>
          <input
            v-model="goal"
            class="hero-input"
            placeholder="输入任务目标，例如：分析当前项目架构并给出优化建议..."
            @keydown.enter="createSession"
          />
          <div class="quick-tags">
            <span v-for="q in quickTags" :key="q" class="tag" @click="quick(q)">{{ q }}</span>
          </div>
        </div>
        <div class="hero-right">
          <svg class="cube" viewBox="0 0 140 140" fill="none">
            <defs>
              <linearGradient id="g1" x1="0" y1="0" x2="1" y2="1">
                <stop offset="0%" stop-color="#3b82f6" />
                <stop offset="100%" stop-color="#1d4ed8" />
              </linearGradient>
              <linearGradient id="g2" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stop-color="#60a5fa" />
                <stop offset="100%" stop-color="#2563eb" />
              </linearGradient>
              <linearGradient id="g3" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stop-color="#93c5fd" />
                <stop offset="100%" stop-color="#3b82f6" />
              </linearGradient>
            </defs>
            <g opacity="0.95">
              <path d="M70 10L130 44V98L70 132L10 98V44L70 10Z" fill="url(#g1)" />
              <path d="M70 10L130 44L70 78L10 44L70 10Z" fill="url(#g2)" />
              <path d="M70 78V132L130 98V44L70 78Z" fill="url(#g3)" />
              <path d="M70 78L10 44V98L70 132V78Z" fill="#1e40af" fill-opacity="0.6" />
              <circle cx="70" cy="78" r="8" fill="#bfdbfe" />
              <circle cx="70" cy="78" r="3" fill="#1e3a8a" />
            </g>
          </svg>
        </div>
      </div>
    </Panel>

    <!-- main -->
    <div class="main-grid">
      <!-- processes -->
      <Panel title="全部进程" :subtitle="`共 ${allProcesses.length} 个`" class="processes">
        <div class="process-tabs">
          <button v-for="f in ['all','running','completed','paused','attention']" :key="f" :class="{active: processFilter===f}" @click="processFilter=f">
            {{ {all:'全部', running:'运行中', completed:'已完成', paused:'已暂停', attention:'需要关注'}[f] }}
          </button>
        </div>
        <div class="process-list">
          <div v-for="p in allProcesses" :key="p.id" class="process">
            <div class="process-main">
              <StatusBadge :status="p.status" :text="statusText(p.status)" small />
              <div class="process-info">
                <div class="process-title">{{ p.goal }}</div>
                <div class="process-meta">
                  <span class="cat">{{ p.category }}</span>
                  <span class="time">{{ fmtDate(p.started_at) }}</span>
                </div>
              </div>
            </div>
            <div class="progress">
              <div class="progress-track">
                <div class="progress-fill" :style="{ width: p.progress + '%', background: statusColor(p.status) }"></div>
              </div>
              <span class="progress-val">{{ p.progress }}%</span>
            </div>
          </div>
        </div>
      </Panel>

      <!-- right -->
      <div class="right-col">
        <Panel title="提升幅度 (今日)">
          <div class="summary-grid">
            <div class="summary-item">
              <div class="summary-val">{{ summary.tasks }}</div>
              <div class="summary-lab">任务数</div>
            </div>
            <div class="summary-item">
              <div class="summary-val">{{ summary.rate }}%</div>
              <div class="summary-lab">完成率</div>
            </div>
            <div class="summary-item">
              <div class="summary-val">{{ summary.tokens.toLocaleString() }}</div>
              <div class="summary-lab">Token</div>
            </div>
            <div class="summary-item">
              <div class="summary-val">{{ summary.boost }}%</div>
              <div class="summary-lab">提升</div>
            </div>
          </div>
          <div class="chart-wrap">
            <Sparkline :data="trend" color="#3b82f6" fill height="80" />
          </div>
        </Panel>

        <Panel title="最近活动">
          <TimelineItem
            v-for="(a, i) in recentActivity"
            :key="i"
            :time="a.time"
            :title="a.title"
            :subtitle="a.subtitle"
            :color="a.status === 'success' ? '#22c55e' : a.status === 'running' ? '#3b82f6' : a.status === 'warning' ? '#eab308' : '#64748b'"
          />
        </Panel>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dashboard {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 16px;
  height: 100%;
  overflow-y: auto;
}
.topbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.status-chips { display: flex; gap: 10px; flex-wrap: wrap; }
.chip {
  display: flex;
  align-items: center;
  gap: 8px;
  background: var(--bg-card, #161f2e);
  border: 1px solid var(--border, #243447);
  border-radius: 20px;
  padding: 5px 12px;
  font-size: 12px;
}
.chip-label { color: var(--text-muted, #64748b); }
.chip-value { color: var(--text-primary, #e2e8f0); font-weight: 600; }
.chip-value.online, .chip-value.healthy { color: #22c55e; }
.run-btn {
  background: #f59e0b;
  color: #111827;
  border: none;
  border-radius: 8px;
  padding: 8px 22px;
  font-size: 13px;
  font-weight: 700;
  cursor: pointer;
}
.run-btn:hover { background: #fbbf24; }
.run-btn:disabled { opacity: 0.5; cursor: not-allowed; }

.hero { padding: 0; overflow: hidden; background: linear-gradient(135deg, rgba(37,99,235,0.12) 0%, rgba(37,99,235,0) 55%); }
.hero-inner {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 24px;
  padding: 28px 30px;
}
.hero-left { flex: 1; min-width: 0; }
.hero-title { font-size: 20px; font-weight: 700; color: var(--text-primary, #e2e8f0); margin-bottom: 6px; }
.hero-sub { font-size: 12px; color: var(--text-muted, #64748b); margin-bottom: 16px; }
.hero-input {
  width: 100%;
  background: var(--bg-tertiary, #1a2332);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
  padding: 11px 14px;
  color: var(--text-primary, #e2e8f0);
  font-size: 13px;
  outline: none;
  margin-bottom: 12px;
}
.hero-input::placeholder { color: var(--text-muted, #64748b); }
.hero-input:focus { border-color: var(--accent-blue, #3b82f6); }
.quick-tags { display: flex; gap: 8px; flex-wrap: wrap; }
.quick-tags .tag {
  background: rgba(59, 130, 246, 0.12);
  border: 1px solid rgba(59, 130, 246, 0.25);
  border-radius: 12px;
  padding: 4px 10px;
  font-size: 11px;
  color: #93c5fd;
  cursor: pointer;
}
.quick-tags .tag:hover { background: rgba(59, 130, 246, 0.22); color: #bfdbfe; }
.hero-right { flex-shrink: 0; }
.cube { width: 120px; height: 120px; filter: drop-shadow(0 10px 30px rgba(59,130,246,0.3)); }

.main-grid {
  display: grid;
  grid-template-columns: 1.5fr 1fr;
  gap: 14px;
  flex: 1;
  min-height: 0;
}
.processes { min-height: 0; }
.process-tabs {
  display: flex;
  gap: 4px;
  margin-bottom: 12px;
  border-bottom: 1px solid var(--border, #243447);
  padding-bottom: 10px;
}
.process-tabs button {
  background: transparent;
  border: none;
  border-radius: 12px;
  padding: 4px 12px;
  font-size: 11px;
  color: var(--text-muted, #64748b);
  cursor: pointer;
}
.process-tabs button.active { background: rgba(59,130,246,0.12); color: #93c5fd; }
.process-list { display: flex; flex-direction: column; gap: 14px; }
.process {
  padding: 14px;
  background: var(--bg-tertiary, #1a2332);
  border: 1px solid var(--border, #243447);
  border-radius: 8px;
}
.process-main { display: flex; gap: 12px; margin-bottom: 12px; }
.process-info { flex: 1; min-width: 0; }
.process-title { font-size: 13px; font-weight: 600; color: var(--text-primary, #e2e8f0); margin-bottom: 5px; }
.process-meta { display: flex; gap: 10px; align-items: center; font-size: 11px; }
.process-meta .cat {
  background: rgba(59,130,246,0.1); color: #93c5fd;
  border-radius: 10px; padding: 1px 8px;
}
.process-meta .time { color: var(--text-muted, #64748b); }
.progress { display: flex; align-items: center; gap: 10px; }
.progress-track { flex: 1; height: 6px; background: rgba(255,255,255,0.06); border-radius: 3px; overflow: hidden; }
.progress-fill { height: 100%; border-radius: 3px; transition: width 0.3s; }
.progress-val { font-size: 11px; color: var(--text-muted, #64748b); min-width: 34px; text-align: right; }

.right-col { display: flex; flex-direction: column; gap: 14px; }
.summary-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 10px;
  margin-bottom: 14px;
}
.summary-item { text-align: center; }
.summary-val { font-size: 22px; font-weight: 700; color: var(--text-primary, #e2e8f0); }
.summary-lab { font-size: 10px; color: var(--text-muted, #64748b); margin-top: 2px; }
.chart-wrap { background: var(--bg-tertiary, #1a2332); border-radius: 8px; padding: 10px; }

@media (max-width: 1100px) {
  .main-grid { grid-template-columns: 1fr; }
  .hero-inner { flex-direction: column; align-items: flex-start; }
  .cube { width: 90px; height: 90px; }
}
@media (max-width: 700px) {
  .summary-grid { grid-template-columns: repeat(2, 1fr); }
  .topbar { flex-direction: column; align-items: stretch; }
  .status-chips { width: 100%; overflow-x: auto; }
}
</style>
