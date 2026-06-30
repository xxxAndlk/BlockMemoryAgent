<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-gray-300">
    <!-- Header Area -->
    <div class="shrink-0 mb-2">
      <h1 class="text-xl font-bold text-gray-200 mb-1">Knowledge Base / 知识库搜索</h1>
      <p class="text-sm text-gray-500">基于语义搜索的全局知识库，支持查看、归档和管理知识条目</p>
    </div>

    <!-- Search Box -->
    <el-card class="!border-[#2a2d35] !bg-[#1a1d24] shrink-0">
      <div class="font-bold text-sm text-gray-200 mb-4">语义搜索</div>
      <div class="flex gap-3">
        <el-input
          v-model="searchQuery"
          size="large"
          placeholder="输入关键词搜索知识库…"
          class="flex-1 !bg-[#0f1115] search-input"
          @keydown.enter="doSearch"
        >
          <template #prefix><el-icon class="text-gray-500"><Search /></el-icon></template>
        </el-input>
        <el-button type="primary" size="large" class="!bg-primary px-6" :loading="searching" @click="doSearch">
          <el-icon class="mr-1"><Search /></el-icon> 搜索
        </el-button>
      </div>
    </el-card>

    <!-- Main Content Area -->
    <div class="flex-1 flex items-center justify-center min-h-0">
      <div class="text-center">
        <el-icon class="text-5xl text-gray-600 mb-4"><FolderOpened /></el-icon>
        <div class="text-lg text-gray-400 mb-2">知识库功能开发中</div>
        <div class="text-sm text-gray-500">知识库 API 接入后，此处将展示语义搜索结果、分类筛选和归档管理。</div>
        <div v-if="searchQuery" class="text-xs text-gray-600 mt-3">搜索词"{{ searchQuery }}"的语义检索将在后端 API 就绪后可用。</div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const searchQuery = ref('')
const searching = ref(false)

function doSearch() {
  if (!searchQuery.value.trim()) return
  searching.value = true
  setTimeout(() => { searching.value = false }, 600)
}
</script>

<style scoped>
:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}
</style>
