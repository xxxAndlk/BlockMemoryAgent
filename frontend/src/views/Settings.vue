<script setup lang="ts">
import { ref } from 'vue'
const soul = ref('# Soul 配置\n\n## 人格\n你是一个严谨、精确的 AI 助手。\n\n## 沟通风格\n- 简洁直接\n- 技术准确性优先\n- 断言前必验证')
const temp = ref(30)
function save() { alert('保存 API 待实现') }
</script>

<template>
  <div class="layout">
    <div class="section">
      <h3>Soul 配置</h3>
      <textarea v-model="soul" rows="12"></textarea>
      <div class="actions"><button class="btn" @click="save">保存</button></div>
    </div>
    <div class="section">
      <h3>Temperature</h3>
      <div class="slider">
        <input type="range" v-model.number="temp" min="0" max="100">
        <span class="val">{{ (temp/100).toFixed(2) }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.layout { max-width: 600px; display: flex; flex-direction: column; gap: 16px; }
.section { background: #161f2e; border: 1px solid #243447; border-radius: 8px; padding: 16px; }
.section h3 { font-size: 14px; font-weight: 700; margin-bottom: 12px; }
textarea { width: 100%; background: #1a2332; border: 1px solid #243447; border-radius: 4px; padding: 10px; color: #e2e8f0; font-family: 'JetBrains Mono', monospace; font-size: 12px; resize: vertical; }
.actions { margin-top: 12px; display: flex; justify-content: flex-end; }
.btn { background: #2563eb; color: white; border: none; border-radius: 4px; padding: 7px 16px; font-size: 12px; cursor: pointer; }
.slider { display: flex; align-items: center; gap: 12px; }
.slider input { flex: 1; }
.val { font-size: 13px; font-weight: 600; color: #3b82f6; min-width: 30px; }
</style>
