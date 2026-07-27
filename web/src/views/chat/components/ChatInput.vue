<script setup lang="ts">
import { ref, computed } from 'vue'

const props = defineProps<{
  loading?: boolean
  sessionActive?: boolean
  verbose?: boolean
  memoryEnabled?: boolean
  inputTokens?: number
  outputTokens?: number
}>()

const emit = defineEmits<{
  (e: 'submit', content: string): void
  (e: 'update:verbose', val: boolean): void
  (e: 'update:memoryEnabled', val: boolean): void
  (e: 'new-session'): void
}>()

const content = ref('')

const quickTags = [
  '展开方案细节',
  '给出可运行代码',
  '解释一下原理',
  '列出关键步骤',
  '改成 Go 实现',
  '总结要点',
]

const canSend = computed(() => content.value.trim().length > 0 && !props.loading)

// token 数值格式化：≥1000 显示为 1.2k，否则原样显示。
function fmtTokens(n: number): string {
  if (!n) return '0'
  if (n < 1000) return String(n)
  return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
}

const tokenLabel = computed(() => {
  const input = props.inputTokens || 0
  const output = props.outputTokens || 0
  return `↑ ${fmtTokens(input)}  ↓ ${fmtTokens(output)}`
})

function handleSubmit() {
  if (!canSend.value) return
  emit('submit', content.value.trim())
  content.value = ''
}

function onKeydown(e: KeyboardEvent) {
  // Ctrl/Cmd + Enter 提交
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault()
    handleSubmit()
  }
}

function applyQuickTag(tag: string) {
  if (content.value.trim()) {
    content.value = content.value.trim() + '；' + tag
  } else {
    content.value = tag
  }
}

const verboseModel = computed({
  get: () => !!props.verbose,
  set: (v: boolean) => emit('update:verbose', v),
})
const memoryModel = computed({
  get: () => !!props.memoryEnabled,
  set: (v: boolean) => emit('update:memoryEnabled', v),
})
</script>

<template>
  <div class="border-t border-[#2a2d35] bg-[#14161a] px-6 py-3">
    <!-- 模式开关 + 快捷标签 + Token 计数 -->
    <div class="flex items-center justify-between mb-2 flex-wrap gap-2">
      <div class="flex items-center gap-4 text-xs text-gray-400">
        <label class="flex items-center gap-2 cursor-pointer">
          <el-switch v-model="verboseModel" size="small" />
          <span>详细模式</span>
        </label>
        <label class="flex items-center gap-2 cursor-pointer">
          <el-switch v-model="memoryModel" size="small" />
          <span>启用记忆</span>
        </label>
        <span v-if="sessionActive" class="text-green-400 flex items-center gap-1">
          <span class="w-1.5 h-1.5 rounded-full bg-green-500 animate-pulse"></span>
          会话已连接
        </span>
        <span v-else class="text-gray-500">将创建新会话</span>
      </div>
      <div class="flex items-center gap-3 text-xs">
        <span class="text-gray-500 font-mono" :title="`输入 ${props.inputTokens || 0} / 输出 ${props.outputTokens || 0} tokens`">
          <span class="text-gray-600">Token</span>
          <span class="ml-2 text-blue-400">↑{{ fmtTokens(props.inputTokens || 0) }}</span>
          <span class="ml-1 text-green-400">↓{{ fmtTokens(props.outputTokens || 0) }}</span>
        </span>
        <el-button size="small" plain class="!bg-transparent !border-[#2a2d35] !text-gray-400 hover:!text-white"
                   @click="emit('new-session')">
          <el-icon class="mr-1"><Plus /></el-icon> 新建会话
        </el-button>
      </div>
    </div>

    <div class="flex gap-2 mb-2 flex-wrap">
      <el-tag v-for="t in quickTags" :key="t"
              effect="plain"
              size="small"
              class="!bg-[#0f1115] !border-[#2a2d35] !text-gray-400 hover:!text-white cursor-pointer transition-colors"
              @click="applyQuickTag(t)">
        {{ t }}
      </el-tag>
    </div>

    <!-- 输入区 -->
    <div class="flex gap-2 items-end">
      <div class="flex-1 chat-input">
        <el-input v-model="content"
                  type="textarea"
                  :autosize="{ minRows: 2, maxRows: 6 }"
                  placeholder="向 AI 下达命令…（Ctrl/Cmd + Enter 发送）"
                  resize="none"
                  @keydown="onKeydown" />
      </div>
      <el-button type="primary"
                 :loading="loading"
                 :disabled="!canSend"
                 class="!bg-blue-600 !border-blue-600 hover:!bg-blue-500 h-[68px]! w-24"
                 @click="handleSubmit">
        <el-icon class="mr-1"><Promotion /></el-icon> 发送
        <span class="sr-only">{{ tokenLabel }}</span>
      </el-button>
    </div>
  </div>
</template>

<style scoped>
:deep(.chat-input .el-textarea__inner) {
  background-color: #0f1115 !important;
  border: 1px solid #2a2d35;
  color: #e5e7eb;
  box-shadow: none !important;
  font-size: 13px;
  padding: 10px 12px;
}
:deep(.chat-input .el-textarea__inner:focus) {
  border-color: #3b82f6;
}
</style>
