<script setup lang="ts">
import { computed } from 'vue'
import type { SessionMetrics } from '@/api/metrics'
import { APP_CONFIG } from '@/config/app'

const props = defineProps<{
  metrics: SessionMetrics | null
}>()

// 上下文占用（2026-10-19）：进度条口径 = 顶层 Agent 最近一次 LLM 调用的输入 token
//（system+历史+tools 的真实请求输入），衡量"当前上下文有多长"；与会话累计消耗分开。
const currentContext = computed(() => props.metrics?.current_context_tokens ?? 0)

const contextPercent = computed(() => {
  if (currentContext.value <= 0) return 0
  return Math.min(100, Math.round((currentContext.value / APP_CONFIG.tokenContextLimit) * 100))
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
    <div class="mb-3 text-xs">
      <div class="flex justify-between mb-2">
        <span>当前上下文占用</span>
        <span class="text-blue-400 font-bold">{{ contextPercent }}%</span>
      </div>
      <div class="text-ink-2 mb-2">{{ currentContext.toLocaleString() }} / {{ APP_CONFIG.tokenContextLimit.toLocaleString() }} <span class="text-[10px]">tokens</span></div>
      <div class="relative pt-1">
        <el-progress :percentage="contextPercent" :show-text="false" class="custom-progress" />
        <div class="absolute top-0 bottom-0 left-[80%] border-l-2 border-yellow-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
        <div class="absolute top-0 bottom-0 left-[95%] border-l-2 border-red-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
        <div class="flex justify-between mt-1 text-[10px]">
          <span class="text-yellow-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-yellow-500"></div> {{ Math.round(APP_CONFIG.warningThreshold * 100) }}% 警告线</span>
          <span class="text-red-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-red-500"></div> {{ Math.round(APP_CONFIG.errorThreshold * 100) }}% 硬限制</span>
        </div>
      </div>
    </div>

    <!-- 累计消耗（2026-10-19）：与上下文占用分开的总消耗口径（全会话所有 Agent 累计）。 -->
    <div class="text-xs text-ink-2 mb-2">累计消耗</div>
    <div class="mb-6 text-xs flex items-baseline justify-between">
      <span class="text-ink-2">会话累计 Token</span>
      <span class="text-ink font-bold text-base">{{ (metrics?.total_tokens ?? 0).toLocaleString() }}</span>
    </div>
    <div class="text-ink-3 text-[10px] -mt-5 mb-6 text-right">
      输入 {{ (metrics?.input_tokens ?? 0).toLocaleString() }} · 输出 {{ (metrics?.output_tokens ?? 0).toLocaleString() }}
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
