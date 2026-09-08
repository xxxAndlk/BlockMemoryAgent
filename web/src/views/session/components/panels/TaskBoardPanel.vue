<script setup lang="ts">
import { computed, toRef } from 'vue'
import { CircleCheck, Loading, Lock } from '@element-plus/icons-vue'
import type { AgentNode, TaskBoardData } from '@/types'
import { useRoleTree } from '@/composables/useRoleTree'
import { useTaskBoard } from '@/composables/useTaskBoard'

const props = defineProps<{
  agents: AgentNode[]
  board: TaskBoardData | null
  goal?: string
}>()

const { roleTree, defaultProps } = useRoleTree(toRef(props, 'agents'))
const { tasks, constraints, taskProgress } = useTaskBoard(toRef(props, 'board'))

// 任务目标：board 快照的 goal 优先（随执行演进），回退会话初始 goal
const goalText = computed(() => (props.board?.goal || props.goal || '').trim())

const statusLegend = [
  { icon: CircleCheck, cls: 'text-green-500', label: '完成' },
  { icon: Loading, cls: 'text-yellow-500', label: '运行中' },
  { icon: Lock, cls: 'text-ink-2', label: '等待/阻塞' },
]
</script>

<template>
  <div class="space-y-4 text-xs">
    <!-- 任务目标（对齐 TUI 执行计划面板：goal + 编号任务行 + 总体进度） -->
    <div>
      <div class="flex justify-between items-center mb-2">
        <span class="font-bold text-sm text-ink">任务目标</span>
        <el-progress v-if="tasks.length" :percentage="taskProgress" :show-text="false" class="w-20 custom-progress" />
      </div>
      <div v-if="goalText" class="p-2 mb-2 bg-page rounded border border-line text-ink leading-relaxed">{{ goalText }}</div>
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

    <!-- Agent 编排（对齐 TUI 编排面板：角色树 + 状态图例） -->
    <div>
      <div class="flex justify-between items-center mb-2">
        <span class="font-bold text-sm text-ink">Agent 编排</span>
        <div class="flex items-center gap-2 text-[10px] text-ink-3">
          <span v-for="l in statusLegend" :key="l.label" class="flex items-center gap-0.5">
            <el-icon :class="l.cls" class="text-[11px]"><component :is="l.icon" /></el-icon>{{ l.label }}
          </span>
        </div>
      </div>
      <div v-if="!roleTree.length" class="text-ink-3 py-3 text-center">暂无角色实例，会话启动后自动创建</div>
      <el-tree v-else :data="roleTree" :props="defaultProps" default-expand-all
               class="!bg-transparent custom-tree" :expand-on-click-node="false">
        <template #default="{ node, data }">
          <div class="flex items-center justify-between w-full pr-1 py-0.5">
            <span class="flex items-center gap-1.5">
              <el-icon :class="data.iconColor" class="text-sm">
                <UserFilled v-if="data.isUser" /><User v-else />
              </el-icon>
              <span :class="data.active ? 'text-ink' : 'text-ink-3'" class="text-xs">{{ node.label }}</span>
              <span v-if="data.activity" class="text-[10px] text-ink-3">{{ data.activity }}</span>
            </span>
            <el-tag v-if="data.status" :type="data.statusType" size="small" effect="plain"
                    class="scale-75 origin-right">
              {{ data.status === 'delivered-unverified' ? '已交付未验证' : data.status }}
            </el-tag>
          </div>
        </template>
      </el-tree>
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
