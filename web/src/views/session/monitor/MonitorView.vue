<script setup lang="ts">
import { ref } from 'vue'
import type { Session, SessionEvent, AgentNode } from '@/types'
import type { SessionLog } from '@/api/session'
import ExecutionLog from '../components/ExecutionLog.vue'
import SkillSet from '../components/SkillSet.vue'
import SessionLogsPanel from '../components/SessionLogsPanel.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  sessionLogs: SessionLog[]
}>()

const logAgent = defineModel<string>('logAgent', { default: '' })
const logLevel = defineModel<string>('logLevel', { default: '' })
const expandedLogId = defineModel<number | null>('expandedLogId', { default: null })

const emit = defineEmits<{ 'query-logs': [] }>()

const activeTab = ref<'log' | 'skill' | 'logs'>('log')
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <div class="h-12 border-b border-line flex items-center px-4 gap-6 text-sm shrink-0">
      <span @click="activeTab = 'log'"
            :class="activeTab === 'log' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Document /></el-icon> 执行日志
      </span>
      <span @click="activeTab = 'skill'"
            :class="activeTab === 'skill' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Connection /></el-icon> Skill 装配
      </span>
      <span @click="activeTab = 'logs'"
            :class="activeTab === 'logs' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Tickets /></el-icon> 日志分析
      </span>
    </div>

    <div class="flex-1 overflow-hidden relative flex flex-col">
      <ExecutionLog v-if="activeTab === 'log'" :events="events" />
      <SkillSet v-if="activeTab === 'skill'" :agents="agents" />
      <SessionLogsPanel
        v-if="activeTab === 'logs'"
        :agent="logAgent"
        @update:agent="(v: string) => (logAgent = v)"
        :level="logLevel"
        @update:level="(v: string) => (logLevel = v)"
        :expanded-log-id="expandedLogId"
        @update:expanded-log-id="(v: number | null | undefined) => (expandedLogId = v ?? null)"
        :logs="sessionLogs"
        @query="emit('query-logs')"
      />
    </div>
  </div>
</template>
