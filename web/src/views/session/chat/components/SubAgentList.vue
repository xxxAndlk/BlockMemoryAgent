<script setup lang="ts">
// SubAgentList.vue 对话栏子 Agent 列表（2026-09-12 用户诉求）：
// 任务全部派发出去、正在等子 Agent 时，用户最想知道"派了谁、各自在干什么、进展如何"。
// 列表来自回合内的派发事件（一人一行：中文领域名 + 任务简要），状态用 agents 接口的
// 实时实例状态叠加——事件只记录"派发"这一刻，完成/失败要跟实例状态走。
import { computed } from 'vue'
import type { AgentNode } from '@/types'
import type { SubAgentRef } from '../utils/turns'
import { fmtTime } from '../utils/eventStyles'

const props = defineProps<{
  items: SubAgentRef[]
  /** 会话内全部 Agent 实例（父级每 3s 轮询刷新）：用于取实时状态 */
  agents: AgentNode[]
  /** 本回合是否仍在进行（决定未知状态的兜底文案） */
  running: boolean
}>()

interface StatusStyle {
  text: string
  cls: string
  dot: string
}

/** 状态字典：覆盖编排树全部节点状态（orchestrator.Status.String()）。 */
const STATUS_MAP: Record<string, StatusStyle> = {
  running: { text: '运行中', cls: 'text-blue-600 dark:text-blue-300', dot: 'bg-blue-500 animate-pulse' },
  active: { text: '运行中', cls: 'text-blue-600 dark:text-blue-300', dot: 'bg-blue-500 animate-pulse' },
  done: { text: '已完成', cls: 'text-green-600 dark:text-green-300', dot: 'bg-green-500' },
  failed: { text: '失败', cls: 'text-red-600 dark:text-red-300', dot: 'bg-red-500' },
  error: { text: '失败', cls: 'text-red-600 dark:text-red-300', dot: 'bg-red-500' },
  cancelled: { text: '已取消', cls: 'text-ink-3', dot: 'bg-ink-3' },
  paused: { text: '已暂停', cls: 'text-amber-600 dark:text-amber-300', dot: 'bg-amber-500' },
  idle: { text: '空闲（热驻）', cls: 'text-ink-3', dot: 'bg-ink-3' },
  'delivered-unverified': { text: '已交付待验证', cls: 'text-amber-600 dark:text-amber-300', dot: 'bg-amber-500' },
}
const UNKNOWN: StatusStyle = { text: '—', cls: 'text-ink-3', dot: 'bg-ink-3' }

/**
 * 领域名 → 实时实例（派发瞬间实例尚未进树，下一次轮询即补齐）。
 * 同名多实例（旧实例已完成、复活/续建起了新实例）时优先取**在跑的**——否则状态会
 * 挂在早已结束的旧实例上显示"已完成/空闲"，而实际工作还在跑。
 */
const byName = computed(() => {
  const m = new Map<string, AgentNode>()
  const put = (key: string, a: AgentNode) => {
    const prev = m.get(key)
    if (!prev || (prev.status !== 'running' && a.status === 'running')) m.set(key, a)
  }
  for (const a of props.agents) {
    if (a.domain) put(a.domain, a)
    if (a.name) put(a.name, a)
  }
  return m
})

/** 公共前缀长度：旧事件的派发事件没带领域名，只能靠任务文本与实例 goal 对齐。 */
function commonPrefixLen(a: string, b: string): number {
  const n = Math.min(a.length, b.length)
  let i = 0
  while (i < n && a[i] === b[i]) i++
  return i
}

/**
 * 找该派发对应的实例：领域名精确匹配优先；旧事件（后端未带 domain）回退"任务前缀 ↔
 * 实例 goal"最长公共前缀匹配，并要求唯一——多个实例并列最长说明区分不开，
 * 宁可不显示状态，也不能把 A 的状态挂到 B 头上。
 */
function instanceOf(item: SubAgentRef): AgentNode | undefined {
  const direct = byName.value.get(item.name)
  if (direct) return direct
  const probe = item.task
  if (!probe) return undefined
  let best: AgentNode | undefined
  let bestLen = 0
  let tied = false
  for (const a of props.agents) {
    const goal = a.goal || ''
    if (!goal) continue
    const l = commonPrefixLen(goal, probe)
    if (l < 16) continue
    if (l > bestLen) {
      best = a
      bestLen = l
      tied = false
    } else if (l === bestLen) {
      tied = true
    }
  }
  return tied ? undefined : best
}

const rows = computed(() =>
  props.items.map((item) => {
    const node = instanceOf(item)
    const style: StatusStyle = node ? STATUS_MAP[node.status] || UNKNOWN : UNKNOWN
    // 展示名：事件带的领域名优先；旧事件回退到匹配到的实例领域名；再兜底通用标签。
    const name =
      item.name === '子 Agent' && node?.domain ? node.domain : item.name
    return { ...item, name, style }
  }),
)

/** 运行中 / 失败 / 总数：头部统计，一眼看出整批进度。 */
const counts = computed(() => {
  let runningN = 0
  let failedN = 0
  for (const r of rows.value) {
    if (r.style.text === '运行中') runningN++
    else if (r.style.text === '失败') failedN++
  }
  return { running: runningN, failed: failedN, total: rows.value.length }
})
</script>

<template>
  <div v-if="items.length" class="rounded-lg border border-line bg-page my-2">
    <div class="px-3 py-2 flex items-center gap-2 text-xs text-ink flex-wrap">
      <el-icon class="text-blue-400"><Grid /></el-icon>
      <span class="font-medium">子 Agent</span>
      <span class="text-ink-2">
        共 {{ counts.total }}
        <template v-if="counts.running">· 运行中 {{ counts.running }}</template>
        <template v-if="counts.failed">· 失败 {{ counts.failed }}</template>
      </span>
    </div>

    <!-- 一人一行：领域名 · 任务简要 · 状态（长列表限高滚动，不把对话栏顶飞） -->
    <div class="px-3 pb-2 space-y-1 max-h-56 overflow-y-auto">
      <div v-for="r in rows" :key="r.key"
           class="flex items-center gap-2 text-xs rounded px-1.5 py-1 hover:bg-card transition-colors">
        <span class="shrink-0 w-1.5 h-1.5 rounded-full" :class="r.style.dot"></span>
        <span class="shrink-0 font-medium text-ink max-w-[38%] truncate" :title="r.name">{{ r.name }}</span>
        <span class="text-ink-2">·</span>
        <span class="flex-1 min-w-0 truncate text-ink-2" :title="r.task">{{ r.task || '（未提供任务摘要）' }}</span>
        <span class="shrink-0 text-[10px] text-ink-3 hidden sm:inline">{{ fmtTime(r.ts) }}</span>
        <span class="shrink-0 w-24 text-right" :class="r.style.cls">{{ r.style.text }}</span>
      </div>
    </div>
  </div>
</template>
