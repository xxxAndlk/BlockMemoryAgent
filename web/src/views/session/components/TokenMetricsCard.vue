<script setup lang="ts">
import type { SessionTokenMetricsResponse } from '@/api/metrics'

defineProps<{
  tokenMetrics: SessionTokenMetricsResponse | null
}>()
</script>

<template>
  <el-card class="!border-line !bg-card">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-ink">Token 消耗 (Token Metrics)</div>
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
