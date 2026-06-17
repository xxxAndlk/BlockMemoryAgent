<script setup lang="ts">
defineProps<{ title: string }>()
const emit = defineEmits<{ (e: 'create', goal: string): void }>()

let goal = ''
function submit() {
  if (!goal.trim()) return
  emit('create', goal.trim())
  goal = ''
}
</script>

<template>
  <header class="topbar">
    <div class="left">
      <div class="breadcrumb">BlockMemoryAgent / {{ title }}</div>
      <h1>{{ title }}</h1>
    </div>
    <div class="right">
      <input
        v-model="goal"
        class="search"
        placeholder="输入任务目标，按回车执行..."
        @keydown.enter="submit"
      />
      <button class="btn" @click="submit">运行</button>
    </div>
  </header>
</template>

<style scoped>
.topbar {
  height: 52px; background: #111827; border-bottom: 1px solid #243447;
  display: flex; align-items: center; justify-content: space-between;
  padding: 0 20px; flex-shrink: 0;
}
.left { display: flex; flex-direction: column; gap: 2px; }
.breadcrumb { font-size: 11px; color: #64748b; }
h1 { font-size: 16px; font-weight: 700; color: #e2e8f0; margin: 0; }
.right { display: flex; gap: 8px; align-items: center; }
.search {
  width: 320px; background: #1a2332; border: 1px solid #243447;
  border-radius: 6px; padding: 6px 12px; color: #e2e8f0;
  font-size: 12px; outline: none;
}
.search::placeholder { color: #64748b; }
.btn {
  background: #2563eb; color: white; border: none;
  border-radius: 6px; padding: 6px 16px; font-size: 12px;
  font-weight: 600; cursor: pointer;
}
.btn:hover { background: #3b82f6; }
</style>
