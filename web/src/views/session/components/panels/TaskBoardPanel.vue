<script setup lang="ts">
import { computed, ref, toRef } from 'vue'
import { CircleCheck, Loading, Lock } from '@element-plus/icons-vue'
import type { AgentNode, SessionEvent, TaskBoardData } from '@/types'
import { useTaskBoard } from '@/composables/useTaskBoard'
import { useGoalTimeline } from '@/composables/useGoalTimeline'
import OrchMiniCanvas from './OrchMiniCanvas.vue'

const props = defineProps<{
  agents: AgentNode[]
  board: TaskBoardData | null
  goal?: string
  /** 会话事件流：任务目标时间线数据源（设计 §8）。 */
  events?: SessionEvent[]
}>()

const { tasks, constraints, taskProgress } = useTaskBoard(toRef(props, 'board'))
// 时间线（设计 §8）：user/派发/完成/失败里程碑 + board.tasks 兜底，纯前端聚合。
const { milestones } = useGoalTimeline(
  computed(() => props.events || []),
  toRef(props, 'board'),
)

// 任务目标：board 快照的 goal 优先（随执行演进），回退会话初始 goal
const goalText = computed(() => (props.board?.goal || props.goal || '').trim())
const goalExpanded = ref(false)
/** 时间线折叠：条数超过阈值时默认收起，避免长会话把面板撑爆。 */
const timelineExpanded = ref(false)
const TIMELINE_PREVIEW = 6
const shownMilestones = computed(() =>
  timelineExpanded.value ? milestones.value : milestones.value.slice(0, TIMELINE_PREVIEW),
)

const statusLegend = [
  { icon: CircleCheck, cls: 'text-green-500', label: '完成' },
  { icon: Loading, cls: 'text-yellow-500', label: '运行中' },
  { icon: Lock, cls: 'text-ink-2', label: '等待/阻塞' },
]

/** 里程碑圆点配色：user=主色 / dispatch=蓝 / done=绿 / failed=红。 */
function milestoneDot(kind: string): string {
  if (kind === 'user') return 'bg-primary'
  if (kind === 'dispatch') return 'bg-blue-500'
  if (kind === 'done') return 'bg-green-500'
  return 'bg-red-500'
}

function fmtTime(iso?: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  return isNaN(d.getTime()) ? '' : d.toLocaleTimeString('zh-CN', { hour12: false })
}
</script>

<template>
  <div class="space-y-4 text-xs">
    <!-- 任务目标（对齐 TUI 执行计划面板：goal + 编号任务行 + 总体进度） -->
    <div>
      <div class="flex justify-between items-center mb-2">
        <span class="font-bold text-sm text-ink">任务目标</span>
        <el-progress v-if="tasks.length" :percentage="taskProgress" :show-text="false" class="w-20 custom-progress" />
      </div>
      <!-- 目标文本默认单行折叠，点击展开（长目标不再整段撑开面板） -->
      <div v-if="goalText" class="p-2 mb-2 bg-page rounded border border-line text-ink leading-relaxed">
        <div :class="goalExpanded ? '' : 'truncate'">{{ goalText }}</div>
        <button v-if="goalText.length > 40" class="text-[11px] text-primary hover:underline mt-0.5"
                @click="goalExpanded = !goalExpanded">{{ goalExpanded ? '收起' : '展开' }}</button>
      </div>

      <!-- 任务目标时间线：user/派发/完成/失败里程碑 -->
      <div v-if="milestones.length" class="mb-2">
        <div class="space-y-1">
          <div v-for="(m, i) in shownMilestones" :key="i" class="flex items-start gap-2">
            <span class="w-1.5 h-1.5 rounded-full mt-1 shrink-0" :class="milestoneDot(m.kind)"></span>
            <span class="text-[10px] text-ink-3 font-mono w-14 shrink-0">{{ fmtTime(m.at) }}</span>
            <span class="text-ink-2 flex-1 min-w-0 break-words">{{ m.text }}</span>
          </div>
        </div>
        <button v-if="milestones.length > TIMELINE_PREVIEW" class="text-[11px] text-primary hover:underline mt-1"
                @click="timelineExpanded = !timelineExpanded">
          {{ timelineExpanded ? '收起' : `显示全部 ${milestones.length} 条` }}
        </button>
      </div>
      <div v-if="!tasks.length" class="text-ink-3 py-3 text-center">暂无子任务，等待 DomainAgent 拆解</div>
      <div v-else class="space-y-1.5">
        <div v-for="(t, i) in tasks" :key="i"
             class="flex items-center gap-2 p-1.5 bg-page rounded border border-line">
          <span class="w-4 text-right text-ink-3 font-mono shrink-0">{{ i + 1 }}.</span>
          <el-icon v-if="t.status === 'done'" class="text-green-500 text-sm shrink-0"><CircleCheck /></el-icon>
          <el-icon v-else-if="t.status === 'running' || t.status === 'in_progress'" class="text-yellow-500 text-sm shrink-0"><Loading /></el-icon>
          <el-icon v-else-if="t.status === 'delivered-unverified'" class="text-yellow-500 text-sm shrink-0"><WarningFilled /></el-icon>
          <el-icon v-else-if="t.status === 'failed'" class="text-red-500 text-sm shrink-0"><CircleClose /></el-icon>
          <el-icon v-else-if="t.status === 'blocked'" class="text-ink-2 text-sm shrink-0"><Lock /></el-icon>
          <el-icon v-else class="text-ink-3 text-sm shrink-0"><CirclePlus /></el-icon>
          <span class="text-ink truncate flex-1" :class="t.status === 'done' ? 'opacity-60' : ''">{{ t.title || t.name }}</span>
          <span v-if="t.assignee" class="text-[10px] text-ink-3 shrink-0 font-mono">{{ t.assignee }}</span>
        </div>
      </div>
    </div>

    <!-- Agent 编排：横向滑动迷你画布，点击节点直达该 Agent 对话 -->
    <div class="p-2 bg-page rounded border border-line">
      <div class="flex justify-between items-center mb-1">
        <span class="font-bold text-sm text-ink">🌳 Agent 编排</span>
        <div class="flex items-center gap-2 text-[10px] text-ink-3">
          <span v-for="l in statusLegend" :key="l.label" class="flex items-center gap-0.5">
            <el-icon :class="l.cls" class="text-[11px]"><component :is="l.icon" /></el-icon>{{ l.label }}
          </span>
        </div>
      </div>
      <div class="text-[11px] text-ink-3 mb-2">层级树图查看 Agent 关系，点击节点直达该 Agent 对话。</div>
      <OrchMiniCanvas :agents="agents" />
    </div>

    <!-- 约束条件 -->
    <div v-if="constraints.length">
      <div class="font-bold text-sm text-ink mb-2">约束条件</div>
      <div class="space-y-1.5">
        <div v-for="(c, i) in constraints" :key="i" class="flex justify-between p-2 bg-page rounded border border-line">
          <span class="text-ink-3">{{ c[0] }}</span>
          <span class="text-ink">{{ c[1] }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
:deep(.custom-tree .el-tree-node__content) {
  background-color: transparent !important;
  height: 28px;
}
:deep(.custom-tree .el-tree-node__content:hover) {
  background-color: var(--bma-primary-soft) !important;
}
:deep(.custom-progress .el-progress-bar__outer) {
  background-color: var(--bma-border);
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: var(--bma-primary);
}
</style>
