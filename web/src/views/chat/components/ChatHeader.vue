<script setup lang="ts">
import { computed } from 'vue'
import type { Session, AgentNode } from '@/types'

const props = defineProps<{
  session: Session | null
  agents: AgentNode[]
}>()

const emit = defineEmits<{ (e: 'cancel'): void }>()

const statusColor = computed(() => {
  if (!props.session) return 'text-gray-500'
  switch (props.session.status) {
    case 'running': return 'text-blue-400'
    case 'completed': return 'text-green-400'
    case 'error': return 'text-red-400'
    default: return 'text-gray-500'
  }
})

const statusLabel = computed(() => {
  if (!props.session) return '未连接'
  switch (props.session.status) {
    case 'running': return '运行中'
    case 'completed': return '已完成'
    case 'error': return '失败'
    default: return props.session.status
  }
})

const chain = computed(() => {
  if (!props.agents.length) return [] as AgentNode[]
  // 按 type 排序: meta -> domain -> subdomain -> assistant
  const order = ['meta', 'domain', 'subdomain', 'assistant']
  return [...props.agents].sort((a, b) =>
    (order.indexOf(a.type) - order.indexOf(b.type)) || a.name.localeCompare(b.name))
})

function nodeColor(type: string) {
  if (type === 'meta') return 'text-blue-400'
  if (type === 'domain') return 'text-purple-400'
  if (type === 'subdomain') return 'text-fuchsia-400'
  if (type === 'assistant') return 'text-emerald-400'
  return 'text-gray-400'
}
</script>

<template>
  <div class="h-14 border-b border-[#2a2d35] bg-[#14161a] flex items-center px-6 gap-4 shrink-0">
    <div class="flex items-center gap-2 min-w-0 flex-1">
      <el-icon class="text-blue-400"><ChatLineRound /></el-icon>
      <span class="font-semibold text-gray-200 truncate">
        {{ session?.goal || '尚未选择会话' }}
      </span>
      <el-tag size="small" effect="plain"
              class="!bg-transparent !border-[#2a2d35] scale-90 shrink-0"
              :class="statusColor">{{ statusLabel }}</el-tag>
      <el-button v-if="session?.status === 'running'" size="small"
                 class="!bg-red-900/30 !border-red-700/40 !text-red-400 hover:!bg-red-800/50 shrink-0"
                 @click="emit('cancel')">
        <el-icon class="mr-1"><VideoPause /></el-icon>停止
      </el-button>
      <span class="text-xs text-gray-500 truncate shrink-0">{{ session?.id || '' }}</span>
    </div>

    <!-- Agent 链路 -->
    <div v-if="chain.length" class="flex items-center gap-1.5 text-xs text-gray-500 overflow-x-auto max-w-[60%]">
      <template v-for="(a, i) in chain" :key="a.inst_id">
        <span :class="nodeColor(a.type)" class="whitespace-nowrap">{{ a.name }}</span>
        <el-icon v-if="i < chain.length - 1" class="text-gray-600 text-[10px]"><ArrowRight /></el-icon>
      </template>
    </div>
  </div>
</template>
