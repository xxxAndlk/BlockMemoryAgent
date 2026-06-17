<script setup lang="ts">
import { ref } from 'vue'
const tab = ref('snapshot')
</script>

<template>
  <div class="layout">
    <div class="left">
      <div class="panel-header"><h3>选择智能体</h3></div>
      <div class="list"><div class="item active"><span>◆</span> MetaAgent</div></div>
    </div>
    <div class="right">
      <div class="panel-header"><h3>记忆快照</h3></div>
      <div class="tabs">
        <button :class="{active:tab==='snapshot'}" @click="tab='snapshot'">快照</button>
        <button :class="{active:tab==='search'}" @click="tab='search'">搜索</button>
        <button :class="{active:tab==='compression'}" @click="tab='compression'">压缩</button>
      </div>
      <div class="content">
        <div v-if="tab==='snapshot'">
          <div class="section"><h4>关键摘要</h4><div class="item">无摘要</div></div>
          <div class="section"><h4>未解决问题</h4><div class="item">无</div></div>
        </div>
        <div v-else-if="tab==='search'">
          <input class="search" placeholder="搜索记忆..." />
          <button class="btn">搜索</button>
        </div>
        <div v-else><div class="item">Raw: 0 / Standard: 0 / Compact: 0 / Marker: 0</div></div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 240px 1fr; gap: 16px; height: 100%; }
.left, .right { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; overflow: hidden; }
.panel-header { padding: 10px 14px; border-bottom: 1px solid #243447; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.list { flex: 1; overflow-y: auto; padding: 8px; }
.item { padding: 8px 10px; border-radius: 4px; margin-bottom: 4px; font-size: 12px; color: #94a3b8; display: flex; align-items: center; gap: 8px; cursor: pointer; }
.item:hover { background: #1e293b; }
.item.active { background: rgba(59,130,246,0.1); color: #3b82f6; }
.tabs { display: flex; gap: 2px; padding: 0 14px; border-bottom: 1px solid #243447; }
.tabs button { background: none; border: none; border-bottom: 2px solid transparent; padding: 8px 14px; color: #64748b; font-size: 12px; cursor: pointer; }
.tabs button.active { color: #3b82f6; border-bottom-color: #3b82f6; }
.content { flex: 1; overflow-y: auto; padding: 12px; }
.section { margin-bottom: 16px; }
.section h4 { font-size: 12px; color: #64748b; margin-bottom: 8px; text-transform: uppercase; }
.search { flex: 1; background: #1a2332; border: 1px solid #243447; border-radius: 4px; padding: 8px 12px; color: #e2e8f0; font-size: 12px; }
.btn { background: #2563eb; color: white; border: none; border-radius: 4px; padding: 8px 16px; font-size: 12px; cursor: pointer; margin-left: 8px; }
</style>
