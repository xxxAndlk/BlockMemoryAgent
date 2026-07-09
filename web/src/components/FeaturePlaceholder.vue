<script setup lang="ts">
import AppCard from './AppCard.vue'

const props = defineProps<{
  icon: string
  title: string
  description?: string
}>()
</script>

<template>
  <AppCard class="h-full">
    <div class="h-full flex flex-col items-center justify-center text-gray-400 text-center p-8">
      <div class="rounded-full bg-[#1e3a8a]/20 p-4 mb-4">
        <el-icon class="text-5xl text-primary"><component :is="props.icon" /></el-icon>
      </div>
      <div class="text-lg font-bold text-gray-200 mb-2">{{ props.title }}</div>
      <div v-if="props.description" class="text-sm text-gray-500 max-w-md">{{ props.description }}</div>
    </div>
  </AppCard>
</template>
