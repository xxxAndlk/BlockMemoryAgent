<template>
  <div class="flex-1 flex flex-col h-full bg-[#1a1d24]">
    <div class="p-3 border-b border-[#2a2d35] flex items-center gap-4 text-xs shrink-0">
      <div class="flex items-center gap-2">
        <span class="text-gray-400">Agent:</span>
        <el-select v-model="filterAgent" size="small" class="w-28 !bg-transparent filter-select">
          <el-option label="All" value="all" />
        </el-select>
      </div>
      <div class="flex items-center gap-2">
        <span class="text-gray-400">Kind:</span>
        <el-select v-model="filterKind" size="small" class="w-28 !bg-transparent filter-select">
          <el-option label="All" value="all" />
        </el-select>
      </div>
      <el-input v-model="searchLog" size="small" placeholder="搜索日志..." class="w-64 ml-auto !bg-[#0f1115] search-input">
        <template #suffix>
          <el-icon class="text-gray-500 hover:text-white cursor-pointer mr-2"><Search /></el-icon>
          <el-icon class="text-gray-500 hover:text-white cursor-pointer"><Filter /></el-icon>
        </template>
      </el-input>
    </div>

    <div class="flex-1 overflow-y-auto p-4 space-y-4 min-h-0">
      <div v-for="(log, index) in logs" :key="index" class="flex text-xs items-start gap-4">
        <div class="text-gray-500 w-16 shrink-0 pt-0.5">{{ log.time }}</div>
        <div class="w-32 shrink-0 pt-0.5" :class="log.agentColor">{{ log.agent }}</div>
        <div class="w-20 shrink-0 pt-0.5 flex justify-center">
          <el-tag size="small" :type="log.kindType" effect="plain" class="!bg-transparent !border-[#2a2d35] scale-90">{{ log.kind }}</el-tag>
        </div>
        <div class="flex-1 min-w-0">
          <div class="text-gray-300 break-words leading-relaxed pt-0.5" v-html="log.content"></div>
          
          <!-- Nested Details -->
          <div v-if="log.details" class="mt-2 ml-2 pl-3 border-l-2 border-[#2a2d35]">
            <div class="text-gray-400 mb-1 flex items-center gap-1 cursor-pointer hover:text-gray-300">
              <el-icon><ArrowDown /></el-icon> 详情 ({{ log.details.length }} 条记录)
            </div>
            <ul class="list-none space-y-1 text-gray-500">
              <li v-for="(detail, idx) in log.details" :key="idx" class="truncate">- {{ detail }}</li>
              <li class="text-gray-600">... 还有 {{ log.detailsTotal - log.details.length }} 条记录</li>
            </ul>
          </div>
        </div>
      </div>
    </div>
    
    <!-- Overall Progress Bar -->
    <div class="h-12 border-t border-[#2a2d35] flex items-center px-6 gap-4 shrink-0 bg-[#14161a]">
      <span class="text-xs text-gray-400 whitespace-nowrap">整体进度</span>
      <el-progress :percentage="75" :show-text="false" class="flex-1 custom-progress" />
      <span class="text-xs text-gray-400 whitespace-nowrap">75% (6/8)</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const filterAgent = ref('all')
const filterKind = ref('all')
const searchLog = ref('')

const logs = ref([
  { time: '15:42:10', agent: 'MetaAgent', agentColor: 'text-blue-400', kind: 'think', kindType: 'warning', content: '正在分析用户目标和任务分解策略...' },
  { time: '15:42:11', agent: 'MetaAgent', agentColor: 'text-blue-400', kind: 'intend', kindType: 'primary', content: '将创建 CodeAnalysis 领域的 DomainAgent' },
  { time: '15:42:12', agent: 'MetaAgent', agentColor: 'text-blue-400', kind: 'tool_call', kindType: 'primary', content: 'CreateDomainAgent({"domain":"CodeAnalysis"})' },
  { time: '15:42:12', agent: 'system', agentColor: 'text-gray-500', kind: 'tool_result', kindType: 'success', content: '<span class="text-green-500 mr-1">✓</span> DomainAgent-CodeAnalysis 创建成功 (ID: da_123)' },
  { time: '15:42:15', agent: 'DomainAgent-CodeAnalysis', agentColor: 'text-gray-300', kind: 'think', kindType: 'warning', content: '开始分析项目代码结构...' },
  { time: '15:42:16', agent: 'DomainAgent-CodeAnalysis', agentColor: 'text-gray-300', kind: 'tool_call', kindType: 'primary', content: 'SearchKnowledge({"query":"golang project structure analysis"})' },
  { 
    time: '15:42:17', 
    agent: 'system', 
    agentColor: 'text-gray-500', 
    kind: 'tool_result', 
    kindType: 'success', 
    content: '<span class="text-green-500 mr-1">✓</span> 找到 12 条相关知识记录',
    details: [
      'Go 项目结构最佳实践',
      'DDD 在 Go 项目中的应用'
    ],
    detailsTotal: 12
  },
  { time: '15:42:20', agent: 'SubAgent-Parser', agentColor: 'text-gray-300', kind: 'think', kindType: 'warning', content: '开始解析项目 AST...' },
  { time: '15:42:21', agent: 'SubAgent-Parser', agentColor: 'text-gray-300', kind: 'tool_call', kindType: 'primary', content: 'ReadFile {"path":"./go.mod"}' },
  { time: '15:42:21', agent: 'system', agentColor: 'text-gray-500', kind: 'tool_result', kindType: 'success', content: '<span class="text-green-500 mr-1">✓</span> 文件读取成功 (2.3KB)' },
  { time: '15:42:23', agent: 'SubAgent-Parser', agentColor: 'text-gray-300', kind: 'tool_call', kindType: 'primary', content: 'ASTParser {"content":"module github.com/example/project..."}' },
  { time: '15:42:24', agent: 'system', agentColor: 'text-gray-500', kind: 'tool_result', kindType: 'success', content: '<span class="text-green-500 mr-1">✓</span> AST 解析完成, 发现 45 个包, 156 个文件' },
  { time: '15:42:25', agent: 'SubAgent-Architecture', agentColor: 'text-gray-300', kind: 'think', kindType: 'warning', content: '基于解析结果进行架构分析...' },
  { time: '15:42:26', agent: 'SubAgent-Architecture', agentColor: 'text-gray-300', kind: 'tool_call', kindType: 'primary', content: 'CallLLM {"prompt":"请分析这个Go项目的架构设计..."}' },
  { time: '15:42:28', agent: 'system', agentColor: 'text-gray-500', kind: 'llm_result', kindType: 'success', content: '<span class="text-green-500 mr-1">✓</span> 分析完成 (耗时: 2.34s, tokens: 1,234)' },
  { time: '15:42:30', agent: 'DomainAgent-CodeAnalysis', agentColor: 'text-gray-300', kind: 'notify', kindType: 'primary', content: '架构分析阶段完成, 进度更新: 75%' }
])
</script>

<style scoped>
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
:deep(.custom-progress .el-progress-bar__outer) {
  background-color: #2a2d35;
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: #1e3a8a;
}
</style>