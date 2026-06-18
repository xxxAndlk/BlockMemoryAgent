<template>
  <div class="h-full flex gap-6 overflow-hidden">
    <!-- Left Column (Create Session & Session List) -->
    <div class="flex-1 flex flex-col gap-6 min-w-0 overflow-y-auto pr-2">
      <!-- Create Session -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="font-bold text-sm text-gray-200">创建新会话</div>
        </template>
        <div class="flex">
          <div class="flex-1 pr-6 space-y-4">
            <div class="text-xs text-gray-400">输入你的目标，我们将为你规划并执行任务</div>
            <el-input
              v-model="goal"
              type="textarea"
              :rows="3"
              placeholder="例如：分析 gin 项目的架构并生成设计文档"
              class="w-full bg-[#0f1115] border-none"
            />
            <div class="flex items-center gap-4">
              <span class="text-sm text-gray-400">快捷模板:</span>
              <div class="flex gap-2">
                <el-tag effect="plain" class="!bg-transparent !border-[#2a2d35] !text-gray-300 cursor-pointer hover:!border-primary">分析代码</el-tag>
                <el-tag effect="plain" class="!bg-transparent !border-[#2a2d35] !text-gray-300 cursor-pointer hover:!border-primary">写文件</el-tag>
                <el-tag effect="plain" class="!bg-transparent !border-[#2a2d35] !text-gray-300 cursor-pointer hover:!border-primary">运行命令</el-tag>
                <el-tag effect="plain" class="!bg-transparent !border-[#2a2d35] !text-gray-300 cursor-pointer hover:!border-primary">搜索知识</el-tag>
              </div>
              <el-button type="primary" class="ml-auto !bg-primary w-40">
                <el-icon class="mr-2"><Promotion /></el-icon> Run Session
              </el-button>
            </div>
          </div>
          <!-- Illustration Area (Placeholder) -->
          <div class="w-48 h-32 flex items-center justify-center shrink-0">
            <div class="relative w-32 h-32">
              <div class="absolute inset-0 bg-blue-500/10 rotate-45 rounded-lg border border-blue-500/30"></div>
              <div class="absolute inset-2 bg-blue-500/20 rotate-12 rounded-lg border border-blue-500/40"></div>
              <div class="absolute inset-4 bg-[#1e3a8a] rounded-lg border border-blue-400 flex items-center justify-center shadow-[0_0_15px_rgba(59,130,246,0.5)]">
                <el-icon class="text-blue-300 text-3xl"><Box /></el-icon>
              </div>
            </div>
          </div>
        </div>
      </el-card>

      <!-- Session List -->
      <el-card class="flex-1 !border-[#2a2d35] !bg-[#1a1d24] flex flex-col body-flex-1 min-h-[400px]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">会话列表</div>
            <el-input v-model="searchSession" size="small" placeholder="搜索会话..." class="w-48 !bg-[#0f1115]">
              <template #suffix><el-icon><Search /></el-icon></template>
            </el-input>
          </div>
        </template>
        
        <!-- Tabs -->
        <div class="flex gap-2 mb-4">
          <el-button size="small" class="!bg-transparent !border-none !text-gray-400 hover:!text-gray-200">全部 12</el-button>
          <el-button size="small" type="primary" class="!bg-[#1e3a8a] !border-none !text-white">运行中 3</el-button>
          <el-button size="small" class="!bg-transparent !border-none !text-gray-400 hover:!text-gray-200">已完成 7</el-button>
          <el-button size="small" class="!bg-transparent !border-none !text-gray-400 hover:!text-gray-200">已失败 2</el-button>
          <el-button size="small" class="!bg-transparent !border-none !text-gray-400 hover:!text-gray-200">已暂停 0</el-button>
        </div>

        <div class="space-y-3 flex-1 overflow-y-auto">
          <div v-for="session in sessions" :key="session.id" class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] flex items-center justify-between group hover:border-primary transition-colors cursor-pointer">
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-3 mb-1">
                <span class="font-bold text-sm text-gray-200 truncate">{{ session.title }}</span>
                <el-tag :type="getStatusType(session.status)" size="small" effect="plain" class="!bg-transparent !border-none px-0">
                  {{ session.status }} <span class="ml-1" :class="getStatusDotClass(session.status)">●</span>
                </el-tag>
              </div>
              <div class="text-xs text-gray-500">创建时间: {{ session.created_at }}</div>
            </div>
            
            <div class="w-48 px-4 flex flex-col items-end">
              <div v-if="session.status === 'RUNNING' || session.status === 'PAUSED'" class="w-full flex items-center gap-2">
                <span class="text-xs text-gray-400 whitespace-nowrap">进度 {{ session.progress }}%</span>
                <el-progress :percentage="session.progress" :show-text="false" :status="session.status === 'PAUSED' ? 'warning' : ''" class="flex-1" />
              </div>
              <div v-else-if="session.status === 'DONE'" class="w-full flex items-center gap-2">
                <span class="text-xs text-gray-400 whitespace-nowrap">100%</span>
                <el-progress :percentage="100" :show-text="false" status="success" class="flex-1" />
              </div>
              <div v-else class="text-xs text-gray-400">-</div>
            </div>

            <div class="w-20 text-right text-xs text-gray-500 flex items-center justify-end gap-2">
              {{ session.ago }}
              <el-icon class="text-gray-600 group-hover:text-primary"><ArrowRight /></el-icon>
            </div>
          </div>
        </div>

        <div class="mt-4 flex justify-between items-center text-xs text-gray-500">
          <span>共 12 条会话</span>
          <el-pagination
            small
            background
            layout="prev, pager, next"
            :total="12"
            class="!p-0"
          />
        </div>
      </el-card>
    </div>

    <!-- Right Column (Stats & Recent Activity) -->
    <div class="w-[400px] flex flex-col gap-6 shrink-0 overflow-y-auto">
      <!-- Stats -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">统计概览 (今日)</div>
            <el-button link type="primary" size="small">查看更多 <el-icon><ArrowRight /></el-icon></el-button>
          </div>
        </template>
        
        <div class="grid grid-cols-2 gap-4 mb-6">
          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">会话总数</div>
            <div class="text-2xl font-bold text-gray-200">28</div>
            <div class="text-xs text-gray-500 mt-1">较昨日 <span class="text-green-500">+12%</span></div>
            <!-- Sparkline placeholder -->
            <svg class="absolute bottom-2 right-2 w-16 h-8 text-green-500 opacity-50" viewBox="0 0 100 30" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M0 30 L20 20 L40 25 L60 10 L80 15 L100 5" />
            </svg>
          </div>
          
          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">完成率</div>
            <div class="text-2xl font-bold text-gray-200">92%</div>
            <div class="text-xs text-gray-500 mt-1">较昨日 <span class="text-green-500">+8%</span></div>
            <!-- Sparkline placeholder -->
            <svg class="absolute bottom-2 right-2 w-16 h-8 text-green-500 opacity-50" viewBox="0 0 100 30" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M0 30 L20 25 L40 15 L60 20 L80 10 L100 5" />
            </svg>
          </div>
          
          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">LLM 调用总数</div>
            <div class="text-2xl font-bold text-gray-200">1,203</div>
            <div class="text-xs text-gray-500 mt-1">较昨日 <span class="text-purple-500">+23%</span></div>
            <!-- Sparkline placeholder -->
            <svg class="absolute bottom-2 right-2 w-16 h-8 text-purple-500 opacity-50" viewBox="0 0 100 30" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M0 30 L20 10 L40 15 L60 5 L80 20 L100 0" />
            </svg>
          </div>
          
          <div class="p-3 bg-[#0f1115] rounded border border-[#2a2d35] relative overflow-hidden">
            <div class="text-xs text-gray-400 mb-1">超时率</div>
            <div class="text-2xl font-bold text-gray-200">2.4%</div>
            <div class="text-xs text-gray-500 mt-1">较昨日 <span class="text-yellow-500">-1.2%</span></div>
            <!-- Sparkline placeholder -->
            <svg class="absolute bottom-2 right-2 w-16 h-8 text-yellow-500 opacity-50" viewBox="0 0 100 30" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M0 5 L20 15 L40 10 L60 25 L80 20 L100 30" />
            </svg>
          </div>
        </div>

        <div>
          <div class="flex justify-between text-xs text-gray-400 mb-2">
            <span>平均响应时间趋势 (最近 10 次)</span>
            <span>单位: 秒</span>
          </div>
          <div class="h-40 bg-[#0f1115] rounded border border-[#2a2d35] relative">
            <!-- Main Line Chart Placeholder -->
            <svg class="w-full h-full text-blue-500 p-4" viewBox="0 0 100 40" preserveAspectRatio="none" fill="none" stroke="currentColor" stroke-width="1.5">
              <path d="M0 20 L10 15 L20 18 L30 10 L40 15 L50 22 L60 20 L70 25 L80 12 L90 15 L100 18" />
              <!-- Points -->
              <circle cx="0" cy="20" r="1.5" fill="currentColor" />
              <circle cx="10" cy="15" r="1.5" fill="currentColor" />
              <circle cx="20" cy="18" r="1.5" fill="currentColor" />
              <circle cx="30" cy="10" r="1.5" fill="currentColor" />
              <circle cx="40" cy="15" r="1.5" fill="currentColor" />
              <circle cx="50" cy="22" r="1.5" fill="currentColor" />
              <circle cx="60" cy="20" r="1.5" fill="currentColor" />
              <circle cx="70" cy="25" r="1.5" fill="currentColor" />
              <circle cx="80" cy="12" r="1.5" fill="currentColor" />
              <circle cx="90" cy="15" r="1.5" fill="currentColor" />
              <circle cx="100" cy="18" r="1.5" fill="currentColor" />
            </svg>
            <div class="absolute bottom-2 left-4 right-4 flex justify-between text-[10px] text-gray-600">
              <span>-9</span><span>-8</span><span>-7</span><span>-6</span><span>-5</span><span>-4</span><span>-3</span><span>-2</span><span>-1</span><span>0</span>
            </div>
            <div class="absolute top-2 bottom-6 left-2 flex flex-col justify-between text-[10px] text-gray-600">
              <span>4</span><span>3</span><span>2</span><span>1</span><span>0</span>
            </div>
          </div>
        </div>
      </el-card>

      <!-- Recent Activity -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">最近活动</div>
            <el-button link type="primary" size="small">查看更多 <el-icon><ArrowRight /></el-icon></el-button>
          </div>
        </template>
        
        <div class="space-y-4">
          <div v-for="(activity, idx) in activities" :key="idx" class="flex items-start gap-3 text-sm">
            <div class="mt-0.5 rounded-full p-1 shrink-0" :class="activity.bgClass">
              <el-icon :class="activity.iconClass"><component :is="activity.icon" /></el-icon>
            </div>
            <div class="flex-1 min-w-0">
              <div class="text-gray-300 break-words line-clamp-2">
                <span class="text-gray-400 mr-1" v-if="activity.agent">{{ activity.agent }}</span>
                {{ activity.content }}
                <el-tag v-if="activity.tag" :type="activity.tagType" size="small" effect="dark" class="scale-75 origin-left ml-1">{{ activity.tag }}</el-tag>
              </div>
            </div>
            <div class="text-xs text-gray-500 shrink-0 whitespace-nowrap">{{ activity.time }}</div>
          </div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const goal = ref('')
const searchSession = ref('')

const sessions = ref([
  { id: 1, title: '分析 gin 项目的架构并生成设计文档', status: 'RUNNING', created_at: '2025-06-17 14:32:10', progress: 75, ago: '2m ago' },
  { id: 2, title: '生成项目接口设计文档', status: 'RUNNING', created_at: '2025-06-17 13:20:45', progress: 45, ago: '11m ago' },
  { id: 3, title: '修复用户登录问题', status: 'PAUSED', created_at: '2025-06-17 11:45:30', progress: 60, ago: '3h ago' },
  { id: 4, title: '调研 Redis 缓存方案', status: 'FAILED', created_at: '2025-06-17 10:30:15', progress: 0, ago: '5h ago' },
  { id: 5, title: '编写单元测试', status: 'DONE', created_at: '2025-06-16 09:15:20', progress: 100, ago: '1d ago' },
  { id: 6, title: '优化接口性能', status: 'DONE', created_at: '2025-06-16 18:20:05', progress: 100, ago: '1d ago' }
])

const activities = ref([
  { icon: 'Connection', bgClass: 'bg-blue-900/30', iconClass: 'text-blue-400', agent: 'MetaAgent', content: '在会话「分析 gin 项目的架构并生成设计文档」中调用了 SearchKnowledge 工具', time: '2m ago' },
  { icon: 'User', bgClass: 'bg-blue-900/30', iconClass: 'text-blue-400', agent: 'DomainAgent-CodeAnalysis', content: '完成了子任务「解析项目结构」', time: '5m ago' },
  { icon: 'Trophy', bgClass: 'bg-green-900/30', iconClass: 'text-green-400', content: '收到来自 SubAgent-Architecture 的里程碑通知', tag: 'Milestone', tagType: 'success', time: '12m ago' },
  { icon: 'Warning', bgClass: 'bg-yellow-900/30', iconClass: 'text-yellow-400', agent: 'Watchdog', content: '触发记忆压缩 (Compress)', tag: 'Warning', tagType: 'warning', time: '15m ago' },
  { icon: 'CircleClose', bgClass: 'bg-red-900/30', iconClass: 'text-red-400', content: '会话「调研 Redis 缓存方案」执行失败', tag: 'Error', tagType: 'danger', time: '5h ago' }
])

const getStatusType = (status: string) => {
  switch (status) {
    case 'RUNNING': return 'primary'
    case 'DONE': return 'success'
    case 'FAILED': return 'danger'
    case 'PAUSED': return 'warning'
    default: return 'info'
  }
}

const getStatusDotClass = (status: string) => {
  switch (status) {
    case 'RUNNING': return 'text-blue-500'
    case 'DONE': return 'text-green-500'
    case 'FAILED': return 'text-red-500'
    case 'PAUSED': return 'text-yellow-500'
    default: return 'text-gray-500'
  }
}
</script>

<style scoped>
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:deep(.el-textarea__inner) {
  background-color: transparent;
  box-shadow: none !important;
  color: #e5e7eb;
}
:deep(.el-textarea__inner:focus) {
  box-shadow: none !important;
}
:deep(.el-input__wrapper) {
  background-color: #0f1115;
  box-shadow: 0 0 0 1px #2a2d35 inset;
}
:deep(.el-pagination.is-background .el-pager li:not(.is-disabled).is-active) {
  background-color: var(--el-color-primary);
}
</style>
