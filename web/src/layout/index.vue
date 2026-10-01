<template>
  <div class="flex flex-col h-screen w-full overflow-hidden bg-page text-ink">
    <!-- Header -->
    <header class="h-14 bg-card border-b border-line flex items-center justify-between px-4 shrink-0">
      <div class="flex items-center">
        <el-icon class="text-primary text-2xl mr-2"><Grid /></el-icon>
        <span class="font-bold text-lg">BlockMemoryAgent</span>
      </div>

      <!-- 顶栏右侧操作区：统一 pill 容器（底色 + 描边 + 圆角），
           与左侧标题同一行、垂直居中，hover 与项目现有按钮一致 -->
      <div class="flex items-center gap-0.5 p-1 rounded-full border border-line bg-page">
        <button type="button" title="切换主题"
                class="w-8 h-8 rounded-full flex items-center justify-center text-ink-2 hover:text-primary hover:bg-primary-soft transition-colors cursor-pointer"
                @click="toggleTheme">
          <el-icon class="text-lg"><Moon v-if="theme === 'light'" /><Sunny v-else /></el-icon>
        </button>
        <router-link to="/settings" title="系统设置"
                     class="w-8 h-8 rounded-full flex items-center justify-center text-ink-2 hover:text-primary hover:bg-primary-soft transition-colors">
          <el-icon class="text-lg"><Setting /></el-icon>
        </router-link>
      </div>
    </header>

    <!-- Main Body -->
    <div class="flex flex-1 min-h-0">
      <!-- Sidebar -->
      <div class="w-[280px] bg-card border-r border-line flex flex-col shrink-0">
        <!-- 首页：置顶在会话树上方（用户要求），与右侧菜单项同款高亮 -->
        <router-link to="/dashboard"
                     class="flex items-center gap-2 px-5 py-2.5 mt-1 text-sm shrink-0 transition-colors"
                     :class="route.path === '/dashboard' ? 'text-primary font-semibold' : 'text-ink-2 hover:text-primary'">
          <el-icon><House /></el-icon>
          <span>首页</span>
        </router-link>
        <!-- 会话目录树（Codex 式）：按工作目录分组会话，取代原来的「会话 / 工作目录」两个导航项。
             自身按展开/收起决定是否 flex-1（收起时不吃剩余高度），故外面不再套弹性容器。 -->
        <WorkDirTree class="min-h-0" />

        <el-menu
          :default-active="route.path"
          class="!border-r-0 py-2 shrink-0 overflow-y-auto max-h-[45vh]"
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

        <!-- Beta 预留：工作流编排（mt-auto：会话树收起时把尾部区块钉在侧栏底部） -->
        <div class="px-3 pb-3 mt-auto">
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
import WorkDirTree from '@/components/WorkDirTree.vue'

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

// 静态分组菜单（IA 定稿：4 组 + 底部 Beta 预留项，见 spec §信息架构）。
// 「会话 / 工作目录」两项已并入上方目录树（树里可直接开旧会话、起新会话、改目录配置），
// 「首页」已上移到侧栏顶部（会话树上方），故均不再在此占位；
// /projects、/session 路由仍可用（后者由目录树与首页的跳转进入）。
const menuGroups = [
  {
    // 「工作目录」页不只是目录清单：项目偏好、测试助手、目录增删都在这，侧栏树再方便也替代不了
    title: '资源库',
    items: [
      { path: '/projects', title: '工作目录', icon: 'FolderOpened' },
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
    items: [
      { path: '/dag', title: '定时任务', icon: 'AlarmClock' },
      { path: '/settings', title: '系统设置', icon: 'Setting' },
    ],
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
