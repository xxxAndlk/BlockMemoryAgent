<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4 flex-wrap gap-3">
      <div>
        <h2 class="text-lg font-bold text-ink">流程编排</h2>
        <p class="text-xs text-ink-2 mt-1">
          节点图工作流（<code>.bma/flows/*.json</code>）——start / llm / tool / branch / approval / answer
          拖拽连线编排，可试运行、按节点追踪。
        </p>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-xs text-ink-2 shrink-0">工作目录</span>
        <div class="w-[320px] shrink-0">
          <el-input v-model="workdir" size="small" placeholder="流程所在工作目录绝对路径" clearable class="!w-full"
                    @change="persistWorkdir" />
        </div>
        <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="refreshKey++">
          <el-icon class="mr-1"><Refresh /></el-icon>刷新
        </el-button>
      </div>
    </div>

    <el-tabs v-model="tab">
      <el-tab-pane label="流程库" name="flows">
        <FlowList :workdir="workdir" :refresh-key="refreshKey" />
      </el-tab-pane>
      <el-tab-pane label="运行列表" name="runs">
        <FlowRunList :refresh-key="refreshKey" />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import FlowList from './FlowList.vue'
import FlowRunList from './RunList.vue'

const route = useRoute()
const router = useRouter()

const tab = ref<'flows' | 'runs'>((route.query.tab as string) === 'runs' ? 'runs' : 'flows')
const refreshKey = ref(0)

// 工作目录：?workdir= > localStorage 记忆 > 空（空时流程库提示先选目录）。
const stored = typeof localStorage !== 'undefined' ? localStorage.getItem('bma:flow-workdir') || '' : ''
const workdir = ref((route.query.workdir as string) || stored)

function persistWorkdir() {
  if (typeof localStorage !== 'undefined') localStorage.setItem('bma:flow-workdir', workdir.value)
  router.replace({ query: { ...route.query, workdir: workdir.value || undefined } })
}
</script>
