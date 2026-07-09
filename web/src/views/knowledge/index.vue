<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-gray-300">
    <div class="shrink-0 mb-2">
      <h1 class="text-xl font-bold text-gray-200 mb-1">Knowledge Base / 知识库搜索</h1>
      <p class="text-sm text-gray-500">基于语义搜索的全局知识库，支持查看、归档和管理知识条目</p>
    </div>
    <FeaturePlaceholder
      icon="FolderOpened"
      title="知识库功能开发中"
      description="语义检索、分类筛选与归档管理将在后端 API 就绪后提供。"
    />
  </div>
</template>

<script setup lang="ts">
import FeaturePlaceholder from '@/components/FeaturePlaceholder.vue'
</script>
