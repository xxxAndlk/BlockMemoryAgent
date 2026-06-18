<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: Current Agent & Equipped Skills -->
    <div class="w-80 flex flex-col gap-4 overflow-y-auto">
      <el-card class="!border-dark-border !bg-dark-panel">
        <div class="text-sm">
          <div class="text-gray-400 mb-1">当前 Agent</div>
          <div class="font-bold text-primary text-lg">ArchitectureAgent</div>
          <div class="text-xs text-gray-500 mt-1">Domain: CodeAnalysis</div>
        </div>
      </el-card>

      <el-card class="!border-dark-border !bg-dark-panel flex-1">
        <template #header>
          <div class="font-bold text-sm">当前装配的 Skills</div>
        </template>
        
        <div class="space-y-3">
          <div v-for="(skill, idx) in equippedSkills" :key="idx" class="p-3 bg-dark-bg rounded border border-dark-border relative group">
            <div class="flex justify-between items-start">
              <div>
                <div class="font-bold text-sm text-gray-200">{{ skill.name }}</div>
                <div class="text-xs text-gray-500 mt-1">Tool: <span class="text-blue-400">{{ skill.tool }}</span></div>
                <div class="text-xs text-gray-500 mt-1">Cost: {{ skill.cost }}</div>
              </div>
              <el-button type="danger" link class="opacity-0 group-hover:opacity-100 transition-opacity">
                <el-icon><Delete /></el-icon>
              </el-button>
            </div>
            <div class="text-xs text-gray-400 mt-2 pt-2 border-t border-dark-border">
              描述: {{ skill.desc }}
            </div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Right Column: Available Skills & History -->
    <div class="flex-1 flex flex-col gap-4 overflow-hidden">
      <!-- Available Skills -->
      <el-card class="!border-dark-border !bg-dark-panel flex-1 overflow-y-auto">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm">技能候选池 (CodeAnalysis Domain)</div>
            <div class="flex gap-2">
              <el-input v-model="searchSkill" size="small" placeholder="搜索技能..." class="w-48">
                <template #prefix><el-icon><Search /></el-icon></template>
              </el-input>
              <el-button type="primary" size="small" plain>+ 新增 Skill</el-button>
            </div>
          </div>
        </template>
        
        <div class="grid grid-cols-3 gap-4">
          <div v-for="(skill, idx) in availableSkills" :key="idx" class="p-4 bg-dark-bg rounded border border-dark-border hover:border-primary transition-colors flex flex-col">
            <div class="font-bold text-sm text-gray-200 mb-1">{{ skill.name }}</div>
            <div class="text-xs text-gray-500 mb-1">Tool: <span class="text-blue-400">{{ skill.tool }}</span></div>
            <div class="text-xs text-gray-500 mb-4">Cost: {{ skill.cost }}</div>
            
            <div class="mt-auto">
              <el-button type="primary" size="small" class="w-full !bg-primary/20 !text-primary !border-primary hover:!bg-primary hover:!text-white transition-colors">
                <el-icon class="mr-1"><Plus /></el-icon> 装配
              </el-button>
            </div>
          </div>
        </div>
      </el-card>

      <!-- History -->
      <el-card class="!border-dark-border !bg-dark-panel h-64">
        <template #header>
          <div class="font-bold text-sm">装配历史记录</div>
        </template>
        <el-table :data="history" size="small" class="!bg-transparent" height="100%">
          <el-table-column prop="time" label="时间" width="160" class-name="text-gray-400" />
          <el-table-column prop="agent" label="Agent" width="180" />
          <el-table-column prop="skill" label="Skill" />
          <el-table-column prop="action" label="操作" width="100">
            <template #default="{ row }">
              <span :class="row.action === '装配' ? 'text-green-500' : 'text-red-500'">{{ row.action }}</span>
            </template>
          </el-table-column>
        </el-table>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const searchSkill = ref('')

const equippedSkills = ref([
  { name: 'Code Structure Analyzer', tool: 'ASTParser', cost: 3, desc: '分析代码结构和依赖关系' },
  { name: 'Architecture Pattern Recognizer', tool: 'PatternMatcher', cost: 4, desc: '识别架构模式和设计原则' },
  { name: 'Dependency Analyzer', tool: 'DepParser', cost: 2, desc: '分析模块间依赖关系' }
])

const availableSkills = ref([
  { name: 'Go Code Analyzer', tool: 'GoParser', cost: 3 },
  { name: 'Security Scanner', tool: 'SecScanner', cost: 5 },
  { name: 'Performance Profiler', tool: 'Profiler', cost: 4 },
  { name: 'Database Analyzer', tool: 'DBAnalyzer', cost: 2 },
  { name: 'API Documentation', tool: 'APIDocGen', cost: 3 },
  { name: 'Test Generator', tool: 'TestGen', cost: 2 }
])

const history = ref([
  { time: '2024-06-17 14:30', agent: 'ArchitectureAgent', skill: 'Code Structure Analyzer', action: '装配' },
  { time: '2024-06-17 14:28', agent: 'ArchitectureAgent', skill: 'Architecture Pattern Recognizer', action: '装配' },
  { time: '2024-06-17 14:25', agent: 'ArchitectureAgent', skill: 'Dependency Analyzer', action: '装配' }
])
</script>

<style scoped>
.body-flex-1 {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
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
</style>
