<script setup lang="ts">
import { ref } from 'vue'
const query = ref('')
const results = ref<{ score: number; content: string }[]>([])

function search() {
  if (!query.value.trim()) return
  results.value = [
    { score: 0.92, content: `Mock result for "${query.value}"` },
  ]
}
</script>

<template>
  <div class="layout">
    <div class="search-box">
      <input v-model="query" placeholder="输入查询进行语义搜索..." @keydown.enter="search" />
      <button class="btn" @click="search">搜索</button>
    </div>
    <div class="results">
      <div v-for="(r,i) in results" :key="i" class="result">
        <div class="score">相似度: {{ r.score }}</div>
        <div class="content">{{ r.content }}</div>
      </div>
      <div v-if="results.length===0" class="empty">输入关键词搜索知识库</div>
    </div>
  </div>
</template>

<style scoped>
.layout { max-width: 800px; }
.search-box { display: flex; gap: 8px; background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 12px; margin-bottom: 12px; }
.search-box input { flex: 1; background: #1a2332; border: 1px solid #243447; border-radius: 4px; padding: 8px 12px; color: #e2e8f0; font-size: 13px; }
.btn { background: #2563eb; color: white; border: none; border-radius: 4px; padding: 8px 16px; font-size: 12px; cursor: pointer; }
.results { display: flex; flex-direction: column; gap: 8px; }
.result { background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 14px; }
.score { font-size: 11px; color: #22c55e; font-weight: 600; margin-bottom: 6px; }
.content { font-size: 13px; color: #94a3b8; }
.empty { padding: 40px; text-align: center; color: #64748b; }
</style>
