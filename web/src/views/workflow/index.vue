<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4 flex-wrap gap-3">
      <div>
        <h2 class="text-lg font-bold text-ink">工作流编排</h2>
        <p class="text-xs text-ink-2 mt-1">
          打样定标 → 配方（<code>.bma/workflows/*.yaml</code>）→ 有界单位流水线批量产出；配方库与运行记录都在这。
        </p>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-xs text-ink-2 shrink-0">工作目录</span>
        <div class="w-[320px] shrink-0">
          <el-input v-model="workdir" size="small" placeholder="配方所在工作目录绝对路径" clearable class="!w-full"
                    @change="persistWorkdir" />
        </div>
        <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="refreshKey++">
          <el-icon class="mr-1"><Refresh /></el-icon>刷新
        </el-button>
      </div>
    </div>

    <el-tabs v-model="tab">
      <el-tab-pane label="配方库" name="recipes">
        <RecipeList :workdir="workdir" :refresh-key="refreshKey" />
      </el-tab-pane>
      <el-tab-pane label="运行列表" name="runs">
        <RunList :refresh-key="refreshKey" />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import RecipeList from './RecipeList.vue'
import RunList from './RunList.vue'

const route = useRoute()
const router = useRouter()

const tab = ref<'recipes' | 'runs'>((route.query.tab as string) === 'runs' ? 'runs' : 'recipes')
const refreshKey = ref(0)

// 工作目录：?workdir= > localStorage 记忆 > 空（空时配方库提示先选目录）。
const stored = typeof localStorage !== 'undefined' ? localStorage.getItem('bma:workflow-workdir') || '' : ''
const workdir = ref((route.query.workdir as string) || stored)

function persistWorkdir() {
  if (typeof localStorage !== 'undefined') localStorage.setItem('bma:workflow-workdir', workdir.value)
  router.replace({ query: { ...route.query, workdir: workdir.value || undefined } })
}
</script>
