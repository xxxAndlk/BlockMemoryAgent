<script setup lang="ts">
import { ref, computed } from 'vue'
import type { ToolCallGroup } from '../utils/turns'
import { toolHeadline } from '../utils/turns'
import { fmtTime } from '../utils/eventStyles'
import ArtifactCard from './ArtifactCard.vue'

const props = defineProps<{ group: ToolCallGroup; sessionId: string }>()

const expanded = ref(false)

const statusLabel = computed(() => {
  if (props.group.pending) return '执行中…'
  return props.group.success ? '执行成功' : '执行失败'
})

const statusColor = computed(() => {
  if (props.group.pending) return 'text-blue-400'
  return props.group.success ? 'text-green-400' : 'text-red-400'
})

const callArgs = computed(() => props.group.call?.detail_json || props.group.call?.tool_args || '')
const resultText = computed(() => props.group.result?.tool_output || props.group.result?.message || '')
const errorText = computed(() => props.group.result?.tool_error || '')
// 折叠态标题行：关键入参摘要（读了哪个文件 / 跑了什么命令）
const headline = computed(() => toolHeadline(props.group))
const startedAt = computed(() => props.group.call?.timestamp || props.group.result?.timestamp || '')
</script>

<template>
  <div class="rounded-lg border border-line bg-page my-2 overflow-hidden">
    <button class="w-full text-left px-3 py-2 flex items-center justify-between hover:bg-card transition-colors"
            @click="expanded = !expanded">
      <span class="flex items-center gap-2 text-xs">
        <el-icon class="text-blue-400 text-base"><Tools /></el-icon>
        <span class="font-mono text-ink">{{ group.tool }}</span>
        <el-tag size="small" effect="plain" class="!bg-transparent !border-line scale-90"
                :class="statusColor">
          <el-icon v-if="group.pending" class="is-loading text-[10px] mr-0.5"><Loading /></el-icon>
          <el-icon v-else-if="group.success" class="text-[10px] mr-0.5"><Check /></el-icon>
          <el-icon v-else class="text-[10px] mr-0.5"><Close /></el-icon>
          {{ statusLabel }}
        </el-tag>
        <span class="text-ink-2 text-[10px]">{{ group.agent }}</span>
        <span v-if="headline" class="text-ink-2 text-[10px] truncate max-w-[280px]" :title="headline">→ {{ headline }}</span>
        <span v-if="group.artifacts?.length" class="text-[10px] px-1 rounded bg-primary-soft text-primary shrink-0"
              :title="`本次产出 ${group.artifacts.length} 个可视成果`">🖼 {{ group.artifacts.length }}</span>
      </span>
      <span class="flex items-center gap-2">
        <span class="text-[10px] text-ink-2">{{ fmtTime(startedAt) }}</span>
        <el-icon class="text-ink-2 transition-transform" :class="{'rotate-180': expanded}"><ArrowDown /></el-icon>
      </span>
    </button>

    <div v-show="expanded" class="px-3 pb-3 space-y-2 border-t border-line pt-2">
      <!-- 本次工具产出的可视成果（效果图/视频/HTML）：就地渲染，免去滚到回合末尾 -->
      <div v-if="group.artifacts?.length" class="space-y-2">
        <div class="text-[10px] text-ink-2">产出（{{ group.artifacts.length }}）</div>
        <ArtifactCard v-for="(a, i) in group.artifacts" :key="'tart-' + i + '-' + a.path"
                      :artifact="a" :session-id="sessionId" />
      </div>
      <div v-if="callArgs">
        <div class="text-[10px] text-ink-2 mb-1">调用参数</div>
        <pre class="bg-page p-2 rounded text-[11px] text-ink whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ callArgs }}</pre>
      </div>
      <div v-if="resultText">
        <div class="text-[10px] text-ink-2 mb-1">执行结果</div>
        <pre class="bg-page p-2 rounded text-[11px] text-ink whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ resultText }}</pre>
      </div>
      <div v-if="errorText">
        <div class="text-[10px] text-ink-2 mb-1">错误信息</div>
        <pre class="bg-page p-2 rounded text-[11px] text-red-400 whitespace-pre-wrap font-mono max-h-64 overflow-auto">{{ errorText }}</pre>
      </div>
      <div v-if="!callArgs && !resultText && !errorText" class="text-[11px] text-ink-2">
        暂无详细信息
      </div>
    </div>
  </div>
</template>
