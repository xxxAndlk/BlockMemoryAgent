<template>
  <div class="flex flex-col h-screen w-full overflow-hidden bg-page text-ink">
    <!-- Header -->
    <header class="h-14 bg-card border-b border-line flex items-center px-4 shrink-0">
      <div class="flex items-center w-52 shrink-0">
        <el-icon class="text-primary text-2xl mr-2"><Grid /></el-icon>
        <span class="font-bold text-lg">BlockMemoryAgent</span>
      </div>

      <div class="flex items-center space-x-3 shrink-0 ml-auto">
        <el-icon class="text-xl cursor-pointer text-ink-2 hover:text-primary" @click="toggleTheme">
          <Moon v-if="theme === 'light'" /><Sunny v-else />
        </el-icon>
        <router-link to="/settings">
          <el-icon class="text-xl cursor-pointer text-ink-2 hover:text-primary"><Setting /></el-icon>
        </router-link>
      </div>
    </header>

    <!-- Main Body -->
    <div class="flex flex-1 min-h-0">
      <!-- Sidebar -->
      <div class="w-56 bg-card border-r border-line flex flex-col shrink-0">
        <el-menu
          :default-active="route.path"
          class="flex-1 overflow-y-auto !border-r-0 py-3"
          background-color="transparent"
          router
        >
          <el-menu-item-group v-for="g in menuGroups" :key="g.title">
            <template #title>
              <span class="text-xs text-ink-3">{{ g.title }}</span>
            </template>
            <el-menu-item v-for="item in g.items" :key="item.path" :index="item.path"
                          :class="{ 'is-active-custom': route.path === item.path }">
              <el-icon><component :is="item.icon" /></el-icon>
              <span>{{ item.title }}</span>
            </el-menu-item>
          </el-menu-item-group>
        </el-menu>

        <!-- Beta 预留：工作流编排 -->
        <div class="px-3 pb-3">
          <router-link to="/workflow"
                       class="flex items-center justify-between px-3 py-2.5 rounded-card border border-dashed border-line text-ink-3 hover:text-primary hover:border-primary transition-colors">
            <span class="flex items-center gap-2 text-sm">
              <el-icon><SetUp /></el-icon>工作流编排
            </span>
            <el-tag size="small" effect="plain" type="warning">Beta</el-tag>
          </router-link>
        </div>

        <div class="p-4 text-xs text-ink-3 border-t border-line flex justify-between items-center">
          <div>
            <div>当前人格</div>
            <div class="text-ink-2">{{ soulName }}</div>
          </div>
          <el-icon class="cursor-pointer hover:text-primary" @click="loadStatus"><Refresh /></el-icon>
        </div>
      </div>

      <!-- Content -->
      <main class="flex-1 overflow-y-auto p-6 bg-page">
        <router-view v-slot="{ Component }">
          <transition name="fade" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { getStatus } from '@/api/health'
import { useTheme } from '@/composables/useTheme'

const route = useRoute()
const { theme, toggleTheme } = useTheme()

const soulName = ref('default')

async function loadStatus() {
  try {
    const s = await getStatus()
    if (s?.soul) soulName.value = s.soul
  } catch {
    // ignore
  }
}

onMounted(loadStatus)

// 静态分组菜单（IA 定稿：4 组 + 底部 Beta 预留项，见 spec §信息架构）
const menuGroups = [
  {
    title: '工作台',
    items: [
      { path: '/dashboard', title: '首页', icon: 'House' },
      { path: '/session', title: '会话', icon: 'ChatDotRound' },
    ],
  },
  {
    title: '项目',
    items: [{ path: '/projects', title: '工作目录', icon: 'FolderOpened' }],
  },
  {
    title: '资源库',
    items: [
      { path: '/skills', title: '技能库', icon: 'Connection' },
      { path: '/plugins', title: '插件', icon: 'MagicStick' },
      { path: '/knowledge', title: '知识库', icon: 'Document' },
    ],
  },
  {
    title: '记忆',
    items: [
      { path: '/memory', title: '记忆中心', icon: 'Coin' },
      { path: '/profile', title: '用户画像', icon: 'UserFilled' },
      { path: '/soul', title: '人格配置', icon: 'User' },
      { path: '/history', title: '会话历史', icon: 'Clock' },
    ],
  },
  {
    title: '系统',
    items: [{ path: '/settings', title: '系统设置', icon: 'Setting' }],
  },
]
</script>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

:deep(.el-menu-item) {
  border-radius: 8px;
  margin: 0 8px;
  height: 40px;
  line-height: 40px;
}

:deep(.el-menu-item-group__title) {
  padding: 8px 16px 4px;
}

.is-active-custom {
  background-color: var(--bma-primary-soft) !important;
  color: var(--bma-primary) !important;
  font-weight: 600;
}
</style>
