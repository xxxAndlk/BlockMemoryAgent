<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import type { SessionTokenMetricsResponse, SessionEfficiency } from '@/api/metrics'
import { getSessionEfficiency } from '@/api/metrics'

const props = defineProps<{
  tokenMetrics: SessionTokenMetricsResponse | null
  /** 会话 ID：非空时并行拉取 /efficiency 做会话累计行（TODO #15 T14 成本可见） */
  sessionId?: string
}>()

// 会话累计（T14）：总 token / 总耗时 / 总轮次，数据复用 GET /sessions/:id/efficiency
//（纯聚合端点，无新采集管道）。失败静默——累计行隐藏，卡片主体不受影响。
const efficiency = ref<SessionEfficiency | null>(null)

async function loadEfficiency(id?: string) {
  efficiency.value = null
  if (!id) return
  try {
    efficiency.value = await getSessionEfficiency(id)
  } catch {
    efficiency.value = null
  }
}

watch(() => props.sessionId, (id) => { void loadEfficiency(id) }, { immediate: true })

const totalTokens = computed(() =>
  efficiency.value ? efficiency.value.total_input_tokens + efficiency.value.total_output_tokens : 0)

// 总耗时 = 支路墙钟和（并行支路有重叠，仅作感知参考口径，标注在 UI）。
const totalWallSec = computed(() =>
  efficiency.value ? efficiency.value.branches.reduce((a, b) => a + (b.wall_clock_sec || 0), 0) : 0)

const totalRounds = computed(() =>
  efficiency.value ? efficiency.value.branches.reduce((a, b) => a + (b.rounds || 0), 0) : 0)

function fmtWall(sec: number): string {
  if (!sec) return '0s'
  if (sec < 60) return `${Math.round(sec)}s`
  const m = Math.floor(sec / 60)
  const s = Math.round(sec % 60)
  return s ? `${m}m${s}s` : `${m}m`
}
</script>

<template>
  <el-card class="!border-line !bg-card">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-ink">Token 消耗 (Token Metrics)</div>
      </div>
    </template>
    <!-- 会话累计行（T14）：效率端点聚合的总 token/耗时/轮次 -->
    <template v-if="efficiency">
      <div class="text-xs text-ink-2 mb-2">会话累计</div>
      <div class="grid grid-cols-3 gap-2 mb-4 text-center">
        <div>
          <div class="text-xs text-ink-2">总 Token</div>
          <div class="text-lg font-bold text-ink">{{ totalTokens.toLocaleString() }}</div>
        </div>
        <div>
          <div class="text-xs text-ink-2">支路耗时</div>
          <div class="text-lg font-bold text-ink" title="各支路墙钟之和；并行支路有重叠，仅作量级参考">{{ fmtWall(totalWallSec) }}</div>
        </div>
        <div>
          <div class="text-xs text-ink-2">总轮次</div>
          <div class="text-lg font-bold text-ink">{{ totalRounds.toLocaleString() }}</div>
        </div>
      </div>
    </template>
    <div class="text-xs text-ink-2 mb-2">总计</div>
    <div class="grid grid-cols-3 gap-2 mb-4 text-center">
      <div>
        <div class="text-xs text-ink-2">Input</div>
        <div class="text-lg font-bold text-ink">{{ (tokenMetrics?.total_input_tokens ?? 0).toLocaleString() }}</div>
      </div>
      <div>
        <div class="text-xs text-ink-2">Output</div>
        <div class="text-lg font-bold text-ink">{{ (tokenMetrics?.total_output_tokens ?? 0).toLocaleString() }}</div>
      </div>
      <div>
        <div class="text-xs text-ink-2">Calls</div>
        <div class="text-lg font-bold text-ink">{{ tokenMetrics?.total_calls ?? 0 }}</div>
      </div>
    </div>
    <div class="text-xs text-ink-2 mb-2">按 Agent / Model</div>
    <div class="space-y-1 text-xs">
      <div v-for="s in tokenMetrics?.stats || []" :key="s.agent + '|' + s.model" class="flex justify-between p-2 bg-page rounded">
        <span class="text-ink-2 truncate flex-1">{{ s.agent }} <span v-if="s.model" class="text-ink-3">({{ s.model }})</span></span>
        <span class="text-ink">{{ s.input_tokens + s.output_tokens }}</span>
      </div>
      <div v-if="!tokenMetrics?.stats?.length" class="text-ink-2 text-xs text-center py-2">暂无 token 数据</div>
    </div>
  </el-card>
</template>
