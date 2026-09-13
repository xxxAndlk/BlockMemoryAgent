<script setup lang="ts">
import { ref, computed } from 'vue'
import type { SessionEvent } from '@/types'
import { fmtTime, kindTagType, kindIcon, kindLabel, agentTextColor, hasDetail } from '../utils/eventStyles'
import { renderMd } from '@/utils/markdown'

const props = defineProps<{
  events: SessionEvent[]
  verbose?: boolean
}>()

const collapsed = ref(false)
const expanded = ref<Set<number>>(new Set())

function toggle(i: number) {
  // 重新赋值新 Set 触发 ref 响应性（F5 修复）。
  // Vue 3 ref 追踪值重新赋值，不追踪 Set 内部 add/delete 变更，
  // 原地改 expanded.value 后模板中 expanded.has(i) 不会重新求值，按钮无视觉反馈。
  const next = new Set(expanded.value)
  if (next.has(i)) next.delete(i)
  else next.add(i)
  expanded.value = next
}

// **盒里只留模型自己的推理**（think/intend）：说给用户的话（llm_result 子 Agent 结果摘要、
// message 中继文本）按正文展示，prompt/token_usage/子Agent完成 这类管线细节折叠成
// 「已折叠 +N」（verbose / 监控页可查）。此前整包显示，交付结论之类的输出会冒充思考
// （2026-09-13 用户实证）。
const visible = computed(() => {
  if (props.verbose) return props.events
  return props.events.filter(ev => {
    const k = ev.kind || ev.type
    return k === 'think' || k === 'intend'
  })
})

const hiddenCount = computed(() => props.events.length - visible.value.length)

// LLM 产出的思考/回复文本走 markdown 渲染（列表/代码块/加粗等格式保留）；
// 非文本类事件（prompt 摘要等调试内容）保持纯文本避免 markdown 错位排版。
const MD_KINDS = new Set(['think', 'intend', 'llm', 'llm_result', 'llm_response', 'wait', 'agent_done', 'clarify'])
function useMd(ev: SessionEvent): boolean {
  return MD_KINDS.has(ev.kind || ev.type || '')
}
function asText(s: string | undefined): string {
  return s || ''
}
</script>

<template>
  <!-- 简洁模式下 prompt/token_usage 这类调试事件会被折叠掉；若一段思考里**只剩**这类
       事件，面板会渲染成"思考链路 0 步（已折叠 +1）"的空壳（2026-09-12 用户实证），
       既没内容又让人以为出了 bug——没有可视步骤就不出这块。 -->
  <div v-if="visible.length" class="rounded-lg border border-line bg-page my-2">
    <button class="w-full text-left px-3 py-2 flex items-center justify-between hover:bg-card transition-colors"
            @click="collapsed = !collapsed">
      <span class="flex items-center gap-2 text-xs text-ink">
        <el-icon class="text-blue-400"><ChatLineRound /></el-icon>
        <span class="font-medium">思考链路</span>
        <span class="text-ink-2">{{ visible.length }} 步</span>
        <span v-if="!verbose && hiddenCount > 0" class="text-ink-2">（已折叠 +{{ hiddenCount }}）</span>
      </span>
      <el-icon class="text-ink-2 transition-transform" :class="{'rotate-180': !collapsed}"><ArrowDown /></el-icon>
    </button>

    <!-- 链内步骤累计展示后条数不限（任务 155 起全部保留）：内容区定高滚动，
         面板不再随链条长度无限增高（2026-09-13 用户诉求：思考用固定高度+滚动，
         不然占的空间太大）。折叠按钮不受影响。 -->
    <div v-show="!collapsed" class="chain-body px-3 pb-3 space-y-2">
      <div v-for="(ev, i) in visible" :key="i" class="text-xs">
        <div class="flex items-start gap-2">
          <div class="w-12 shrink-0 text-ink-2 pt-0.5">{{ fmtTime(ev.timestamp) }}</div>
          <el-tag size="small" :type="kindTagType(ev.kind, ev.type)" effect="plain"
                  class="!bg-transparent !border-line shrink-0 scale-90 origin-left">
            <el-icon class="mr-0.5 text-[10px]"><component :is="kindIcon(ev.kind, ev.type)" /></el-icon>
            {{ kindLabel(ev.kind, ev.type) }}
          </el-tag>
          <span class="text-[11px] shrink-0" :class="agentTextColor(ev.agent)">{{ ev.agent }}</span>
          <!-- 思考链正文一律灰色（text-ink-2）：思考是过程，不该与最终结论抢视线
               （2026-09-13 用户诉求：模型思考=灰色小字，输出结论=大字）。 -->
          <div class="flex-1 min-w-0">
            <div v-if="useMd(ev)" class="markdown-body text-ink-2 break-words leading-relaxed"
                 v-html="renderMd(ev.message)"></div>
            <div v-else class="text-ink-2 break-words leading-relaxed whitespace-pre-wrap">{{ asText(ev.message) }}</div>
            <button v-if="hasDetail(ev)"
                    class="text-[10px] text-ink-2 hover:text-ink mt-1 flex items-center gap-1"
                    @click="toggle(i)">
              <el-icon class="text-[10px]"><component :is="expanded.has(i) ? 'ArrowUp' : 'ArrowDown'" /></el-icon>
              {{ expanded.has(i) ? '收起详情' : '展开详情' }}
            </button>
            <div v-if="expanded.has(i)" class="mt-2 space-y-1.5 pl-2 border-l-2 border-line">
              <div v-if="ev.prompt">
                <div class="text-[10px] text-ink-2 mb-0.5">Prompt 摘要</div>
                <pre class="bg-page p-2 rounded text-[11px] text-ink-2 whitespace-pre-wrap font-mono">{{ ev.prompt }}</pre>
              </div>
              <div v-if="ev.detail_json">
                <div class="text-[10px] text-ink-2 mb-0.5">结构化详情</div>
                <pre class="bg-page p-2 rounded text-[11px] text-ink-2 whitespace-pre-wrap font-mono">{{ ev.detail_json }}</pre>
              </div>
              <div v-if="ev.tool_output">
                <div class="text-[10px] text-ink-2 mb-0.5">工具输出</div>
                <pre class="bg-page p-2 rounded text-[11px] text-ink-2 whitespace-pre-wrap font-mono">{{ ev.tool_output }}</pre>
              </div>
              <div v-if="ev.tool_error">
                <div class="text-[10px] text-ink-2 mb-0.5">错误</div>
                <pre class="bg-page p-2 rounded text-[11px] text-red-400 whitespace-pre-wrap font-mono">{{ ev.tool_error }}</pre>
              </div>
              <div v-if="ev.input_tokens || ev.output_tokens" class="text-[10px] text-ink-2">
                Token: in={{ ev.input_tokens || 0 }} · out={{ ev.output_tokens || 0 }}
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
/* 思考面板内容区定高：链条长了（12 步起步）整块思考能占掉半屏，收进固定高度的
   内部滚动窗；滚到底不把整页带着滚。 */
.chain-body {
  max-height: 280px;
  overflow-y: auto;
  overscroll-behavior: contain;
}
.markdown-body :deep(p) { margin: 0 0 0.4em 0; }
.markdown-body :deep(p:last-child) { margin-bottom: 0; }
.markdown-body :deep(ul), .markdown-body :deep(ol) { margin: 0.3em 0 0.3em 1.2em; }
.markdown-body :deep(pre) {
  background: var(--bma-page);
  padding: 8px;
  border-radius: 4px;
  margin: 4px 0;
  overflow-x: auto;
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(h1), .markdown-body :deep(h2), .markdown-body :deep(h3), .markdown-body :deep(h4) {
  margin: 0.5em 0 0.25em;
  font-weight: 600;
}
</style>
