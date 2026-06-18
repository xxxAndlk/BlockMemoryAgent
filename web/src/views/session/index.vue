<template>
  <div class="h-full flex gap-4 overflow-hidden text-gray-300">
    <!-- Left Column: Hierarchy & Task Board -->
    <div class="w-[320px] flex flex-col gap-4 overflow-y-auto shrink-0 pr-1">
      <!-- Role Hierarchy -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">角色层级 (Role Hierarchy)</div>
            <el-icon class="text-gray-500 cursor-pointer hover:text-white"><Close /></el-icon>
          </div>
        </template>
        <el-tree
          :data="roleData"
          :props="defaultProps"
          default-expand-all
          class="!bg-transparent text-sm custom-tree"
          :expand-on-click-node="false"
        >
          <template #default="{ node, data }">
            <div class="flex items-center justify-between w-full pr-2 py-1">
              <span class="flex items-center gap-2">
                <el-icon :class="data.iconColor" class="text-lg"><UserFilled v-if="data.isUser" /><User v-else /></el-icon>
                <span :class="{'text-gray-200': data.active, 'text-gray-500': !data.active}">{{ node.label }}</span>
              </span>
              <el-tag v-if="data.status" :type="data.statusType" size="small" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90 origin-right" :class="{'!text-green-500': data.status==='active', '!text-yellow-500': data.status==='running', '!text-gray-500': data.status==='pending'}">
                {{ data.status }}
              </el-tag>
            </div>
          </template>
        </el-tree>
      </el-card>

      <!-- Task Board -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24] flex-1 min-h-0 flex flex-col body-flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">任务看板 (Task Board)</div>
            <el-icon class="text-gray-500 cursor-pointer hover:text-white"><Close /></el-icon>
          </div>
        </template>
        
        <div class="flex-1 overflow-y-auto">
          <div class="text-xs mb-6">
            <div class="flex justify-between items-start mb-2">
              <span class="text-gray-400">Goal: <span class="text-gray-200">分析项目并生成技术设计文档</span></span>
              <el-tag size="small" effect="dark" class="!bg-[#1e3a8a] !border-none !text-blue-300 scale-90">IN_PROGRESS</el-tag>
            </div>
            <div class="flex items-center justify-between mt-4 mb-1">
              <span class="text-gray-400">Progress</span>
              <span class="text-gray-400">6 / 8 (75%)</span>
            </div>
            <el-progress :percentage="75" :show-text="false" class="custom-progress" />
          </div>
          
          <div class="flex text-xs text-gray-500 mb-2 px-2">
            <div class="flex-1">子任务列表</div>
            <div class="w-24">接收方</div>
            <div class="w-20 text-right">状态</div>
          </div>
          
          <div class="space-y-1 mb-6">
            <div v-for="(task, index) in tasks" :key="index" class="flex items-center text-xs p-2 hover:bg-[#2a2d35] rounded transition-colors group">
              <div class="flex-1 flex items-center gap-2 truncate pr-2" :class="{'text-gray-200': task.status !== 'pending', 'text-gray-500': task.status === 'pending'}">
                <span class="text-gray-500">{{ index + 1 }}.</span>
                {{ task.name }}
              </div>
              <div class="w-24 text-gray-400 truncate">{{ task.assignee }}</div>
              <div class="w-20 text-right flex items-center justify-end gap-1">
                <el-icon v-if="task.status === 'done'" class="text-green-500"><Check /></el-icon>
                <el-icon v-else-if="task.status === 'in_progress'" class="text-blue-500 is-loading"><Loading /></el-icon>
                <el-icon v-else-if="task.status === 'blocked'" class="text-yellow-500"><Warning /></el-icon>
                <el-icon v-else class="text-gray-600"><Clock /></el-icon>
                <span :class="{'text-green-500': task.status==='done', 'text-blue-500': task.status==='in_progress', 'text-yellow-500': task.status==='blocked', 'text-gray-500': task.status==='pending'}">{{ task.status }}</span>
              </div>
            </div>
          </div>

          <!-- Constraints -->
          <div class="border-t border-[#2a2d35] pt-4">
            <div class="text-xs font-bold text-gray-400 mb-3">约束条件 (Constraints)</div>
            <div class="space-y-2 text-xs">
              <div class="flex justify-between p-2 bg-[#0f1115] rounded">
                <span class="text-gray-500">编程语言</span>
                <span class="text-gray-200">Go</span>
              </div>
              <div class="flex justify-between p-2 bg-[#0f1115] rounded">
                <span class="text-gray-500">架构风格</span>
                <span class="text-gray-200">DDD</span>
              </div>
              <div class="flex justify-between p-2 bg-[#0f1115] rounded">
                <span class="text-gray-500">文档格式</span>
                <span class="text-gray-200">Markdown</span>
              </div>
              <div class="flex justify-between p-2 bg-[#0f1115] rounded">
                <span class="text-gray-500">输出路径</span>
                <span class="text-gray-200">./docs/design.md</span>
              </div>
            </div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Middle Column: Dynamic Content based on Tabs -->
    <div class="flex-1 flex flex-col bg-[#1a1d24] border border-[#2a2d35] rounded-lg overflow-hidden min-w-0">
      <div class="h-12 border-b border-[#2a2d35] flex items-center px-4 bg-[#14161a] gap-6 text-sm shrink-0">
        <span @click="activeTab = 'log'" :class="activeTab === 'log' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer">
          <el-icon class="mr-1"><Document /></el-icon> 执行日志
        </span>
        <span @click="activeTab = 'memory'" :class="activeTab === 'memory' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer">
          <el-icon class="mr-1"><List /></el-icon> 记忆浏览器
        </span>
        <span @click="activeTab = 'skill'" :class="activeTab === 'skill' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer">
          <el-icon class="mr-1"><Connection /></el-icon> Skill 装配
        </span>
        <span @click="activeTab = 'file'" :class="activeTab === 'file' ? 'text-blue-400 font-bold border-b-2 border-blue-500 pb-[2px]' : 'text-gray-400 hover:text-gray-200'" class="flex items-center h-full cursor-pointer">
          <el-icon class="mr-1"><Files /></el-icon> 文件预览
        </span>
      </div>
      
      <div class="flex-1 overflow-hidden relative flex flex-col">
        <ExecutionLog v-if="activeTab === 'log'" />
        <MemoryExplorer v-if="activeTab === 'memory'" />
        <SkillSet v-if="activeTab === 'skill'" />
        <FilePreview v-if="activeTab === 'file'" />
      </div>
    </div>

    <!-- Right Column: Metrics & Health -->
    <div class="w-[320px] flex flex-col gap-4 overflow-y-auto shrink-0 pl-1">
      <!-- Metrics -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">实时指标 (Metrics)</div>
            <el-icon class="text-gray-500 cursor-pointer hover:text-white"><Close /></el-icon>
          </div>
        </template>
        
        <div class="text-xs text-gray-400 mb-2">LLM 调用统计</div>
        <div class="grid grid-cols-3 gap-2 mb-6">
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">总调用</div>
            <div class="text-xl font-bold text-gray-200">127<span class="text-xs font-normal ml-1">次</span></div>
          </div>
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">超时</div>
            <div class="text-xl font-bold text-red-400">3<span class="text-xs font-normal ml-1">次</span></div>
          </div>
          <div class="p-2 text-center">
            <div class="text-xs text-gray-500 mb-1">平均耗时</div>
            <div class="text-xl font-bold text-green-400">2.34s</div>
          </div>
          <div class="p-2 text-center col-start-3 row-start-1 -mt-10 opacity-0 absolute pointer-events-none">
            <!-- Hidden element just for grid structure reference from previous layout if needed -->
          </div>
          <!-- Adjusting to 4 columns conceptually based on design -->
          <div class="p-2 text-center absolute right-6 top-[4.5rem]">
            <div class="text-xs text-gray-500 mb-1">最长耗时</div>
            <div class="text-xl font-bold text-gray-200">9.12s</div>
          </div>
        </div>

        <div class="text-xs text-gray-400 mb-2">上下文用量</div>
        <div class="mb-6 text-xs">
          <div class="flex justify-between mb-2">
            <span>当前 Agent: SubAgent-Architecture</span>
            <span class="text-blue-400 font-bold">37.9%</span>
          </div>
          <div class="text-gray-500 mb-2">48,567 / 128,000 <span class="text-[10px]">tokens</span></div>
          <div class="relative pt-1">
            <el-progress :percentage="37.9" :show-text="false" class="custom-progress" />
            <!-- Markers -->
            <div class="absolute top-0 bottom-0 left-[80%] border-l-2 border-yellow-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
            <div class="absolute top-0 bottom-0 left-[95%] border-l-2 border-red-500 z-10 h-full -mt-0.5" style="height: 12px;"></div>
            <div class="flex justify-between mt-1 text-[10px]">
              <span class="text-yellow-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-yellow-500"></div> 80% 警告线</span>
              <span class="text-red-500 flex items-center gap-1"><div class="w-1.5 h-1.5 rounded-full bg-red-500"></div> 95% 硬限制</span>
            </div>
          </div>
        </div>

        <!-- Watchdog Status (看门狗状态) -->
        <div class="text-xs text-gray-400 mb-2">看门狗状态</div>
        <div class="space-y-1 text-xs">
          <div class="flex items-center gap-4">
            <span class="text-gray-500 w-12">15:42:20</span>
            <el-tag size="small" type="success" effect="plain" class="!bg-transparent !border-[#2a2d35] w-16 text-center">OK</el-tag>
            <span class="text-gray-300">正常</span>
          </div>
          <div class="flex items-center gap-4">
            <span class="text-gray-500 w-12">15:41:15</span>
            <el-tag size="small" type="warning" effect="plain" class="!bg-transparent !border-[#2a2d35] w-16 text-center">Compress</el-tag>
            <span class="text-gray-300">压缩记忆</span>
          </div>
          <div class="flex items-center gap-4">
            <span class="text-gray-500 w-12">15:40:10</span>
            <el-tag size="small" type="success" effect="plain" class="!bg-transparent !border-[#2a2d35] w-16 text-center">OK</el-tag>
            <span class="text-gray-300">正常</span>
          </div>
        </div>
      </el-card>

      <!-- Mailbox -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">邮箱通知 (Mailbox)</div>
            <el-icon class="text-gray-500 cursor-pointer hover:text-white"><Close /></el-icon>
          </div>
        </template>
        <div class="flex justify-between text-xs mb-4">
          <span class="text-gray-400">未读消息 (5)</span>
          <span class="text-blue-400 cursor-pointer hover:text-blue-300">全部标记已读</span>
        </div>
        <div class="space-y-4">
          <div v-for="(mail, idx) in mails" :key="idx" class="flex gap-3 text-xs">
            <div class="mt-0.5 rounded-full p-1 shrink-0" :class="mail.bgClass">
              <el-icon :class="mail.iconColor"><component :is="mail.icon" /></el-icon>
            </div>
            <div class="flex-1 min-w-0">
              <div class="flex justify-between mb-1">
                <span class="font-bold" :class="mail.color">{{ mail.type }}</span>
                <span class="text-gray-500">{{ mail.time }}</span>
              </div>
              <div class="text-gray-300 truncate">{{ mail.content }}</div>
              <div class="text-gray-500 mt-1 truncate">{{ mail.subContent }}</div>
            </div>
          </div>
        </div>
      </el-card>

      <!-- System Health -->
      <el-card class="!border-[#2a2d35] !bg-[#1a1d24]">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm text-gray-200">系统状态 (System Health)</div>
            <el-icon class="text-gray-500 cursor-pointer hover:text-white"><Close /></el-icon>
          </div>
        </template>
        <div class="space-y-2 text-xs">
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><Coin /></el-icon>
            <div class="w-16 text-gray-300">Postgres</div>
            <div class="text-green-500 w-16">Connected</div>
            <div class="text-gray-500 flex-1">表行数: 24,567</div>
            <div class="text-gray-500">延迟: 2ms</div>
          </div>
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><DataLine /></el-icon>
            <div class="w-16 text-gray-300">Redis</div>
            <div class="text-green-500 w-16">Connected</div>
            <div class="text-gray-500 flex-1">Key 数量: 1,823</div>
            <div class="text-gray-500">延迟: 1ms</div>
          </div>
          <div class="flex items-center gap-4 bg-[#0f1115] p-2 rounded border border-[#2a2d35]">
            <el-icon class="text-gray-400 text-lg"><Connection /></el-icon>
            <div class="w-16 text-gray-300">LLM API</div>
            <div class="text-green-500 w-16">Connected</div>
            <div class="text-gray-500 flex-1">最近调用: 1.82s</div>
            <div class="text-gray-500">成功率: 99.2%</div>
          </div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import ExecutionLog from './components/ExecutionLog.vue'
import MemoryExplorer from './components/MemoryExplorer.vue'
import SkillSet from './components/SkillSet.vue'
import FilePreview from './components/FilePreview.vue'

const activeTab = ref('log')

const defaultProps = {
  children: 'children',
  label: 'label',
}

const roleData = ref([
  {
    label: 'MetaAgent',
    iconColor: 'text-green-500',
    status: 'active',
    statusType: 'success',
    active: true,
    isUser: false,
    children: [
      { 
        label: 'DomainAgent - CodeAnalysis', 
        iconColor: 'text-green-500', 
        status: 'active', 
        statusType: 'success',
        active: true,
        isUser: true,
        children: [
          { label: 'SubAgent - Parser', iconColor: 'text-green-500', status: 'active', statusType: 'success', active: true, isUser: false },
          { label: 'SubAgent - Architecture', iconColor: 'text-yellow-500', status: 'running', statusType: 'warning', active: true, isUser: false },
          { label: 'SubAgent - Dependency', iconColor: 'text-gray-500', status: 'pending', statusType: 'info', active: false, isUser: false }
        ]
      },
      {
        label: 'DomainAgent - Documentation',
        iconColor: 'text-gray-500',
        status: 'pending',
        statusType: 'info',
        active: false,
        isUser: true,
        children: [
          { label: 'Assistant - Writer', iconColor: 'text-gray-500', status: 'pending', statusType: 'info', active: false, isUser: false },
          { label: 'Assistant - Reviewer', iconColor: 'text-gray-500', status: 'pending', statusType: 'info', active: false, isUser: false }
        ]
      }
    ]
  }
])

const tasks = ref([
  { name: '解析项目结构', assignee: 'Parser', status: 'done' },
  { name: '分析依赖关系', assignee: 'Dependency', status: 'done' },
  { name: '构建架构图', assignee: 'Architecture', status: 'in_progress' },
  { name: '分析核心模块', assignee: 'Architecture', status: 'in_progress' },
  { name: '生成设计文档', assignee: 'Writer', status: 'pending' },
  { name: '文档审查', assignee: 'Reviewer', status: 'pending' },
  { name: '优化建议', assignee: 'MetaAgent', status: 'blocked' },
  { name: '最终确认', assignee: 'MetaAgent', status: 'pending' }
])

const mails = ref([
  { type: 'Milestone', color: 'text-green-500', icon: 'UserFilled', bgClass: 'bg-green-900/30', iconColor: 'text-green-500', time: '2分钟前', content: 'Architecture Analysis Complete', subContent: 'SubAgent-Architecture → MetaAgent' },

  { type: 'Request', color: 'text-blue-400', icon: 'Connection', bgClass: 'bg-blue-900/30', iconColor: 'text-blue-400', time: '5分钟前', content: '需要确认架构设计方向', subContent: 'DomainAgent-CodeAnalysis → MetaAgent' },
  { type: 'Dependency', color: 'text-yellow-500', icon: 'User', bgClass: 'bg-yellow-900/30', iconColor: 'text-yellow-500', time: '8分钟前', content: '等待 Redis 架构方案', subContent: 'SubAgent-Dependency → MetaAgent' },
  { type: 'Info', color: 'text-gray-400', icon: 'Document', bgClass: 'bg-gray-800/50', iconColor: 'text-gray-400', time: '10分钟前', content: '项目结构解析完成', subContent: 'SubAgent-Parser → DomainAgent-CodeAnalysis' },
  { type: 'Escalate', color: 'text-red-500', icon: 'WarningFilled', bgClass: 'bg-red-900/30', iconColor: 'text-red-500', time: '15分钟前', content: 'LLM 调用频率过高', subContent: 'Watchdog → MetaAgent' }
])
</script>

<style scoped>
:deep(.custom-tree .el-tree-node__content) {
  background-color: transparent !important;
  height: 32px;
}
:deep(.custom-tree .el-tree-node__content:hover) {
  background-color: #2a2d35 !important;
}
:deep(.custom-tree .el-tree-node:focus > .el-tree-node__content) {
  background-color: transparent !important;
}

:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 0;
}
:deep(.body-flex-1 .el-card__body > div) {
  padding: 16px;
}

:deep(.custom-progress .el-progress-bar__outer) {
  background-color: #2a2d35;
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: #1e3a8a;
}
</style>
