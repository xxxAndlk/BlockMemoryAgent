<script setup lang="ts">
import { ref, computed } from 'vue'
import type { SessionEvent } from '@/types'
import { fmtTime, kindTagType, kindIcon, kindLabel, agentTextColor, hasDetail } from '../utils/eventStyles'

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

// 简洁模式时折叠 prompt / token_usage / detail_json 详情；可在每条上单独展开
const visible = computed(() => {
  if (props.verbose) return props.events
  return props.events.filter(ev => {
    const k = ev.kind || ev.type
    return k === 'think' || k === 'intend' || k === 'llm' || k === 'llm_result' || k === 'llm_response' || k === 'wait' || k === 'agent_done' || k === 'clarify'
  })
})

const hiddenCount = computed(() => props.events.length - visible.value.length)

// 消息体只展示纯文本：多行混合中英/路径/JSON 的 prompt 摘要若走 markdown
// 会出现错位、乱码式排版。仅最终回答（AssistantTurn）走 markdown。
function asText(s: string | undefined): string {
  return s || ''
}
</script>

<template>
  <div v-if="events.length" class="rounded-lg border border-[#2a2d35] bg-[#0f1115]/60 my-2">
    <button class="w-full text-left px-3 py-2 flex items-center justify-between hover:bg-[#14161a]/80 transition-colors"
            @click="collapsed = !collapsed">
      <span class="flex items-center gap-2 text-xs text-gray-300">
        <el-icon class="text-blue-400"><ChatLineRound /></el-icon>
        <span class="font-medium">思考链路</span>
        <span class="text-gray-500">{{ visible.length }} 步</span>
        <span v-if="!verbose && hiddenCount > 0" class="text-gray-500">（已折叠 +{{ hiddenCount }}）</span>
      </span>
      <el-icon class="text-gray-500 transition-transform" :class="{'rotate-180': !collapsed}"><ArrowDown /></el-icon>
    </button>

    <div v-show="!collapsed" class="px-3 pb-3 space-y-2">
      <div v-for="(ev, i) in visible" :key="i" class="text-xs">
        <div class="flex items-start gap-2">
          <div class="w-12 shrink-0 text-gray-500 pt-0.5">{{ fmtTime(ev.timestamp) }}</div>
          <el-tag size="small" :type="kindTagType(ev.kind, ev.type)" effect="plain"
                  class="!bg-transparent !border-[#2a2d35] shrink-0 scale-90 origin-left">
            <el-icon class="mr-0.5 text-[10px]"><component :is="kindIcon(ev.kind, ev.type)" /></el-icon>
            {{ kindLabel(ev.kind, ev.type) }}
          </el-tag>
          <span class="text-[11px] shrink-0" :class="agentTextColor(ev.agent)">{{ ev.agent }}</span>
          <div class="flex-1 min-w-0">
            <div class="text-gray-300 break-words leading-relaxed whitespace-pre-wrap">{{ asText(ev.message) }}</div>
            <button v-if="hasDetail(ev)"
                    class="text-[10px] text-gray-500 hover:text-gray-300 mt-1 flex items-center gap-1"
                    @click="toggle(i)">
              <el-icon class="text-[10px]"><component :is="expanded.has(i) ? 'ArrowUp' : 'ArrowDown'" /></el-icon>
              {{ expanded.has(i) ? '收起详情' : '展开详情' }}
            </button>
            <div v-if="expanded.has(i)" class="mt-2 space-y-1.5 pl-2 border-l-2 border-[#2a2d35]">
              <div v-if="ev.prompt">
                <div class="text-[10px] text-gray-500 mb-0.5">Prompt 摘要</div>
                <pre class="bg-[#0f1115] p-2 rounded text-[11px] text-gray-400 whitespace-pre-wrap font-mono">{{ ev.prompt }}</pre>
              </div>
              <div v-if="ev.detail_json">
                <div class="text-[10px] text-gray-500 mb-0.5">结构化详情</div>
                <pre class="bg-[#0f1115] p-2 rounded text-[11px] text-gray-400 whitespace-pre-wrap font-mono">{{ ev.detail_json }}</pre>
              </div>
              <div v-if="ev.tool_output">
                <div class="text-[10px] text-gray-500 mb-0.5">工具输出</div>
                <pre class="bg-[#0f1115] p-2 rounded text-[11px] text-gray-400 whitespace-pre-wrap font-mono">{{ ev.tool_output }}</pre>
              </div>
              <div v-if="ev.tool_error">
                <div class="text-[10px] text-gray-500 mb-0.5">错误</div>
                <pre class="bg-[#0f1115] p-2 rounded text-[11px] text-red-400 whitespace-pre-wrap font-mono">{{ ev.tool_error }}</pre>
              </div>
              <div v-if="ev.input_tokens || ev.output_tokens" class="text-[10px] text-gray-500">
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
</style>
