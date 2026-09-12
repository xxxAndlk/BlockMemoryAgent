<script setup lang="ts">
import { ref, computed } from 'vue'
import type { ToolCallGroup } from '../utils/turns'
import { toolHeadline } from '../utils/turns'
import ToolCallCard from './ToolCallCard.vue'

// Codex 式工具活动展示：运行中只保留一行「调用 xxx 中 → 成功/失败」就地替换，
// 不逐条留存；回合结束后折叠为汇总 pill，点开可看全部调用详情（监控视图有全量日志）。
const props = defineProps<{
  groups: ToolCallGroup[]
  running: boolean
  /** 会话 ID：透传给工具卡里的媒体卡片（生成工作区文件 URL 用） */
  sessionId: string
}>()

const expanded = ref(false)

const pending = computed(() => props.groups.find((g) => g.pending))
const last = computed(() => props.groups[props.groups.length - 1])
const failCount = computed(() => props.groups.filter((g) => !g.pending && !g.success).length)
// 运行中的活动行：优先展示进行中的调用，否则展示最近一次结果（下一轮 LLM 思考期间）
const liveGroup = computed(() => pending.value || (props.running ? last.value : undefined))
</script>

<template>
  <div class="my-2">
    <!-- 运行中：单行就地替换 -->
    <div v-if="liveGroup" class="flex items-center gap-2 text-xs text-ink-2 px-1 py-1 min-w-0">
      <template v-if="liveGroup.pending">
        <el-icon class="is-loading text-blue-400 shrink-0"><Loading /></el-icon>
        <span class="shrink-0">调用 <span class="font-mono text-ink">{{ liveGroup.tool }}</span> 中</span>
      </template>
      <template v-else>
        <el-icon v-if="liveGroup.success" class="text-green-400 shrink-0"><Check /></el-icon>
        <el-icon v-else class="text-red-400 shrink-0"><Close /></el-icon>
        <span class="shrink-0">
          <span class="font-mono text-ink">{{ liveGroup.tool }}</span>
          {{ liveGroup.success ? '执行成功' : '执行失败' }}
        </span>
      </template>
      <span v-if="toolHeadline(liveGroup)" class="text-ink-3 text-[10px] truncate min-w-0"
            :title="toolHeadline(liveGroup)">{{ toolHeadline(liveGroup) }}</span>
    </div>

    <!-- 结束后：折叠汇总，点开看全部调用 -->
    <div v-else-if="groups.length" class="rounded-lg border border-line bg-page overflow-hidden">
      <button class="w-full text-left px-3 py-1.5 flex items-center gap-2 text-xs hover:bg-card transition-colors"
              @click="expanded = !expanded">
        <el-icon :class="failCount ? 'text-yellow-400' : 'text-green-400'" class="shrink-0">
          <WarningFilled v-if="failCount" /><Check v-else />
        </el-icon>
        <span class="text-ink">已执行 {{ groups.length }} 次工具</span>
        <span v-if="failCount" class="text-red-400">· {{ failCount }} 次失败</span>
        <el-icon class="ml-auto text-ink-2 transition-transform" :class="{ 'rotate-180': expanded }"><ArrowDown /></el-icon>
      </button>
      <div v-show="expanded" class="px-2 pb-2">
        <ToolCallCard v-for="g in groups" :key="g.id" :group="g" :session-id="sessionId" />
      </div>
    </div>
  </div>
</template>
