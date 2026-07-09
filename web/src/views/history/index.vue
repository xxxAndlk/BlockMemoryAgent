<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-gray-300">
    <div class="shrink-0 mb-2">
      <h1 class="text-xl font-bold text-gray-200 mb-1">Session History / 会话历史</h1>
      <p class="text-sm text-gray-500">查看历史会话记录，回顾执行过程和结果</p>
    </div>
    <FeaturePlaceholder
      icon="Clock"
      title="会话历史功能开发中"
      description="历史会话归档、回放与导出能力将在后端 API 就绪后提供，当前不再展示示例数据。"
    />
  </div>
</template>

<script setup lang="ts">
import FeaturePlaceholder from '@/components/FeaturePlaceholder.vue'
</script>
