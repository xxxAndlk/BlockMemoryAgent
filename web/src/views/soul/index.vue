<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { getStatus } from '@/api/health'

const soul = ref('default')
const llm = ref('')

onMounted(async () => {
  try {
    const s = await getStatus()
    soul.value = s.soul || 'default'
    llm.value = `${s.llm_provider || ''} / ${s.llm_model || ''}`
  } catch {
    // ignore
  }
})
</script>

<template>
  <div class="h-full overflow-y-auto text-ink">
    <div class="max-w-2xl mx-auto flex flex-col items-center text-center pt-16">
      <div class="rounded-full bg-primary-soft p-5 mb-4">
        <el-icon class="text-5xl text-primary"><User /></el-icon>
      </div>
      <div class="text-lg font-bold mb-1">人格配置</div>
      <div class="text-sm text-ink-2 mb-6">当前人格决定了 Agent 的语气、价值取向与决策风格。</div>

      <div class="w-full bg-card border border-line rounded-card p-4 text-left text-sm space-y-2">
        <div class="flex justify-between">
          <span class="text-ink-2">当前人格</span>
          <span class="font-bold">{{ soul }}</span>
        </div>
        <div class="flex justify-between">
          <span class="text-ink-2">LLM</span>
          <span class="font-mono text-xs">{{ llm || '-' }}</span>
        </div>
        <div class="flex justify-between">
          <span class="text-ink-2">定义文件</span>
          <span class="font-mono text-xs">config/soul.md</span>
        </div>
      </div>

      <div class="w-full mt-4 bg-primary-soft border border-line rounded-card p-4 text-xs text-ink-2 text-left">
        人格的热切换与在线编辑能力规划中。当前修改方式：编辑安装目录下的
        <span class="font-mono">config/soul.md</span> 后重启后端生效。
      </div>
    </div>
  </div>
</template>
