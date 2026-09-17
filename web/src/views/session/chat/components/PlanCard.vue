<script setup lang="ts">
// PlanCard.vue 计划确认卡：submit_plan / review_plan 单列展示（不随工具活动折叠）。
// 一张卡=一次计划提交：任务简介 + 计划正文（markdown）+ 审批结论（批准/驳回意见/审批中）。
// 入参解析与结论判定见 utils/plan.ts（主对话与子 Agent 面板共用）。
import { computed, ref } from 'vue'
import type { PlanStep } from '../utils/plan'
import { renderMd } from '@/utils/markdown'
import { fmtTime } from '../utils/eventStyles'

const props = defineProps<{ plan: PlanStep }>()

/** 计划正文默认展开（它就是这张卡的意义），过长时可折；超过 1200 字默认折叠。 */
const expanded = ref((props.plan.planText || '').length <= 1200)

const isReview = computed(() => props.plan.tool === 'review_plan')
const title = computed(() => (isReview.value ? '审批计划' : '提交计划'))

const verdictLabel = computed(() => {
  switch (props.plan.verdict) {
    case 'pending': return isReview.value ? '审批中…' : '等待审批…'
    case 'approved': return '已批准'
    case 'rejected': return '已驳回'
    default: return props.plan.success ? '已返回' : '失败'
  }
})

const verdictColor = computed(() => {
  switch (props.plan.verdict) {
    case 'pending': return 'text-blue-400'
    case 'approved': return 'text-green-500'
    case 'rejected': return 'text-red-400'
    default: return props.plan.success ? 'text-ink-2' : 'text-red-400'
  }
})

const accent = computed(() => {
  switch (props.plan.verdict) {
    case 'pending': return 'border-l-blue-400'
    case 'approved': return 'border-l-green-500'
    case 'rejected': return 'border-l-red-400'
    default: return 'border-l-line'
  }
})

const planHtml = computed(() => renderMd(props.plan.planText || ''))
</script>

<template>
  <div class="rounded-lg border border-line border-l-2 bg-card my-2 overflow-hidden" :class="accent">
    <button class="w-full text-left px-3 py-2 flex items-center gap-2 hover:bg-page transition-colors"
            @click="expanded = !expanded">
      <el-icon class="text-amber-500 shrink-0"><Tickets /></el-icon>
      <span class="text-xs font-medium text-ink shrink-0">{{ title }}</span>
      <span class="text-[11px] shrink-0 flex items-center gap-1" :class="verdictColor">
        <el-icon v-if="plan.verdict === 'pending'" class="is-loading text-[10px]"><Loading /></el-icon>
        {{ verdictLabel }}
      </span>
      <span v-if="plan.agent" class="text-[10px] text-ink-3 shrink-0">{{ plan.agent }}</span>
      <span class="text-[10px] text-ink-2 ml-auto shrink-0">{{ fmtTime(plan.ts) }}</span>
      <el-icon class="text-ink-2 shrink-0 transition-transform" :class="{ 'rotate-180': expanded }"><ArrowDown /></el-icon>
    </button>

    <div v-show="expanded" class="px-3 pb-3 pt-1 space-y-2">
      <!-- 任务简介（一句话）：计划正文可能很长，先给"这是干什么的" -->
      <div v-if="plan.taskSummary" class="text-xs text-ink-2 flex gap-1.5">
        <span class="text-ink-3 shrink-0">任务</span>
        <span class="min-w-0">{{ plan.taskSummary }}</span>
      </div>

      <div v-if="plan.planText">
        <div class="text-[10px] text-ink-3 mb-1">计划正文</div>
        <div class="md-article" v-html="planHtml"></div>
      </div>

      <!-- 驳回意见：审批结论里最该被看见的一行，单独提出去 -->
      <div v-if="plan.feedback"
           class="rounded border border-red-200 bg-red-50 dark:border-red-700/40 dark:bg-red-900/20 px-2 py-1.5 text-[11px] text-red-600 dark:text-red-300">
        <span class="font-medium mr-1">修改意见</span>{{ plan.feedback }}
      </div>

      <div v-if="plan.resultText && !plan.feedback">
        <div class="text-[10px] text-ink-3 mb-1">审批结论</div>
        <pre class="bg-page p-2 rounded text-[11px] text-ink-2 whitespace-pre-wrap font-mono max-h-40 overflow-auto">{{ plan.resultText }}</pre>
      </div>
    </div>
  </div>
</template>
