<script setup lang="ts">
// 效率审计面板（TODO 第9⑥ 效率一等指标 + 第10③ 子 Agent 审计面）。
// 顶部五指标卡 + 支路成本表（点击行下钻逐轮事件回放，可手动暂停 domain 支路）
// + 角色级 token 统计表。数据纯聚合自后端 /efficiency 与 /agents/{aid}/events。
import { ref, watch, computed } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  getSessionEfficiency,
  getAgentEvents,
  pauseSessionAgent,
  type SessionEfficiency,
  type EfficiencyBranch,
  type AgentEventRow,
} from '@/api/metrics'
import type { Session } from '@/types'

const props = defineProps<{
  session: Session | null
  /** 选中 Agent 的 inst_id（node_id 口径一致）：有值时面板只看该支路（隐藏会话级指标与角色统计），并自动展开逐轮下钻 */
  selectedNodeId?: string
}>()

const eff = ref<SessionEfficiency | null>(null)
const loading = ref(false)
const expandedBranch = ref<EfficiencyBranch | null>(null)
const branchEvents = ref<AgentEventRow[]>([])
const eventsLoading = ref(false)

// 选中子 Agent 时支路表只留该支路（"只看当前 Agent 自己的记录"；Meta/未选中为全会话）。
const visibleBranches = computed(() => {
  const all = eff.value?.branches || []
  if (!props.selectedNodeId) return all
  return all.filter((b) => b.node_id === props.selectedNodeId)
})

async function load() {
  if (!props.session) return
  loading.value = true
  try {
    eff.value = await getSessionEfficiency(props.session.id)
    autoExpandSelected()
  } catch (e) {
    ElMessage.error(`加载效率指标失败: ${e instanceof Error ? e.message : e}`)
  } finally {
    loading.value = false
  }
}

watch(() => props.session?.id, () => {
  eff.value = null
  expandedBranch.value = null
  branchEvents.value = []
  load()
}, { immediate: true })

/** 选中 Agent 变化/指标加载后：自动展开对应支路的下钻（已展开同一支路则不动）。 */
function autoExpandSelected() {
  const id = props.selectedNodeId
  if (!id || !eff.value) return
  if (expandedBranch.value?.node_id === id) return
  const row = eff.value.branches?.find((b) => b.node_id === id)
  if (row) void openBranch(row)
}

watch(() => props.selectedNodeId, autoExpandSelected)

// 下钻：展开某支路的逐轮事件回放（tool_call/answer 按时间正序）。
async function openBranch(row: EfficiencyBranch) {
  if (!props.session) return
  if (expandedBranch.value?.node_id === row.node_id) {
    expandedBranch.value = null
    branchEvents.value = []
    return
  }
  expandedBranch.value = row
  eventsLoading.value = true
  try {
    const res = await getAgentEvents(props.session.id, row.node_id)
    branchEvents.value = res.events || []
  } catch (e) {
    ElMessage.error(`加载事件失败: ${e instanceof Error ? e.message : e}`)
  } finally {
    eventsLoading.value = false
  }
}

// 手动止血：暂停 domain 支路（仅 Running 可暂停），成功后刷新指标。
async function pauseBranch(row: EfficiencyBranch) {
  if (!props.session) return
  try {
    await ElMessageBox.confirm(`暂停 domain 支路「${row.domain || row.node_id}」？暂停后可从会话恢复。`, '手动暂停', { type: 'warning' })
  } catch {
    return
  }
  try {
    await pauseSessionAgent(props.session.id, row.node_id)
    ElMessage.success('支路已暂停，可稍后恢复')
    await load()
  } catch (e) {
    ElMessage.error(`暂停失败: ${e instanceof Error ? e.message : e}`)
  }
}

function statusClass(status: string): string {
  switch (status) {
    case 'done':
    case 'idle':
      return 'text-green-600'
    case 'failed':
      return 'text-red-500'
    case 'running':
      return 'text-blue-500'
    case 'paused':
      return 'text-orange-500'
    default:
      return 'text-ink-2'
  }
}

function fmtWall(sec: number): string {
  if (!sec) return '-'
  if (sec < 60) return `${sec.toFixed(1)}s`
  return `${Math.floor(sec / 60)}m${Math.round(sec % 60)}s`
}

function fmtNum(n: number): string {
  return Number.isInteger(n) ? n.toLocaleString() : n.toFixed(1)
}

function fmtPct(n: number): string {
  return `${(n * 100).toFixed(1)}%`
}

function eventTitle(ev: AgentEventRow): string {
  if (ev.type === 'tool_call') return `${ev.tool_name} ${ev.input}`
  return ev.content
}
</script>

<template>
  <div class="p-3 space-y-3 overflow-y-auto h-full">
    <div class="flex items-center justify-between">
      <div class="text-sm font-bold text-ink">
        {{ selectedNodeId ? '效率审计（当前 Agent）' : '效率审计（五项一等指标）' }}
      </div>
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
    </div>

    <!-- 五项指标卡（会话级，仅全会话视图展示） -->
    <div v-if="!selectedNodeId" class="grid grid-cols-5 gap-2">
      <div class="p-3 bg-page rounded border border-line text-center">
        <div class="text-xs text-ink-2">每交付文件 token 成本</div>
        <div class="text-lg font-bold text-ink">{{ fmtNum(eff?.tokens_per_file ?? 0) }}</div>
        <div class="text-xs text-ink-3">交付 {{ eff?.files_delivered ?? 0 }} 个文件</div>
      </div>
      <div class="p-3 bg-page rounded border border-line text-center">
        <div class="text-xs text-ink-2">每派发平均轮次</div>
        <div class="text-lg font-bold text-ink">{{ fmtNum(eff?.avg_rounds_per_dispatch ?? 0) }}</div>
        <div class="text-xs text-ink-3">轮次 = 工具轮 + 终答</div>
      </div>
      <div class="p-3 bg-page rounded border border-line text-center">
        <div class="text-xs text-ink-2">校验开销占比</div>
        <div class="text-lg font-bold text-ink">{{ fmtPct(eff?.verify_token_share ?? 0) }}</div>
        <div class="text-xs text-ink-3">review/verify 类角色 token</div>
      </div>
      <div class="p-3 bg-page rounded border border-line text-center">
        <div class="text-xs text-ink-2">meta:domain token 比</div>
        <div class="text-lg font-bold text-ink">{{ fmtNum(eff?.meta_domain_ratio ?? 0) }}</div>
        <div class="text-xs text-ink-3">编排开销 / 执行开销</div>
      </div>
      <div class="p-3 bg-page rounded border border-line text-center">
        <div class="text-xs text-ink-2">单呼输入 P50 / P95</div>
        <div class="text-lg font-bold text-ink">{{ fmtNum(eff?.input_p50 ?? 0) }} / {{ fmtNum(eff?.input_p95 ?? 0) }}</div>
        <div class="text-xs text-ink-3">总入 {{ (eff?.total_input_tokens ?? 0).toLocaleString() }} / 出 {{ (eff?.total_output_tokens ?? 0).toLocaleString() }}</div>
      </div>
    </div>

    <!-- 支路成本表 -->
    <div>
      <div class="text-xs text-ink-2 mb-1">支路成本表（点击行下钻逐轮事件；Running 的 domain 支路可手动暂停）</div>
      <el-table
        :data="visibleBranches"
        size="small"
        class="!border-line"
        highlight-current-row
        @row-click="openBranch"
      >
        <el-table-column prop="node_id" label="实例" min-width="170" show-overflow-tooltip />
        <el-table-column prop="role" label="角色" width="110" show-overflow-tooltip />
        <el-table-column prop="domain" label="领域" width="100" show-overflow-tooltip />
        <el-table-column prop="task" label="任务" min-width="180" show-overflow-tooltip />
        <el-table-column label="状态" width="90">
          <template #default="{ row }">
            <span :class="statusClass(row.status)">{{ row.status }}</span>
          </template>
        </el-table-column>
        <el-table-column label="墙钟" width="80">
          <template #default="{ row }">{{ fmtWall(row.wall_clock_sec) }}</template>
        </el-table-column>
        <el-table-column prop="tool_calls" label="工具" width="60" />
        <el-table-column prop="rounds" label="轮次" width="60" />
        <el-table-column label="交付文件" min-width="160">
          <template #default="{ row }">
            <span v-if="!row.files_written?.length" class="text-ink-3">-</span>
            <span v-else class="truncate block" :title="row.files_written.join('\n')">
              {{ row.files_written.length }} 个: {{ row.files_written[0] }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="80">
          <template #default="{ row }">
            <el-button
              v-if="row.status === 'running' && row.role === 'domain'"
              size="small"
              type="warning"
              link
              @click.stop="pauseBranch(row)"
            >暂停</el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 逐轮事件回放 -->
    <div v-if="expandedBranch" class="border border-line rounded p-2 bg-page">
      <div class="flex items-center justify-between mb-1">
        <div class="text-xs font-bold text-ink">
          逐轮回放: {{ expandedBranch.node_id }}
          <span class="text-ink-3 font-normal">（{{ expandedBranch.role }} / {{ expandedBranch.domain || '-' }}）</span>
        </div>
        <el-button size="small" link @click="expandedBranch = null; branchEvents = []">收起</el-button>
      </div>
      <div v-if="eventsLoading" class="text-xs text-ink-2 py-2">加载中…</div>
      <div v-else-if="!branchEvents.length" class="text-xs text-ink-3 py-2">暂无事件（pgStore 未接线或该支路无记录）</div>
      <div v-else class="max-h-72 overflow-y-auto space-y-1">
        <div v-for="(ev, i) in branchEvents" :key="i" class="text-xs p-1.5 bg-card rounded border border-line">
          <div class="flex items-center gap-2 mb-0.5">
            <span
              :class="ev.type === 'tool_call' ? 'bg-blue-100 text-blue-700' : 'bg-green-100 text-green-700'"
              class="px-1.5 rounded text-[10px] font-bold"
            >{{ ev.type === 'tool_call' ? ev.tool_name : '终答' }}</span>
            <span class="text-ink-3">{{ new Date(ev.occurred).toLocaleTimeString() }}</span>
          </div>
          <div class="text-ink-2 font-mono whitespace-pre-wrap break-all line-clamp-3" :title="eventTitle(ev)">
            {{ eventTitle(ev).slice(0, 300) }}
          </div>
          <div v-if="ev.output" class="text-ink-3 font-mono whitespace-pre-wrap break-all line-clamp-2 mt-0.5" :title="ev.output">
            → {{ ev.output.slice(0, 200) }}
          </div>
        </div>
      </div>
    </div>

    <!-- 角色级 token 统计（会话级，仅全会话视图展示） -->
    <div v-if="!selectedNodeId">
      <div class="text-xs text-ink-2 mb-1">角色级 token / 延迟统计（含单呼输入分位数）</div>
      <el-table :data="eff?.role_stats || []" size="small" class="!border-line">
        <el-table-column prop="role" label="角色" min-width="140" show-overflow-tooltip />
        <el-table-column prop="calls" label="调用" width="70" />
        <el-table-column label="输入" width="100">
          <template #default="{ row }">{{ row.input_tokens.toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="输出" width="100">
          <template #default="{ row }">{{ row.output_tokens.toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="均延迟" width="90">
          <template #default="{ row }">{{ row.avg_latency_ms >= 1000 ? `${(row.avg_latency_ms / 1000).toFixed(1)}s` : `${row.avg_latency_ms}ms` }}</template>
        </el-table-column>
        <el-table-column label="入 P50" width="90">
          <template #default="{ row }">{{ fmtNum(row.p50_input_tokens) }}</template>
        </el-table-column>
        <el-table-column label="入 P95" width="90">
          <template #default="{ row }">{{ fmtNum(row.p95_input_tokens) }}</template>
        </el-table-column>
      </el-table>
    </div>
  </div>
</template>
