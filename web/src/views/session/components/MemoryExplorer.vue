<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: Snapshot -->
    <div class="w-80 flex flex-col gap-4 overflow-y-auto">
      <div class="flex items-center gap-2">
        <span class="text-sm text-gray-400 whitespace-nowrap">Agent 选择</span>
        <el-select v-model="selectedAgent" size="small" class="w-full">
          <el-option label="ArchitectureAgent (da_arch_001)" value="da_arch_001" />
        </el-select>
      </div>

      <el-card class="!border-dark-border !bg-dark-panel flex-1">
        <template #header>
          <div class="font-bold text-sm">记忆快照 (Snapshot)</div>
        </template>
        
        <div class="space-y-4">
          <!-- Key Summaries -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Key Summaries (关键摘要)</div>
            <ul class="list-disc pl-4 text-xs text-gray-300 space-y-1">
              <li>项目采用 DDD 架构模式</li>
              <li>核心模块包括用户、订单、支付</li>
              <li>使用了 Redis 作为缓存层</li>
              <li>数据库设计遵循范式规则</li>
              <li>接口设计遵循 RESTful 规范</li>
            </ul>
          </div>

          <!-- Open Issues -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Open Issues (未解决问题)</div>
            <ul class="list-disc pl-4 text-xs text-gray-300 space-y-1">
              <li>Redis 缓存穿透待优化</li>
              <li>消息队列方案待确认</li>
              <li>分布式事务方案待设计</li>
            </ul>
          </div>

          <!-- Local Variables -->
          <div>
            <div class="text-xs font-bold text-gray-400 mb-2">Local Variables (本地变量)</div>
            <el-table :data="variables" size="small" :show-header="true" class="!bg-transparent">
              <el-table-column prop="key" label="Key" width="100" />
              <el-table-column prop="value" label="Value" />
            </el-table>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Middle Column: Memory Search -->
    <div class="flex-1 flex flex-col gap-4 overflow-hidden">
      <div class="flex items-center gap-2">
        <span class="font-bold text-sm whitespace-nowrap">记忆检索 (Memory Search)</span>
        <el-input v-model="searchQuery" size="small" placeholder="golang ddd architecture" class="flex-1" />
        <el-button type="primary" size="small" class="!bg-primary">检索</el-button>
      </div>

      <el-card class="!border-dark-border !bg-dark-panel flex-1 overflow-y-auto">
        <div class="space-y-4">
          <div v-for="(episode, idx) in episodes" :key="idx" class="p-3 bg-dark-bg rounded border border-dark-border hover:border-primary transition-colors cursor-pointer">
            <div class="flex justify-between items-center mb-2">
              <span class="font-bold text-sm text-primary">{{ episode.title }}</span>
              <div class="flex gap-4 text-xs text-gray-500">
                <span>相关度: {{ episode.relevance }}</span>
                <span>{{ episode.time }}</span>
              </div>
            </div>
            <div class="text-xs text-gray-300">{{ episode.content }}</div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- Right Column: Memory Compression -->
    <div class="w-72 flex flex-col gap-4">
      <el-card class="!border-dark-border !bg-dark-panel h-full">
        <template #header>
          <div class="font-bold text-sm">压缩级别分布<br><span class="text-xs text-gray-400 font-normal">(Memory Compression)</span></div>
        </template>

        <div class="space-y-6 mt-4">
          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 0 (原始记忆)</span>
              <span class="text-gray-400">40%</span>
            </div>
            <el-progress :percentage="40" :show-text="false" />
            <div class="text-right text-xs text-gray-500 mt-1">128 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 1 (轻度压缩)</span>
              <span class="text-gray-400">25%</span>
            </div>
            <el-progress :percentage="25" :show-text="false" status="success" />
            <div class="text-right text-xs text-gray-500 mt-1">80 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 2 (中度压缩)</span>
              <span class="text-gray-400">20%</span>
            </div>
            <el-progress :percentage="20" :show-text="false" status="warning" />
            <div class="text-right text-xs text-gray-500 mt-1">64 条</div>
          </div>

          <div>
            <div class="flex justify-between text-xs mb-1">
              <span>Level 3 (高度压缩)</span>
              <span class="text-gray-400">15%</span>
            </div>
            <el-progress :percentage="15" :show-text="false" status="exception" />
            <div class="text-right text-xs text-gray-500 mt-1">48 条</div>
          </div>
        </div>

        <div class="mt-8 pt-4 border-t border-dark-border text-xs text-gray-400">
          总计：320 条记忆 | 占用：15.7MB
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const selectedAgent = ref('da_arch_001')
const searchQuery = ref('golang ddd architecture')

const variables = ref([
  { key: 'repo', value: 'yk_platform_backend' },
  { key: 'lang', value: 'go' },
  { key: 'framework', value: 'gin' },
  { key: 'cache', value: 'redis' },
  { key: 'db', value: 'postgres' }
])

const episodes = ref([
  { title: 'Episode #421', relevance: '0.93', time: '2024-01-15 14:30', content: '项目将采用 DDD 架构模式，分为领域层、应用层、基础设施层...' },
  { title: 'Episode #368', relevance: '0.89', time: '2024-01-10 09:15', content: 'Go 语言实现 DDD 的最佳实践，物理聚合根，降低耦合...' },
  { title: 'Episode #256', relevance: '0.82', time: '2024-01-05 16:45', content: '微服务架构下的 DDD 实践经验总结...' },
  { title: 'Episode #289', relevance: '0.85', time: '2023-12-28 11:20', content: '使用 Go 和 Gin 框架构建领域驱动的 Web 应用...' },
  { title: 'Episode #156', relevance: '0.80', time: '2023-11-15 10:30', content: '数据库设计与 DDD 聚合的对应关系...' }
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
}

:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}
:deep(.el-progress-bar__outer) {
  background-color: #2a2d35;
}
</style>
