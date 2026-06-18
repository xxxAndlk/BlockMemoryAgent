<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-gray-300">
    <!-- Header Area -->
    <div class="shrink-0 mb-2 flex justify-between items-end">
      <div>
        <h1 class="text-xl font-bold text-gray-200 mb-1">7. Session History / 会话历史</h1>
        <p class="text-sm text-gray-500">查看历史会话记录，回顾执行过程和结果</p>
      </div>
    </div>

    <!-- Filters Area -->
    <div class="flex gap-4 shrink-0">
      <div class="flex items-center gap-2 bg-[#0f1115] border border-[#2a2d35] rounded px-3 py-1.5 text-sm text-gray-400">
        <span>开始日期</span>
        <span>→</span>
        <span>结束日期</span>
        <el-icon><Calendar /></el-icon>
      </div>
      
      <el-select v-model="filterStatus" size="default" class="w-32 !bg-transparent filter-select">
        <el-option label="全部状态" value="all" />
      </el-select>
      
      <el-select v-model="filterSoul" size="default" class="w-32 !bg-transparent filter-select">
        <el-option label="全部人格" value="all" />
      </el-select>
      
      <el-input 
        v-model="searchQuery" 
        size="default" 
        placeholder="搜索会话名称或目标..." 
        class="flex-1 max-w-md !bg-[#0f1115] search-input"
      >
        <template #suffix><el-icon class="text-gray-500 cursor-pointer hover:text-white"><Search /></el-icon></template>
      </el-input>
      
      <el-button class="!bg-transparent !border-none !text-gray-400 hover:!text-white">重置</el-button>
      
      <el-button class="ml-auto !bg-transparent !border-[#2a2d35] !text-gray-300 hover:!text-white hover:!border-gray-500">
        <el-icon class="mr-2"><Download /></el-icon> 导出历史记录
      </el-button>
    </div>

    <!-- Main Content Area -->
    <div class="flex-1 flex gap-4 min-h-0">
      <!-- History List -->
      <el-card class="w-[400px] !border-[#2a2d35] !bg-[#1a1d24] flex flex-col body-flex-1 shrink-0">
        <template #header>
          <div class="font-bold text-sm text-gray-200">历史会话列表 (28)</div>
        </template>
        
        <div class="flex-1 overflow-y-auto pr-2 space-y-3">
          <div v-for="(session, idx) in historyList" :key="idx" 
               class="p-3 bg-[#0f1115] rounded border cursor-pointer transition-colors"
               :class="selectedSessionId === session.id ? 'border-blue-500' : 'border-[#2a2d35] hover:border-gray-600'">
            <div class="flex justify-between items-start mb-2">
              <div class="flex items-center gap-2">
                <el-icon class="text-blue-400" v-if="session.status === '完成'"><DocumentChecked /></el-icon>
                <el-icon class="text-red-400" v-else-if="session.status === '失败'"><Warning /></el-icon>
                <el-icon class="text-yellow-400" v-else><VideoPause /></el-icon>
                <span class="font-bold text-sm text-gray-200 truncate max-w-[180px]">{{ session.title }}</span>
                <span class="text-[10px] text-gray-500 px-1.5 py-0.5 bg-[#1a1d24] rounded">{{ session.soul }}</span>
              </div>
              <span class="text-xs" :class="session.status === '完成' ? 'text-green-500' : (session.status === '失败' ? 'text-red-500' : 'text-yellow-500')">
                {{ session.status }}
              </span>
            </div>
            <div class="flex justify-between text-xs text-gray-500">
              <span>Session ID: {{ session.id }}</span>
              <span>{{ session.date }}</span>
            </div>
          </div>
        </div>

        <div class="mt-4 pt-4 border-t border-[#2a2d35] flex justify-center items-center">
          <el-pagination
            small
            background
            layout="prev, pager, next, jumper"
            :total="28"
            :page-size="7"
            class="custom-pagination"
          />
        </div>
      </el-card>

      <!-- Detail View -->
      <el-card class="flex-1 !border-[#2a2d35] !bg-[#1a1d24] flex flex-col body-flex-1 min-w-0">
        <div class="flex-1 overflow-y-auto pr-4">
          <div class="flex justify-between items-start mb-2">
            <div class="flex items-center gap-3">
              <h2 class="text-lg font-bold text-gray-200">分析 Gin 项目的架构并生成设计文档</h2>
              <el-tag size="small" type="success" effect="plain" class="!bg-transparent !border-[#2a2d35] !text-green-500">完成</el-tag>
            </div>
            <el-button link type="primary" size="small">查看会话详情 <el-icon class="ml-1"><ArrowRight /></el-icon></el-button>
          </div>
          
          <div class="text-xs text-gray-500 mb-4">Session ID: sess_20250617_001</div>
          
          <div class="flex gap-8 text-xs text-gray-400 mb-6 pb-4 border-b border-[#2a2d35]">
            <span>开始时间: 2025-06-17 14:32:10</span>
            <span>结束时间: 2025-06-17 15:38:45</span>
            <span>总时长: 1h 06m 35s</span>
          </div>
          
          <!-- Tabs -->
          <div class="flex gap-6 border-b border-[#2a2d35] mb-6">
            <span class="text-blue-400 font-bold border-b-2 border-blue-500 pb-2 cursor-pointer">执行概览</span>
            <span class="text-gray-400 hover:text-gray-200 pb-2 cursor-pointer">执行时间线</span>
            <span class="text-gray-400 hover:text-gray-200 pb-2 cursor-pointer">输入输出</span>
            <span class="text-gray-400 hover:text-gray-200 pb-2 cursor-pointer">使用资源</span>
            <span class="text-gray-400 hover:text-gray-200 pb-2 cursor-pointer">会话笔记</span>
          </div>

          <!-- Stats Grid -->
          <div class="grid grid-cols-6 gap-4 mb-6">
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col items-center justify-center text-center">
              <div class="text-xs text-gray-500 mb-2">执行状态</div>
              <div class="text-xl font-bold text-green-500 flex items-center gap-1"><el-icon><SuccessFilled /></el-icon> 完成</div>
              <div class="text-[10px] text-gray-500 mt-2">成功完成所有任务</div>
            </div>
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col items-center justify-center text-center">
              <div class="text-xs text-gray-500 mb-2">任务进度</div>
              <div class="text-2xl font-bold text-gray-200">100%</div>
              <div class="w-full h-1 bg-[#1e3a8a] mt-2 mb-2 rounded"></div>
              <div class="text-[10px] text-gray-500">已完成所有子任务</div>
            </div>
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col justify-center">
              <div class="text-xs text-gray-500 mb-2">LLM 调用</div>
              <div class="text-2xl font-bold text-gray-200">128</div>
              <div class="text-[10px] text-gray-500 mt-1">总调用次数</div>
            </div>
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col justify-center">
              <div class="text-xs text-gray-500 mb-2">工具调用</div>
              <div class="text-2xl font-bold text-gray-200">45</div>
              <div class="text-[10px] text-gray-500 mt-1">总调用次数</div>
            </div>
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col justify-center">
              <div class="text-xs text-gray-500 mb-2">知识检索</div>
              <div class="text-2xl font-bold text-gray-200">23</div>
              <div class="text-[10px] text-gray-500 mt-1">检索次数</div>
            </div>
            <div class="col-span-1 p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex flex-col justify-center">
              <div class="text-xs text-gray-500 mb-2">消息传递</div>
              <div class="text-2xl font-bold text-gray-200">67</div>
              <div class="text-[10px] text-gray-500 mt-1">消息总数</div>
            </div>
          </div>

          <!-- Text Areas Grid -->
          <div class="grid grid-cols-2 gap-6 mb-6">
            <div class="p-4 bg-[#0f1115] border border-[#2a2d35] rounded">
              <div class="font-bold text-sm text-gray-200 mb-3">会话目标</div>
              <div class="text-xs text-gray-400 leading-relaxed">
                分析 GIN 项目的整体架构，理解各模块职责和交互关系，并生成详细的架构设计文档。
              </div>
            </div>
            <div class="p-4 bg-[#0f1115] border border-[#2a2d35] rounded">
              <div class="font-bold text-sm text-gray-200 mb-3">执行总结</div>
              <div class="text-xs text-gray-400 leading-relaxed">
                成功完成了 GIN 项目的架构分析，识别出核心模块、依赖关系和数据流向，生成了包含系统架构图、模块设计和接口规范的完整设计文档。
              </div>
            </div>
          </div>

          <!-- Bottom Grid -->
          <div class="grid grid-cols-2 gap-6">
            <!-- Tools Chart -->
            <div class="p-4 bg-[#0f1115] border border-[#2a2d35] rounded">
              <div class="font-bold text-sm text-gray-200 mb-4">使用工具 Top 5</div>
              <div class="space-y-3">
                <div v-for="(tool, idx) in topTools" :key="idx" class="flex items-center text-xs">
                  <span class="w-4 text-gray-500">{{ idx + 1 }}.</span>
                  <span class="w-32 text-gray-300">{{ tool.name }}</span>
                  <div class="flex-1 flex items-center gap-2">
                    <div class="h-1.5 rounded" :class="tool.colorClass" :style="{ width: tool.percent + '%' }"></div>
                    <span class="text-gray-500 w-16 text-right">{{ tool.count }} 次 ({{ tool.percent }}%)</span>
                  </div>
                </div>
              </div>
            </div>
            
            <!-- Tags & Notes -->
            <div class="flex flex-col gap-6">
              <div class="p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex-1">
                <div class="font-bold text-sm text-gray-200 mb-3">涉及标签</div>
                <div class="flex flex-wrap gap-2">
                  <el-tag v-for="tag in tags" :key="tag" size="small" effect="plain" class="!bg-[#1e3a8a]/20 !border-blue-800/50 !text-blue-400">
                    {{ tag }}
                  </el-tag>
                </div>
              </div>
              <div class="p-4 bg-[#0f1115] border border-[#2a2d35] rounded flex-1">
                <div class="font-bold text-sm text-gray-200 mb-3">备注</div>
                <div class="text-xs text-gray-400">
                  项目结构清晰，模块划分合理，建议进一步优化错误处理机制。
                </div>
              </div>
            </div>
          </div>

          <!-- Timeline -->
          <div class="mt-6 p-4 bg-[#0f1115] border border-[#2a2d35] rounded">
            <div class="font-bold text-sm text-gray-200 mb-6">执行时间线 (关键事件)</div>
            
            <div class="relative px-8 pb-4">
              <!-- Connecting Line -->
              <div class="absolute top-2 left-8 right-8 h-0.5 bg-gradient-to-r from-blue-500 via-blue-500 to-green-500 z-0"></div>
              
              <div class="flex justify-between relative z-10">
                <div v-for="(node, idx) in timeline" :key="idx" class="flex flex-col items-center">
                  <div class="w-4 h-4 rounded-full flex items-center justify-center mb-2" :class="node.status === 'done' ? 'bg-[#0f1115] border-2 border-green-500' : (node.status === 'active' ? 'bg-[#0f1115] border-2 border-yellow-500' : 'bg-[#0f1115] border-2 border-blue-500')">
                    <div class="w-1.5 h-1.5 rounded-full" :class="node.status === 'done' ? 'bg-green-500' : (node.status === 'active' ? 'bg-yellow-500' : 'bg-blue-500')"></div>
                  </div>
                  <div class="text-xs mb-1" :class="node.status === 'done' ? 'text-green-500' : (node.status === 'active' ? 'text-yellow-500' : 'text-blue-400')">{{ node.title }}</div>
                  <div class="text-[10px] text-gray-500">{{ node.time }}</div>
                </div>
              </div>
            </div>
          </div>

        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const filterStatus = ref('all')
const filterSoul = ref('all')
const searchQuery = ref('')
const selectedSessionId = ref('sess_20250617_001')

const historyList = ref([
  { id: 'sess_20250617_001', title: '分析 Gin 项目的架构并生成设计文档', soul: '严谨工程师', status: '完成', date: '2025-06-17 14:32:10' },
  { id: 'sess_20250617_002', title: '修复用户登录问题', soul: '严谨工程师', status: '完成', date: '2025-06-17 13:20:45' },
  { id: 'sess_20250617_003', title: '调研 Redis 缓存方案', soul: '创新探索者', status: '失败', date: '2025-06-17 10:30:15' },
  { id: 'sess_20250616_008', title: '生成 API 接口文档', soul: '严谨工程师', status: '完成', date: '2025-06-16 18:45:22' },
  { id: 'sess_20250616_007', title: '优化数据库查询性能', soul: '性能优化师', status: '完成', date: '2025-06-16 16:20:33' },
  { id: 'sess_20250616_006', title: '设计用户权限系统', soul: '安全专家', status: '完成', date: '2025-06-16 14:15:10' },
  { id: 'sess_20250616_005', title: '实现文件上传功能', soul: '全栈工程师', status: '完成', date: '2025-06-16 11:05:45' },
  { id: 'sess_20250616_004', title: '重构订单服务模块', soul: '严谨工程师', status: '完成', date: '2025-06-16 09:30:12' }
])

const topTools = ref([
  { name: 'ReadFile', count: 28, percent: 62, colorClass: 'bg-blue-500' },
  { name: 'SearchKnowledge', count: 12, percent: 27, colorClass: 'bg-green-500' },
  { name: 'WriteFile', count: 4, percent: 9, colorClass: 'bg-yellow-500' },
  { name: 'GrepSearch', count: 2, percent: 4, colorClass: 'bg-purple-500' },
  { name: 'Bash', count: 2, percent: 4, colorClass: 'bg-gray-500' }
])

const tags = ref(['Gin', '架构设计', '微服务', 'API', '数据库', '中间件', '设计文档'])

const timeline = ref([
  { title: '开始执行', time: '14:32:10', status: 'done' },
  { title: '分析项目结构', time: '14:35:22', status: 'normal' },
  { title: '识别核心模块', time: '14:42:18', status: 'normal' },
  { title: '检索相关知识', time: '14:48:33', status: 'active' },
  { title: '生成架构图', time: '15:02:45', status: 'normal' },
  { title: '编写设计文档', time: '15:15:30', status: 'normal' },
  { title: '完成执行', time: '15:38:45', status: 'done' }
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

:deep(.filter-select .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
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
</style>