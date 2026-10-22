<script setup lang="ts">
import { computed } from 'vue'
import type { SessionMetrics } from '@/api/metrics'
import { APP_CONFIG } from '@/config/app'
import { formatTokens } from '@/utils/tokens'

const props = defineProps<{
  metrics: SessionMetrics | null
}>()

// 上下文占用（2026-10-19）：进度条口径 = 顶层 Agent 最近一次 LLM 调用的输入 token
//（system+历史+tools 的真实请求输入），衡量"当前上下文有多长"；与会话累计消耗分开。
const currentContext = computed(() => props.metrics?.current_context_tokens ?? 0)

// 阈值（2026-10-22）：分母取后端下发的真实阈值（config.agent.token_budget_per_role，
// 按档位顶层角色解析），不再写死 128K；0/缺省 = 该角色未设阈值，不画进度条。
const contextLimit = computed(() => props.metrics?.context_token_budget ?? 0)
const hasLimit = computed(() => contextLimit.value > 0)

const contextPercent = computed(() => {
  if (currentContext.value <= 0 || !hasLimit.value) return 0
  return Math.min(100, Math.round((currentContext.value / contextLimit.value) * 100))
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
      <div class="text-ink-2 mb-2">{{ formatTokens(currentContext) }} / {{ hasLimit ? formatTokens(contextLimit) : '不限' }} <span class="text-[10px]">tokens</span></div>
      <div v-if="hasLimit" class="relative pt-1">
        <el-progress :percentage="contextPercent" :show-text="false" class="custom-progress" />
        <div class="absolute top-0 bottom-0 border-l-2 border-yellow-500 z-10 h-full -mt-0.5" style="height: 12px;"
             :style="{ left: `${APP_CONFIG.warningThreshold * 100}%` }"></div>
        <div class="absolute top-0 bottom-0 border-l-2 border-red-500 z-10 h-full -mt-0.5" style="height: 12px;"
             :style="{ left: `${APP_CONFIG.errorThreshold * 100}%` }"></div>
        <div class="flex justify-between mt-1 text-[10px]">
          <span class="text-yellow-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-yellow-500"></div> {{ Math.round(APP_CONFIG.warningThreshold * 100) }}% 警告线</span>
          <span class="text-red-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-red-500"></div> {{ Math.round(APP_CONFIG.errorThreshold * 100) }}% 硬限制</span>
        </div>
      </div>
      <div v-else class="text-ink-3 text-[10px]">当前角色未设上下文阈值（不限制）</div>
    </div>

    <!-- 累计消耗（2026-10-19）：与上下文占用分开的总消耗口径（全会话所有 Agent 累计）。 -->
    <div class="text-xs text-ink-2 mb-2">累计消耗</div>
    <div class="mb-6 text-xs flex items-baseline justify-between">
      <span class="text-ink-2">会话累计 Token</span>
      <span class="text-ink font-bold text-base">{{ formatTokens(metrics?.total_tokens) }}</span>
    </div>
    <div class="text-ink-3 text-[10px] -mt-5 mb-6 text-right">
      输入 {{ formatTokens(metrics?.input_tokens) }} · 输出 {{ formatTokens(metrics?.output_tokens) }}
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
