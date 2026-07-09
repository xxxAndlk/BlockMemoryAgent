<script setup lang="ts">
import { fmtTime } from '@/utils/date'
import type { WatchdogDecision } from '@/api/session'

const props = defineProps<{
  decisions: WatchdogDecision[]
}>()

function tagType(level: string) {
  switch (level) {
    case 'OK': return 'success'
    case 'WARN': return 'warning'
    case 'COMPRESS': return 'warning'
    case 'EVICT': return 'danger'
    default: return 'info'
  }
}

function label(level: string) {
  switch (level) {
    case 'OK': return '正常'
    case 'WARN': return '警告'
    case 'COMPRESS': return '压缩'
    case 'EVICT': return '驱逐'
    default: return level
  }
}
</script>

<template>
  <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
    <template #header>
      <div class="flex justify-between items-center">
        <div class="font-bold text-sm text-gray-200">看门狗状态</div>
      </div>
    </template>
    <div class="space-y-1 text-xs">
      <div v-for="d in props.decisions.slice(0, 5)" :key="d.agent_id + d.occurred_at" class="flex items-center gap-4">
        <span class="text-gray-500 w-12">{{ fmtTime(d.occurred_at) }}</span>
        <el-tag size="small" :type="tagType(d.level)" effect="plain" class="!bg-transparent !border-[#2a2d35] w-16 text-center">{{ d.level }}</el-tag>
        <span class="text-gray-300 truncate flex-1">{{ label(d.level) }}</span>
      </div>
      <div v-if="!props.decisions.length" class="text-gray-500 text-xs">暂无看门狗决策</div>
    </div>
  </el-card>
</template>
