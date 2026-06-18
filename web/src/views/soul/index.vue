<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: Current Soul Config -->
    <div class="flex-1 flex flex-col gap-4 overflow-y-auto">
      <el-card class="!border-dark-border !bg-dark-panel">
        <template #header>
          <div class="font-bold text-sm">当前人格配置</div>
        </template>
        
        <div class="text-sm">
          <div class="flex items-center gap-2 mb-2">
            <span class="text-gray-400">当前文件:</span>
            <span class="text-primary font-bold">soul.md</span>
          </div>
          <div class="text-xs text-gray-500 mb-6">更新时间: 2024-06-17 10:30</div>

          <div class="bg-dark-bg p-4 rounded border border-dark-border">
            <div class="font-bold text-gray-200 mb-2"># 严谨工程师人格</div>
            <div class="text-gray-300 mb-4">你是一位严谨的工程师，具有以下特质：</div>
            <ul class="list-decimal pl-5 text-gray-300 space-y-1">
              <li>追求代码质量和最佳实践</li>
              <li>注重细节和边界条件</li>
              <li>善于分析和解决问题</li>
              <li>基于分享和文档化</li>
            </ul>
            <div class="text-xs text-gray-500 mt-4 pt-4 border-t border-dark-border">
              系统预设提示词：分析、推演、严谨、逻辑...
            </div>
          </div>
        </div>
      </el-card>

      <el-card class="!border-dark-border !bg-dark-panel">
        <template #header>
          <div class="font-bold text-sm">参数配置</div>
        </template>
        
        <div class="space-y-6">
          <div>
            <div class="flex justify-between text-sm mb-2">
              <span class="text-gray-300">Temperature</span>
              <span class="text-primary">0.7</span>
            </div>
            <el-slider v-model="temperature" :min="0" :max="1" :step="0.1" />
          </div>
          
          <div>
            <div class="flex justify-between text-sm mb-2">
              <span class="text-gray-300">Creativity</span>
              <span class="text-primary">0.6</span>
            </div>
            <el-slider v-model="creativity" :min="0" :max="1" :step="0.1" />
          </div>
        </div>
      </el-card>
    </div>

    <!-- Right Column: Switch/Upload -->
    <div class="w-80 flex flex-col gap-4">
      <el-card class="!border-dark-border !bg-dark-panel h-full">
        <template #header>
          <div class="font-bold text-sm">切换人格</div>
        </template>
        
        <div class="space-y-6 mt-2">
          <div>
            <div class="text-xs text-gray-400 mb-2">选择预设人格</div>
            <el-select v-model="selectedSoul" class="w-full">
              <el-option label="严谨工程师" value="engineer" />
              <el-option label="创意设计师" value="designer" />
              <el-option label="产品经理" value="pm" />
            </el-select>
          </div>
          
          <div class="relative">
            <div class="absolute inset-0 flex items-center" aria-hidden="true">
              <div class="w-full border-t border-dark-border"></div>
            </div>
            <div class="relative flex justify-center">
              <span class="px-2 bg-dark-panel text-xs text-gray-500">或</span>
            </div>
          </div>

          <div>
            <div class="text-xs text-gray-400 mb-2">上传新的 soul.md</div>
            <el-upload
              class="w-full"
              drag
              action="#"
              :auto-upload="false"
            >
              <el-icon class="el-icon--upload text-gray-400"><upload-filled /></el-icon>
              <div class="el-upload__text text-gray-400">
                将文件拖到此处，或 <em class="text-primary">点击上传</em>
              </div>
            </el-upload>
            
            <div v-if="uploadedFile" class="mt-4 flex items-center justify-between p-2 bg-dark-bg border border-dark-border rounded text-sm">
              <span class="flex items-center gap-2">
                <el-icon class="text-blue-400"><Document /></el-icon>
                {{ uploadedFile }}
              </span>
              <el-icon class="text-gray-500 cursor-pointer hover:text-red-400"><Close /></el-icon>
            </div>
          </div>

          <div class="pt-4 mt-auto">
            <el-button type="primary" class="w-full !bg-primary">
              <el-icon class="mr-2"><Refresh /></el-icon> 更新人格
            </el-button>
          </div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const temperature = ref(0.7)
const creativity = ref(0.6)
const selectedSoul = ref('engineer')
const uploadedFile = ref('soul_new.md')
</script>

<style scoped>
:deep(.el-upload-dragger) {
  background-color: var(--el-fill-color-blank);
  border-color: var(--el-border-color);
}
:deep(.el-upload-dragger:hover) {
  border-color: var(--el-color-primary);
}
</style>
