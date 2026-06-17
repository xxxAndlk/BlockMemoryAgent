<script setup lang="ts">
const skills = [
  { id: 'read_file', name: '读取文件', desc: '读取本地文件并返回文本内容', cost: 5, tags: ['io', 'fs'] },
  { id: 'write_file', name: '写入文件', desc: '把文本内容写入本地文件', cost: 8, tags: ['io', 'fs'] },
  { id: 'list_dir', name: '列出目录', desc: '列出目录中的文件与子目录', cost: 3, tags: ['io', 'fs'] },
  { id: 'run_command', name: '执行命令', desc: '在沙箱内执行 shell 命令并返回输出', cost: 10, tags: ['shell'] },
  { id: 'search_in_files', name: '代码搜索', desc: '在工作目录中按关键词搜索文件内容', cost: 6, tags: ['search'] },
  { id: 'http_get', name: 'HTTP GET', desc: '对指定 URL 发起 GET 请求', cost: 4, tags: ['http'] },
  { id: 'http_post', name: 'HTTP POST', desc: '对指定 URL 发起 POST 请求', cost: 4, tags: ['http'] },
]
</script>

<template>
  <div class="layout">
    <div class="left">
      <div class="panel-header"><h3>选择智能体</h3></div>
      <div class="list"><div class="item active"><span>◆</span> DomainAgent</div></div>
    </div>
    <div class="right">
      <div class="panel-header"><h3>已装配技能</h3></div>
      <div class="cards">
        <div v-for="s in skills" :key="s.id" class="skill">
          <div class="skill-header">
            <span class="id">{{ s.id }}</span>
            <span class="cost">消耗: {{ s.cost }}</span>
          </div>
          <div class="name">{{ s.name }}</div>
          <div class="desc">{{ s.desc }}</div>
          <div class="tags">
            <span v-for="t in s.tags" :key="t" class="tag">{{ t }}</span>
          </div>
        </div>
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
.item.active { background: rgba(59,130,246,0.1); color: #3b82f6; }
.cards { flex: 1; overflow-y: auto; padding: 12px; display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 12px; }
.skill { background: #1a2332; border: 1px solid #243447; border-radius: 8px; padding: 14px; }
.skill-header { display: flex; justify-content: space-between; margin-bottom: 8px; }
.id { font-size: 11px; font-weight: 700; color: #3b82f6; }
.cost { font-size: 10px; color: #64748b; }
.name { font-size: 13px; font-weight: 600; color: #e2e8f0; margin-bottom: 6px; }
.desc { font-size: 12px; color: #94a3b8; margin-bottom: 8px; }
.tag { font-size: 10px; padding: 2px 6px; background: #1e293b; border-radius: 3px; color: #64748b; margin-right: 4px; }
</style>
