<template>
  <div class="flex flex-col h-screen w-full overflow-hidden bg-dark-bg text-gray-200">
    <!-- Header -->
    <header class="h-14 bg-[#14161a] border-b border-dark-border flex items-center px-4 shrink-0">
      <div class="flex items-center w-52 shrink-0">
        <el-icon class="text-primary text-2xl mr-2"><Grid /></el-icon>
        <span class="font-bold text-lg">BlockMemoryAgent</span>
      </div>
      
      <div class="flex items-center space-x-4 ml-4 shrink-0">
        <div class="flex items-center text-sm">
          <span class="text-gray-400 mr-2 whitespace-nowrap">人格:</span>
          <el-select v-model="currentSoul" size="small" class="w-32 !bg-transparent">
            <el-option label="严谨工程师" value="engineer" />
            <el-option label="创意设计师" value="designer" />
          </el-select>
        </div>
      </div>
      
      <div class="flex items-center space-x-6 text-sm ml-auto mr-8 shrink-0">
        <span class="flex items-center text-gray-300"><div class="w-2 h-2 rounded-full bg-green-500 mr-2"></div>Postgres</span>
        <span class="flex items-center text-gray-300"><div class="w-2 h-2 rounded-full bg-green-500 mr-2"></div>Redis</span>
        <span class="flex items-center text-gray-300"><div class="w-2 h-2 rounded-full bg-green-500 mr-2"></div>LLM API</span>
      </div>
      
      <div class="flex items-center space-x-4 shrink-0">
        <el-button size="small" class="!bg-[#2a2d35] !border-none !text-yellow-500 hover:!bg-[#3a3d45]">
          <el-icon class="mr-1"><VideoPause /></el-icon> 暂停 Graph
        </el-button>
        <el-button size="small" class="!bg-[#1e3a2e] !border-none !text-green-500 hover:!bg-[#2e4a3e]">
          <el-icon class="mr-1"><VideoPlay /></el-icon> 恢复 Graph
        </el-button>
        <el-icon class="text-xl cursor-pointer text-gray-400 hover:text-white"><Setting /></el-icon>
      </div>
    </header>

    <!-- Main Body -->
    <div class="flex flex-1 min-h-0">
      <!-- Sidebar -->
      <div class="w-56 bg-[#14161a] flex flex-col shrink-0">
        <el-menu
          :default-active="route.path"
          class="flex-1 overflow-y-auto !border-r-0 py-4"
          background-color="transparent"
          text-color="#9ca3af"
          active-text-color="#ffffff"
          router
        >
          <el-menu-item v-for="item in menuItems" :key="item.path" :index="item.path" :class="{'is-active-custom': route.path === item.path}">
            <el-icon><component :is="item.meta?.icon" /></el-icon>
            <span>{{ item.meta?.title }}</span>
          </el-menu-item>
        </el-menu>
        
        <div class="p-4 text-xs text-gray-500 border-t border-dark-border flex justify-between items-center">
          <div>
            <div>最后更新</div>
            <div>2025-06-17 15:42:30</div>
          </div>
          <el-icon class="cursor-pointer hover:text-gray-300"><Refresh /></el-icon>
        </div>
      </div>

      <!-- Content -->
      <main class="flex-1 overflow-y-auto p-6 bg-[#0f1115]">
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
import { ref, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

const route = useRoute()
const router = useRouter()

const currentSoul = ref('engineer')

const menuItems = computed(() => {
  const routes = router.options.routes.find(r => r.path === '/')?.children || []
  return routes.map(r => ({
    path: '/' + r.path,
    meta: r.meta
  }))
})
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

:deep(.el-input__wrapper) {
  background-color: var(--el-fill-color-blank);
  box-shadow: 0 0 0 1px var(--el-border-color) inset;
}

.is-active-custom {
  background-color: #1e3a8a !important;
  color: white !important;
  border-radius: 4px;
  margin: 0 8px;
}
</style>
