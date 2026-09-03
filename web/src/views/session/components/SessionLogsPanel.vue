<script setup lang="ts">
import { fmtDateTime } from '@/utils/date'
import type { SessionLog } from '@/api/session'

const props = defineProps<{
  logs: SessionLog[]
}>()

const emit = defineEmits<{
  (e: 'update:agent', val: string): void
  (e: 'update:level', val: string): void
  (e: 'query'): void
}>()

const agentModel = defineModel<string>('agent')
const levelModel = defineModel<string>('level')
const expandedLogId = defineModel<number | null>('expandedLogId')
</script>

<template>
  <div class="flex-1 overflow-hidden flex flex-col p-4">
    <div class="flex items-center gap-3 mb-3 shrink-0">
      <el-input v-model="agentModel" placeholder="Agent 过滤" size="small" class="w-40" />
      <el-select v-model="levelModel" placeholder="Level" size="small" class="w-28">
        <el-option label="全部" value="" />
        <el-option label="info" value="info" />
        <el-option label="warn" value="warn" />
        <el-option label="error" value="error" />
      </el-select>
      <el-button size="small" type="primary" @click="emit('query')">查询</el-button>
    </div>
    <div class="flex-1 overflow-y-auto space-y-2 pr-1">
      <div v-if="!props.logs.length" class="text-ink-2 text-sm text-center py-10">暂无结构化日志</div>
      <div v-for="log in props.logs" :key="log.id" class="text-xs border border-line rounded p-2 bg-page">
        <div class="flex items-center justify-between mb-1">
          <div class="flex items-center gap-2">
            <el-tag size="small" :type="log.level === 'error' ? 'danger' : log.level === 'warn' ? 'warning' : 'info'" effect="plain" class="!bg-transparent !border-line scale-90 origin-left">{{ log.level }}</el-tag>
            <span class="text-ink-2">{{ log.phase }}</span>
            <span class="text-ink-2">{{ log.agent }}</span>
          </div>
          <span class="text-ink-2">{{ fmtDateTime(log.created_at) }}</span>
        </div>
        <div class="text-ink mb-1">{{ log.message }}</div>
        <div v-if="log.input_tokens || log.output_tokens" class="text-ink-2 mb-1">tokens: {{ log.input_tokens }} / {{ log.output_tokens }} · latency: {{ log.latency_ms }}ms · model: {{ log.model || '-' }}</div>
        <div v-if="log.prompt || log.response" class="mt-2">
          <el-button link size="small" type="primary" @click="expandedLogId = expandedLogId === log.id ? null : log.id">
            {{ expandedLogId === log.id ? '收起' : '展开 Prompt/Response' }}
          </el-button>
          <div v-if="expandedLogId === log.id" class="mt-2 space-y-2">
            <div v-if="log.prompt" class="bg-card p-2 rounded text-ink-2 whitespace-pre-wrap">{{ log.prompt }}</div>
            <div v-if="log.response" class="bg-card p-2 rounded text-ink-2 whitespace-pre-wrap">{{ log.response }}</div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
