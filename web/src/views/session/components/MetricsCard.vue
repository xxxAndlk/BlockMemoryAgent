<script setup lang="ts">
import { computed } from 'vue'
import type { SessionMetrics } from '@/api/metrics'
import { APP_CONFIG } from '@/config/app'

const props = defineProps<{
  metrics: SessionMetrics | null
}>()

const tokenPercent = computed(() => {
  if (!props.metrics || props.metrics.total_tokens <= 0) return 0
  return Math.min(100, Math.round((props.metrics.total_tokens / APP_CONFIG.tokenContextLimit) * 100))
})
</script>

<template>
  <el-card class="!border-line !bg-card">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-ink">实时指标 (Metrics)</div>
      </div>
    </template>

    <div class="text-xs text-ink-2 mb-2">LLM 调用统计</div>
    <div class="grid grid-cols-4 gap-2 mb-6">
      <div class="p-2 text-center">
        <div class="text-xs text-ink-2 mb-1">总调用</div>
        <div class="text-xl font-bold text-ink">{{ metrics?.calls ?? 0 }}<span class="text-xs font-normal ml-1">次</span></div>
      </div>
      <div class="p-2 text-center">
        <div class="text-xs text-ink-2 mb-1">超时</div>
        <div class="text-xl font-bold text-red-400">{{ metrics?.timeouts ?? 0 }}<span class="text-xs font-normal ml-1">次</span></div>
      </div>
      <div class="p-2 text-center">
        <div class="text-xs text-ink-2 mb-1">平均耗时</div>
        <div class="text-xl font-bold text-green-400">{{ metrics?.avg_duration ?? '-' }}</div>
      </div>
      <div class="p-2 text-center">
        <div class="text-xs text-ink-2 mb-1">最长耗时</div>
        <div class="text-xl font-bold text-ink">{{ metrics?.max_duration ?? '-' }}</div>
      </div>
    </div>

    <div class="text-xs text-ink-2 mb-2">上下文用量</div>
    <div class="mb-6 text-xs">
      <div class="flex justify-between mb-2">
        <span>当前会话 Token 消耗</span>
        <span class="text-blue-400 font-bold">{{ tokenPercent }}%</span>
      </div>
      <div class="text-ink-2 mb-2">{{ (metrics?.total_tokens ?? 0).toLocaleString() }} / {{ APP_CONFIG.tokenContextLimit.toLocaleString() }} <span class="text-[10px]">tokens</span></div>
      <div class="relative pt-1">
        <el-progress :percentage="tokenPercent" :show-text="false" class="custom-progress" />
        <div class="absolute top-0 bottom-0 left-[80%] border-l-2 border-yellow-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
        <div class="absolute top-0 bottom-0 left-[95%] border-l-2 border-red-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
        <div class="flex justify-between mt-1 text-[10px]">
          <span class="text-yellow-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-yellow-500"></div> {{ Math.round(APP_CONFIG.warningThreshold * 100) }}% 警告线</span>
          <span class="text-red-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-red-500"></div> {{ Math.round(APP_CONFIG.errorThreshold * 100) }}% 硬限制</span>
        </div>
      </div>
    </div>

    <slot name="footer" />
  </el-card>
</template>

<style scoped>
:deep(.custom-progress .el-progress-bar__outer) {
  background-color: var(--bma-border);
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: var(--bma-primary);
}
</style>
