<script setup lang="ts">
const cols = [
  { key: 'pending', label: '待处理' },
  { key: 'in_progress', label: '进行中' },
  { key: 'blocked', label: '阻塞' },
  { key: 'done', label: '已完成' },
  { key: 'failed', label: '失败' },
]
</script>

<template>
  <div class="board">
    <div class="header">
      <div class="title">任务看板</div>
      <span class="badge status-in_progress">进行中</span>
    </div>
    <div class="kanban">
      <div v-for="c in cols" :key="c.key" class="col">
        <div class="col-header">{{ c.label }}</div>
        <div class="col-items">
          <div class="empty">空</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.board { display: flex; flex-direction: column; height: 100%; }
.header { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
.title { font-size: 16px; font-weight: 700; }
.kanban { display: grid; grid-template-columns: repeat(5, 1fr); gap: 12px; flex: 1; }
.col { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; }
.col-header { padding: 10px 12px; font-size: 12px; font-weight: 700; color: #94a3b8; border-bottom: 1px solid #243447; }
.col-items { flex: 1; overflow-y: auto; padding: 8px; }
.empty { padding: 20px 0; text-align: center; font-size: 11px; color: #64748b; }
</style>
