<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: Snapshot -->
    <div class="w-80 flex flex-col gap-4 overflow-y-auto">
      <div class="flex items-center gap-2">
        <span class="text-sm text-gray-400 whitespace-nowrap">Agent 选择</span>
        <el-select v-model="selectedAgent" size="small" class="w-full">
          <el-option v-for="opt in agentOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
        </el-select>
      </div>

      <el-card class="!border-dark-border !bg-dark-panel flex-1">
        <template #header>
          <div class="font-bold text-sm">记忆快照 (Snapshot)</div>
        </template>
        
        <div class="space-y-4">
          <!-- Key Summaries -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Key Summaries (关键摘要)</div>
            <ul v-if="snapshot?.key_summaries?.length" class="list-disc pl-4 text-xs text-gray-300 space-y-1">
              <li v-for="s in snapshot.key_summaries" :key="s.step_id">{{ s.content }}</li>
            </ul>
            <div v-else class="text-xs text-gray-500">暂无关键摘要</div>
          </div>

          <!-- Open Issues -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Open Issues (未解决问题)</div>
            <ul v-if="snapshot?.open_issues?.length" class="list-disc pl-4 text-xs text-gray-300 space-y-1">
              <li v-for="issue in snapshot.open_issues" :key="issue.id">{{ issue.description }}</li>
            </ul>
            <div v-else class="text-xs text-gray-500">暂无未解决问题</div>
          </div>

          <!-- Local Variables -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Local Variables (本地变量)</div>
            <el-table v-if="snapshot?.local_vars && Object.keys(snapshot.local_vars).length" :data="Object.entries(snapshot.local_vars).map(([key, value])=>({key, value}))" size="small" :show-header="true" class="!bg-transparent">
              <el-table-column prop="key" label="Key" width="100" />
              <el-table-column prop="value" label="Value" />
            </el-table>
            <div v-else class="text-xs text-gray-500">暂无本地变量</div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Middle Column: Memory Search -->
    <div class="flex-1 flex flex-col gap-4 overflow-hidden">
      <div class="flex items-center gap-2">
        <span class="font-bold text-sm whitespace-nowrap">记忆检索 (Memory Search)</span>
        <el-input v-model="searchQuery" size="small" placeholder="golang ddd architecture" class="flex-1" @keydown.enter="doSearch" />
        <el-button type="primary" size="small" class="!bg-primary" :loading="loading" @click="doSearch">检索</el-button>
      </div>

      <el-card class="!border-dark-border !bg-dark-panel flex-1 overflow-y-auto">
        <div v-if="loading" class="text-xs text-gray-500 text-center py-4">加载中...</div>
        <div v-else class="space-y-4">
          <div v-for="episode in episodes" :key="episode.step_id" class="p-3 bg-dark-bg rounded border border-dark-border hover:border-primary transition-colors cursor-pointer">
            <div class="flex justify-between items-center mb-2">
              <span class="font-bold text-sm text-primary">{{ episode.step_id }}</span>
              <div class="flex gap-4 text-xs text-gray-500">
                <span>相关度: {{ episode.score.toFixed(2) }}</span>
                <span>{{ fmtTime(episode.time) }}</span>
              </div>
            </div>
            <div class="text-xs text-gray-300">{{ episode.summary }}</div>
          </div>
          <div v-if="!episodes.length" class="text-xs text-gray-500 text-center py-4">无检索结果</div>
        </div>
      </el-card>
    </div>

    <!-- Right Column: Memory Compression -->
    <div class="w-72 flex flex-col gap-4">
      <el-card class="!border-dark-border !bg-dark-panel h-full">
        <template #header>
          <div class="font-bold text-sm">压缩级别分布<br><span class="text-xs text-gray-400 font-normal">(Memory Compression)</span></div>
        </template>

        <div class="space-y-6 mt-4">
          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 0 (原始记忆)</span>
              <span class="text-gray-400">{{ levelPercent('0') }}%</span>
            </div>
            <el-progress :percentage="levelPercent('0')" :show-text="false" />
            <div class="text-right text-xs text-gray-500 mt-1">{{ levels?.levels['0'] ?? 0 }} 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 1 (轻度压缩)</span>
              <span class="text-gray-400">{{ levelPercent('1') }}%</span>
            </div>
            <el-progress :percentage="levelPercent('1')" :show-text="false" status="success" />
            <div class="text-right text-xs text-gray-500 mt-1">{{ levels?.levels['1'] ?? 0 }} 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 2 (中度压缩)</span>
              <span class="text-gray-400">{{ levelPercent('2') }}%</span>
            </div>
            <el-progress :percentage="levelPercent('2')" :show-text="false" status="warning" />
            <div class="text-right text-xs text-gray-500 mt-1">{{ levels?.levels['2'] ?? 0 }} 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 3 (高度压缩)</span>
              <span class="text-gray-400">{{ levelPercent('3') }}%</span>
            </div>
            <el-progress :percentage="levelPercent('3')" :show-text="false" status="exception" />
            <div class="text-right text-xs text-gray-500 mt-1">{{ levels?.levels['3'] ?? 0 }} 条</div>
          </div>
        </div>

        <div class="mt-8 pt-4 border-t border-dark-border text-xs text-gray-400">
          总计：{{ levels?.total ?? 0 }} 条记忆
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, computed } from 'vue'
import { getSnapshot, searchMemory, getMemoryLevels, type AgentSnapshot, type MemorySearchResult, type MemoryLevelsResponse } from '@/api/session'
import type { AgentNode } from '@/types'

const props = defineProps<{
  sessionId: string
  agents: AgentNode[]
}>()

const selectedAgent = ref('')
const searchQuery = ref('golang ddd architecture')
const snapshot = ref<AgentSnapshot | null>(null)
const episodes = ref<MemorySearchResult[]>([])
const levels = ref<MemoryLevelsResponse | null>(null)
const loading = ref(false)

const agentOptions = computed(() => props.agents.map(a => ({ label: a.name, value: a.inst_id })))

watch(() => props.agents, (agents) => {
  if (agents.length && !selectedAgent.value) {
    selectedAgent.value = agents[0].inst_id
  }
}, { immediate: true })

watch(selectedAgent, () => load())

async function load() {
  if (!selectedAgent.value || !props.sessionId) return
  loading.value = true
  try {
    const snapRes = await getSnapshot(selectedAgent.value, props.sessionId)
    snapshot.value = snapRes.snapshot
  } catch {
    snapshot.value = null
  }
  await doSearch()
  try {
    levels.value = await getMemoryLevels(selectedAgent.value, props.sessionId)
  } catch {
    levels.value = null
  }
  loading.value = false
}

async function doSearch() {
  if (!selectedAgent.value || !props.sessionId) return
  try {
    const res = await searchMemory(selectedAgent.value, props.sessionId, searchQuery.value, 10)
    episodes.value = res.results || []
  } catch {
    episodes.value = []
  }
}

function levelPercent(level: string) {
  if (!levels.value || levels.value.total === 0) return 0
  return Math.round((levels.value.levels[level] || 0) / levels.value.total * 100)
}

function fmtTime(iso: string) {
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
</script>

<style scoped>
.body-flex-1 {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}
:deep(.el-progress-bar__outer) {
  background-color: #2a2d35;
}
</style>
