<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-gray-300">
    <!-- Header Area -->
    <div class="shrink-0 mb-2">
      <h1 class="text-xl font-bold text-gray-200 mb-1">6. Knowledge Base / 知识库搜索</h1>
      <p class="text-sm text-gray-500">基于语义搜索的全局知识库，支持查看、归档和管理知识条目</p>
    </div>

    <!-- Top Area: Search & Stats -->
    <div class="flex gap-4 shrink-0">
      <!-- Search Box -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] flex-1">
        <div class="font-bold text-sm text-gray-200 mb-4">语义搜索</div>
        <div class="flex gap-3 mb-4">
          <el-input 
            v-model="searchQuery" 
            size="large" 
            placeholder="golang ddd architecture" 
            class="flex-1 !bg-[#0f1115] search-input"
          >
            <template #prefix><el-icon class="text-gray-500"><Search /></el-icon></template>
          </el-input>
          <el-button type="primary" size="large" class="!bg-primary px-6">
            <el-icon class="mr-1"><Search /></el-icon> 搜索
          </el-button>
          <el-button size="large" class="!bg-[#0f1115] !border-[#2a2d35] !text-gray-300">
            <el-icon class="mr-1"><Filter /></el-icon> 高级筛选
          </el-button>
        </div>
        <div class="flex items-center gap-3 text-xs">
          <span class="text-gray-500">热门搜索:</span>
          <div class="flex gap-2">
            <el-tag v-for="tag in hotSearches" :key="tag" effect="plain" class="!bg-[#0f1115] !border-[#2a2d35] !text-gray-400 cursor-pointer hover:!text-gray-200 hover:!border-gray-500">
              {{ tag }}
            </el-tag>
          </div>
        </div>
      </el-card>

      <!-- Stats -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] w-[400px]">
        <div class="font-bold text-sm text-gray-200 mb-4">知识库概览</div>
        <div class="flex justify-between text-center mt-2">
          <div class="flex-1 border-r border-[#2a2d35]">
            <div class="text-xs text-gray-500 mb-1">总条目数</div>
            <div class="text-2xl font-bold text-gray-200">2,567</div>
          </div>
          <div class="flex-1 border-r border-[#2a2d35]">
            <div class="text-xs text-gray-500 mb-1">已归档</div>
            <div class="text-2xl font-bold text-gray-200">1,193</div>
          </div>
          <div class="flex-1 border-r border-[#2a2d35]">
            <div class="text-xs text-gray-500 mb-1">今日新增</div>
            <div class="text-2xl font-bold text-gray-200">23</div>
          </div>
          <div class="flex-1">
            <div class="text-xs text-gray-500 mb-1">访问总数</div>
            <div class="text-2xl font-bold text-gray-200">18,456</div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Main Content Area -->
    <div class="flex-1 flex gap-4 min-h-0">
      <!-- Results List -->
      <el-card class="flex-1 !border-[#2a2d35] !bg-[#1a1d24] flex flex-col body-flex-1 min-w-0">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">搜索结果 (23)</div>
            <div class="flex items-center gap-4">
              <div class="flex items-center text-xs">
                <span class="text-gray-500 mr-2">排序:</span>
                <el-select v-model="sortBy" size="small" class="w-24 !bg-transparent sort-select">
                  <el-option label="相关度" value="relevance" />
                  <el-option label="最新" value="newest" />
                </el-select>
              </div>
              <div class="flex gap-1 text-gray-500">
                <el-icon class="cursor-pointer hover:text-white p-1 bg-[#2a2d35] rounded"><List /></el-icon>
                <el-icon class="cursor-pointer hover:text-white p-1"><Menu /></el-icon>
              </div>
            </div>
          </div>
        </template>
        
        <div class="flex-1 overflow-y-auto pr-2 space-y-3">
          <div v-for="(item, idx) in results" :key="idx" class="p-4 bg-[#0f1115] rounded border border-[#2a2d35] hover:border-primary transition-colors group">
            <div class="flex justify-between items-start mb-2">
              <div class="flex items-center gap-2">
                <el-icon class="text-blue-400 text-lg"><Document /></el-icon>
                <span class="font-bold text-sm text-blue-400 hover:underline cursor-pointer">{{ item.title }}</span>
                <el-tag :type="item.archived ? 'success' : 'info'" size="small" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90" :class="item.archived ? '!text-green-500' : '!text-gray-500'">
                  {{ item.archived ? '已归档' : '未归档' }}
                </el-tag>
              </div>
              <div class="text-right">
                <div class="text-xs text-gray-500 mb-1">相似度</div>
                <div class="font-bold text-green-500">{{ item.similarity }}</div>
              </div>
            </div>
            
            <div class="text-xs text-gray-400 mb-4 line-clamp-2">{{ item.content }}</div>
            
            <div class="flex justify-between items-center text-xs text-gray-500">
              <div class="flex gap-4">
                <span class="flex items-center gap-1"><el-icon><Folder /></el-icon> {{ item.category }}</span>
                <span class="flex items-center gap-1"><el-icon><User /></el-icon> {{ item.agent }}</span>
                <span class="flex items-center gap-1"><el-icon><Calendar /></el-icon> {{ item.date }}</span>
                <span class="flex items-center gap-1"><el-icon><View /></el-icon> 访问 {{ item.views }} 次</span>
              </div>
              <el-icon class="text-gray-500 cursor-pointer hover:text-white text-lg"><MoreFilled /></el-icon>
            </div>
          </div>
        </div>

        <div class="mt-4 pt-4 border-t border-[#2a2d35] flex justify-center items-center">
          <el-pagination
            background
            layout="prev, pager, next, jumper"
            :total="23"
            :page-size="5"
            class="custom-pagination"
          />
        </div>
      </el-card>

      <!-- Right Sidebar: Filters -->
      <el-card class="w-[300px] !border-[#2a2d35] !bg-[#1a1d24] shrink-0 flex flex-col body-flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">筛选条件</div>
            <span class="text-xs text-blue-400 cursor-pointer hover:text-blue-300">清空</span>
          </div>
        </template>
        
        <div class="flex-1 overflow-y-auto pr-2 space-y-6">
          <!-- Content Type -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-3">内容类型</div>
            <div class="space-y-2">
              <el-checkbox v-for="type in contentTypes" :key="type.label" v-model="type.checked" class="w-full !mr-0 custom-checkbox">
                <div class="flex justify-between text-xs w-[220px]">
                  <span class="text-gray-300">{{ type.label }}</span>
                  <span class="text-gray-500">({{ type.count }})</span>
                </div>
              </el-checkbox>
            </div>
          </div>

          <!-- Domain -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-3">领域分类</div>
            <div class="space-y-2">
              <el-checkbox v-for="domain in domains" :key="domain.label" v-model="domain.checked" class="w-full !mr-0 custom-checkbox">
                <div class="flex justify-between text-xs w-[220px]">
                  <span class="text-gray-300">{{ domain.label }}</span>
                  <span class="text-gray-500">({{ domain.count }})</span>
                </div>
              </el-checkbox>
            </div>
          </div>

          <!-- Status -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-3">状态</div>
            <div class="space-y-2">
              <el-checkbox v-for="status in statuses" :key="status.label" v-model="status.checked" class="w-full !mr-0 custom-checkbox">
                <div class="flex justify-between text-xs w-[220px]">
                  <span class="text-gray-300">{{ status.label }}</span>
                  <span class="text-gray-500">({{ status.count }})</span>
                </div>
              </el-checkbox>
            </div>
          </div>

          <!-- Date Range -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-3">时间范围</div>
            <div class="flex items-center gap-2 bg-[#0f1115] border border-[#2a2d35] rounded p-2 text-xs text-gray-500">
              <span class="flex-1 text-center">开始日期</span>
              <span>-</span>
              <span class="flex-1 text-center">结束日期</span>
              <el-icon><Calendar /></el-icon>
            </div>
          </div>
        </div>

        <div class="mt-4 pt-4 border-t border-[#2a2d35]">
          <el-button type="primary" class="w-full !bg-[#1e3a8a] !border-none">
            <el-icon class="mr-2"><Download /></el-icon> 导出搜索结果
          </el-button>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const searchQuery = ref('golang ddd architecture')
const sortBy = ref('relevance')

const hotSearches = ref([
  'golang best practice',
  'ddd 领域驱动设计',
  'redis 缓存策略',
  'gin 框架',
  'nacos 注册中心'
])

const results = ref([
  { 
    title: 'DDD 架构在 Go 项目中的实践', 
    archived: true,
    similarity: '0.95', 
    content: '本文详细介绍了领域驱动设计 (DDD) 在 Go 语言项目中的落地实践，包括领域建模、聚合设计、领域服务、仓储实现等核心概念...',
    category: '架构设计',
    agent: 'ArchitectureAgent',
    date: '2025-06-10 14:32',
    views: 128
  },
  { 
    title: 'Go 语言项目分层架构最佳实践', 
    archived: true,
    similarity: '0.91', 
    content: '总结了 Go 项目推荐的分层架构模式：接口层、应用层、领域层、基础设施层的职责划分和依赖关系设计...',
    category: '最佳实践',
    agent: 'CodeAnalysisAgent',
    date: '2025-06-08 09:15',
    views: 96
  },
  { 
    title: '领域驱动设计核心概念详解', 
    archived: false,
    similarity: '0.88', 
    content: '深入解析 DDD 的核心概念：领域、子领域、聚合、实体、值对象、领域事件等，附带丰富的示例代码...',
    category: '理论概念',
    agent: 'DocumentAgent',
    date: '2025-06-05 16:20',
    views: 77
  },
  { 
    title: 'Go + DDD 微服务架构实战', 
    archived: true,
    similarity: '0.85', 
    content: '基于 Go 语言和 DDD 设计思想构建微服务架构的完整实践，包括服务拆分、数据一致性、事件驱动等...',
    category: '实战案例',
    agent: 'ArchitectureAgent',
    date: '2025-06-03 11:45',
    views: 64
  },
  { 
    title: 'DDD 中的聚合设计原则', 
    archived: false,
    similarity: '0.82', 
    content: '聚合是 DDD 的核心概念之一，本文介绍聚合的设计原则、边界划分、聚合根选择等关键内容...',
    category: '领域建模',
    agent: 'CodeAnalysisAgent',
    date: '2025-06-01 15:30',
    views: 58
  },
  { 
    title: 'Go 项目中的依赖注入实现', 
    archived: true,
    similarity: '0.79', 
    content: '介绍在 Go 项目中实现依赖注入的多种方式，包括 wire、dig 等工具的使用和最佳实践...',
    category: '技术实现',
    agent: 'CodeAnalysisAgent',
    date: '2025-05-30 10:20',
    views: 42
  }
])

const contentTypes = ref([
  { label: '架构设计', count: 856, checked: false },
  { label: '最佳实践', count: 642, checked: false },
  { label: '理论概念', count: 538, checked: false },
  { label: '实战案例', count: 312, checked: false },
  { label: '技术实现', count: 219, checked: false }
])

const domains = ref([
  { label: '后端开发', count: '1,256', checked: false },
  { label: '架构设计', count: 856, checked: false },
  { label: '数据库', count: 432, checked: false },
  { label: '中间件', count: 398, checked: false },
  { label: '运维部署', count: 245, checked: false }
])

const statuses = ref([
  { label: '未归档', count: '1,374', checked: false },
  { label: '已归档', count: '1,193', checked: false }
])
</script>

<style scoped>
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 16px;
}

:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}

:deep(.sort-select .el-input__wrapper) {
  box-shadow: none !important;
  background-color: transparent !important;
  padding: 0;
}

:deep(.custom-pagination.el-pagination.is-background .el-pager li:not(.is-disabled).is-active) {
  background-color: var(--el-color-primary);
}
:deep(.custom-pagination.el-pagination.is-background .el-pager li) {
  background-color: #0f1115;
  color: var(--el-text-color-regular);
  border: 1px solid #2a2d35;
}
:deep(.custom-pagination.el-pagination.is-background .btn-next),
:deep(.custom-pagination.el-pagination.is-background .btn-prev) {
  background-color: #0f1115;
  color: var(--el-text-color-regular);
  border: 1px solid #2a2d35;
}

:deep(.custom-checkbox .el-checkbox__label) {
  width: 100%;
}
</style>
