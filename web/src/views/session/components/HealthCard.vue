<script setup lang="ts">
import { healthDotClass, healthStatusText } from '@/utils/sessionStatus'
import type { HealthResponse } from '@/api/health'

const props = defineProps<{
  health: HealthResponse | null
}>()
</script>

<template>
  <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-gray-200">系统状态 (System Health)</div>
      </div>
    </template>
    <div class="space-y-2 text-xs">
      <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
        <el-icon class="text-gray-400 text-lg"><Coin /></el-icon>
        <div class="w-16 text-gray-300">Postgres</div>
        <div class="w-16" :class="healthDotClass(props.health?.postgres)">{{ healthStatusText(props.health?.postgres) }}</div>
        <div class="text-gray-500 flex-1">{{ props.health?.postgres?.online ? `延迟: ${props.health?.postgres?.latency_ms}ms` : '未连接' }}</div>
      </div>
      <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
        <el-icon class="text-gray-400 text-lg"><DataLine /></el-icon>
        <div class="w-16 text-gray-300">Redis</div>
        <div class="w-16" :class="healthDotClass(props.health?.redis)">{{ healthStatusText(props.health?.redis) }}</div>
        <div class="text-gray-500 flex-1">{{ props.health?.redis?.online ? `延迟: ${props.health?.redis?.latency_ms}ms` : '未连接' }}</div>
      </div>
      <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
        <el-icon class="text-gray-400 text-lg"><Connection /></el-icon>
        <div class="w-16 text-gray-300">LLM API</div>
        <div class="w-16" :class="healthDotClass(props.health?.llm)">{{ healthStatusText(props.health?.llm) }}</div>
        <div class="text-gray-500 flex-1 truncate">{{ props.health?.llm?.detail || '未配置' }}</div>
      </div>
    </div>
  </el-card>
</template>
