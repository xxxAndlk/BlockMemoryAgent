<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Session, SessionEvent, AgentNode } from '@/types'
import type { SessionLog } from '@/api/session'
import ExecutionLog from '../components/ExecutionLog.vue'
import SessionLogsPanel from '../components/SessionLogsPanel.vue'
import EfficiencyPanel from './EfficiencyPanel.vue'
import { matchAgentLog } from './agentMatch'

const props = defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  sessionLogs: SessionLog[]
  /** 选中的非 meta Agent（?agent=）：执行日志锁定过滤为该 Agent，效率审计自动下钻其支路；
   *  未选中/选中 MetaAgent 时为 null，维持全会话监控 */
  agent?: AgentNode | null
}>()

const logAgent = defineModel<string>('logAgent', { default: '' })
const logLevel = defineModel<string>('logLevel', { default: '' })
const expandedLogId = defineModel<number | null>('expandedLogId', { default: null })

const emit = defineEmits<{ 'query-logs': [] }>()

// Skill 装配 tab 已移除：技能装配/技能库在「技能库」页统一看（此处与那页重复）。
const activeTab = ref<'log' | 'logs' | 'efficiency'>('log')

// 日志分析：选中子 Agent 时只展示该 Agent 的结构化日志（客户端归一化匹配，
// 服务端 agent 过滤是精确等值，匹配不上带"领域Agent"后缀/截断的展示名）。
const visibleLogs = computed(() =>
  props.agent ? props.sessionLogs.filter((l) => matchAgentLog(l, props.agent!)) : props.sessionLogs
)
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <div class="h-12 border-b border-line flex items-center px-4 gap-6 text-sm shrink-0">
      <span @click="activeTab = 'log'"
            :class="activeTab === 'log' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Document /></el-icon> 执行日志
      </span>
      <span @click="activeTab = 'logs'"
            :class="activeTab === 'logs' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Tickets /></el-icon> 日志分析
      </span>
      <span @click="activeTab = 'efficiency'"
            :class="activeTab === 'efficiency' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><DataAnalysis /></el-icon> 效率审计
      </span>
    </div>

    <div class="flex-1 overflow-hidden relative flex flex-col">
      <ExecutionLog v-if="activeTab === 'log'" :events="events" :force-agent="agent" />
      <SessionLogsPanel
        v-if="activeTab === 'logs'"
        :force-agent-name="agent?.name"
        :agent="logAgent"
        @update:agent="(v: string) => (logAgent = v)"
        :level="logLevel"
        @update:level="(v: string) => (logLevel = v)"
        :expanded-log-id="expandedLogId"
        @update:expanded-log-id="(v: number | null | undefined) => (expandedLogId = v ?? null)"
        :logs="visibleLogs"
        @query="emit('query-logs')"
      />
      <EfficiencyPanel v-if="activeTab === 'efficiency'" :session="session" :selected-node-id="agent?.inst_id" />
    </div>
  </div>
</template>
