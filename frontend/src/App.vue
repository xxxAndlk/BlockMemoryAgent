<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import Sidebar from './components/Sidebar.vue'
import Topbar from './components/Topbar.vue'

const route = useRoute()
const titles: Record<string, string> = {
  dashboard: '总览',
  chat: '对话',
  sessions: '会话管理',
  agents: '智能体树',
  board: '任务看板',
  memory: '记忆浏览器',
  skills: '技能装配',
  files: '文件管理',
  knowledge: '知识库',
  history: '会话历史',
  health: '系统健康',
  settings: '设置',
}
const pageTitle = computed(() => titles[route.name as string] || route.name as string)
</script>

<template>
  <Sidebar />
  <div class="main">
    <Topbar :title="pageTitle" />
    <div class="content">
      <router-view />
    </div>
  </div>
</template>

<style scoped>
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; overflow: hidden; }
.content { flex: 1; overflow: auto; padding: 16px 20px; }
</style>
