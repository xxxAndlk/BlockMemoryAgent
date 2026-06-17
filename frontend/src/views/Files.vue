<script setup lang="ts">
import { ref } from 'vue'
const files = ref([
  { name: 'test/snake.py', size: '3.5 KB' },
])
const preview = ref('')
const path = ref('')

function select(f: { name: string }) {
  path.value = f.name
  fetch('/static/' + f.name).then(r => r.text()).then(t => preview.value = t).catch(() => preview.value = '无法加载')
}
</script>

<template>
  <div class="layout">
    <div class="left">
      <div class="panel-header"><h3>会话文件</h3></div>
      <div class="list">
        <div v-for="f in files" :key="f.name" class="item" :class="{active: path===f.name}" @click="select(f)">
          <span>📄</span><span class="name">{{ f.name }}</span><span class="size">{{ f.size }}</span>
        </div>
      </div>
    </div>
    <div class="right">
      <div class="panel-header"><h3>预览</h3><span class="path">{{ path }}</span></div>
      <pre class="preview"><code>{{ preview || '选择文件预览' }}</code></pre>
    </div>
  </div>
</template>

<style scoped>
.layout { display: grid; grid-template-columns: 260px 1fr; gap: 16px; height: 100%; }
.left, .right { background: #161f2e; border: 1px solid #243447; border-radius: 8px; display: flex; flex-direction: column; overflow: hidden; }
.panel-header { padding: 10px 14px; border-bottom: 1px solid #243447; display: flex; justify-content: space-between; align-items: center; }
.panel-header h3 { font-size: 13px; font-weight: 700; margin: 0; }
.path { font-size: 11px; color: #64748b; }
.list { flex: 1; overflow-y: auto; padding: 8px; }
.item { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border-radius: 4px; margin-bottom: 4px; cursor: pointer; font-size: 12px; color: #94a3b8; }
.item:hover { background: #1e293b; }
.item.active { background: rgba(59,130,246,0.1); color: #3b82f6; }
.name { flex: 1; }
.size { font-size: 10px; color: #64748b; }
.preview { flex: 1; overflow: auto; padding: 12px; background: #0a0e17; font-family: 'JetBrains Mono', monospace; font-size: 12px; color: #94a3b8; margin: 0; }
</style>
