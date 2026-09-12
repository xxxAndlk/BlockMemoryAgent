<script setup lang="ts">
// OrchMiniCanvas.vue 任务看板内嵌的横向滑动迷你编排画布：
// 复用 useTreeLayout 布局 + SVG 贝塞尔连线 + 紧凑节点卡（样式简化自 AgentTreeNode），
// 只读无缩放/平移；点击节点选中该 Agent（?agent=<id>，回到对话 tab），当前选中高亮。
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { LocationQuery } from 'vue-router'
import type { AgentNode } from '@/types'
import { useTreeLayout, isChildWaiting, NODE_W, NODE_H, V_GAP } from '@/composables/useTreeLayout'
import type { LayoutEdge, LayoutNode } from '@/composables/useTreeLayout'

const props = defineProps<{ agents: AgentNode[] }>()

const route = useRoute()
const router = useRouter()

const layout = useTreeLayout(computed(() => props.agents))
const roots = layout.roots
const edges = layout.edges
const width = layout.width
const height = layout.height

/** 当前选中的 Agent（?agent=），用于节点高亮。 */
const selectedId = computed(() => (route.query.agent as string) || '')

/** 点击节点：只设 ?agent= 选中该 Agent（移除 query 里的 view，回到对话 tab），对话/监控随选中切换。 */
function handleSelect(id: string) {
  const q: LocationQuery = { ...route.query, agent: id }
  delete q.view
  void router.replace({ query: q })
}

// 连线按子节点状态着色：done 绿 / failed 红 / running 主色 / 其余 line 色。
const statusById = computed(() => {
  const m = new Map<string, string>()
  for (const n of roots.value) m.set(n.id, n.status)
  return m
})

function edgeStrokeClass(to: string): string {
  switch (statusById.value.get(to) ?? '') {
    case 'done':
      return 'stroke-green-500'
    case 'failed':
      return 'stroke-red-500'
    case 'running':
      return 'stroke-primary'
    default:
      return 'stroke-line'
  }
}

// 三次贝塞尔：控制点取垂直落差半程，得到平滑 S 形（父底边中点 → 子顶边中点）。
function edgePath(e: LayoutEdge): string {
  const c = V_GAP / 2
  return `M ${e.x1} ${e.y1} C ${e.x1} ${e.y1 + c}, ${e.x2} ${e.y2 - c}, ${e.x2} ${e.y2}`
}

// 状态点配色对齐看板/树图口径：done 绿、running 黄脉冲、failed 红、
// cancelled/paused 灰、已交付未验证橙；child_wait（等下级返回）用蓝脉冲区分。
function dotClass(n: LayoutNode): string {
  if (isChildWaiting(n)) return 'bg-blue-400 animate-pulse'
  switch (n.status) {
    case 'done':
      return 'bg-green-500'
    case 'active':
    case 'running':
      return 'bg-yellow-400 animate-pulse'
    case 'failed':
      return 'bg-red-500'
    case 'delivered-unverified':
      return 'bg-orange-400'
    default:
      return 'bg-gray-400'
  }
}

const STATUS_TEXT: Record<string, string> = {
  running: '运行中',
  done: '完成',
  failed: '失败',
  cancelled: '已取消',
  paused: '已暂停',
  'delivered-unverified': '已交付未验证',
  idle: '热驻',
  active: '活跃',
}

function statusText(n: LayoutNode): string {
  return STATUS_TEXT[n.status] ?? n.status
}
</script>

<template>
  <div v-if="!roots.length" class="text-center text-[11px] text-ink-3 py-3">
    暂无编排，等待 MetaAgent 派发
  </div>
  <!-- 横向滑动画布：内层宽度=布局宽，超高截断（面板内限高） -->
  <div v-else class="overflow-x-auto overflow-y-hidden max-h-64">
    <div class="relative" :style="{ width: `${width}px`, height: `${height}px` }">
      <!-- 连线层：pointer-events-none，绝不吃节点卡的点击 -->
      <svg class="absolute left-0 top-0 pointer-events-none" :width="width" :height="height">
        <path
          v-for="e in edges"
          :key="`${e.from}->${e.to}`"
          :d="edgePath(e)"
          fill="none"
          stroke-width="1.5"
          :class="edgeStrokeClass(e.to)"
        />
      </svg>
      <!-- 紧凑节点卡：状态点 + 名称 + 状态徽章 + 活动小字 -->
      <div
        v-for="n in roots"
        :key="n.id"
        class="absolute flex flex-col justify-center gap-1 px-2.5 bg-card border rounded-card shadow-card hover:shadow-md cursor-pointer select-none transition-shadow"
        :class="n.id === selectedId ? 'ring-2 ring-primary border-primary' : 'border-line'"
        :style="{ left: `${n.x}px`, top: `${n.y}px`, width: `${NODE_W}px`, height: `${NODE_H}px` }"
        @click="handleSelect(n.id)"
      >
        <div class="flex items-center gap-1.5">
          <span class="w-2 h-2 rounded-full shrink-0" :class="dotClass(n)"></span>
          <span class="flex-1 text-xs font-bold text-ink truncate" :title="n.name">{{ n.name }}</span>
          <span class="shrink-0 px-1 text-[10px] leading-4 bg-page border border-line rounded text-ink-3">
            {{ statusText(n) }}
          </span>
        </div>
        <div class="h-4 text-[10px] leading-4 text-ink-3 truncate" :title="n.activityText">
          {{ n.activityText }}
        </div>
      </div>
    </div>
  </div>
</template>
