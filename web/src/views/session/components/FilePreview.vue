<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: File Tree -->
    <div class="w-72 flex flex-col gap-4 overflow-y-auto">
      <el-card class="!border-dark-border !bg-dark-panel h-full flex flex-col">
        <template #header>
          <div class="font-bold text-sm">文件列表 (WriteFile 结果)</div>
        </template>
        
        <el-tree
          :data="fileTree"
          :props="defaultProps"
          default-expand-all
          highlight-current
          :current-node-key="'design.md'"
          node-key="id"
          class="!bg-transparent text-sm"
        >
          <template #default="{ node, data }">
            <span class="flex items-center gap-2">
              <el-icon v-if="data.children" class="text-yellow-500"><Folder /></el-icon>
              <el-icon v-else class="text-blue-400"><Document /></el-icon>
              <span :class="{ 'text-primary': node.isCurrent }">{{ node.label }}</span>
            </span>
          </template>
        </el-tree>
      </el-card>
    </div>

    <!-- Right Column: File Content -->
    <div class="flex-1 flex flex-col gap-4 overflow-hidden">
      <el-card class="!border-dark-border !bg-dark-panel h-full flex flex-col body-flex-1">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm flex items-center gap-2">
              <el-icon class="text-blue-400"><Document /></el-icon>
              design.md
            </div>
            <div class="flex gap-2">
              <el-button size="small" class="!bg-dark-bg !border-dark-border !text-gray-300">在 VS Code 中打开</el-button>
              <el-button type="primary" size="small" plain>下载文件</el-button>
            </div>
          </div>
        </template>
        
        <div class="bg-[#1e1e1e] p-4 rounded h-full overflow-y-auto font-mono text-sm text-gray-300 whitespace-pre-wrap">
# 项目设计文档

## 1. 架构概述

本项目采用 DDD 架构模式...

## 2. 模块设计

### 2.1 用户模块

```go
type User struct {
    ID    uint   `json:"id"`
    Name  string `json:"name"`
    Email string `json:"email"`
}
```

### 2.2 订单模块

...
        </div>

        <div class="mt-4 pt-2 border-t border-dark-border text-xs text-gray-500 flex justify-between">
          <span>文件路径：/docs/design.md | 大小：2.3KB</span>
          <span>创建时间：2024-06-17 14:32</span>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

const defaultProps = {
  children: 'children',
  label: 'label',
}

const fileTree = ref([
  {
    id: 'docs',
    label: '/docs/',
    children: [
      { id: 'design.md', label: 'design.md' },
      { id: 'architecture.md', label: 'architecture.md' },
      { id: 'api.md', label: 'api.md' }
    ]
  },
  {
    id: 'internal',
    label: '/internal/',
    children: [
      {
        id: 'handler',
        label: 'handler/',
        children: [
          { id: 'user.go', label: 'user.go' }
        ]
      },
      {
        id: 'service',
        label: 'service/',
        children: [
          { id: 'user_service.go', label: 'user_service.go' }
        ]
      }
    ]
  },
  {
    id: 'pkg',
    label: '/pkg/',
    children: [
      {
        id: 'model',
        label: 'model/',
        children: [
          { id: 'user.go', label: 'user.go' }
        ]
      },
      {
        id: 'repository',
        label: 'repository/',
        children: [
          { id: 'user_repo.go', label: 'user_repo.go' }
        ]
      }
    ]
  }
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
  padding: 0;
}

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

/* Scrollbar styles for code block */
pre::-webkit-scrollbar {
  height: 8px;
  width: 8px;
}
pre::-webkit-scrollbar-track {
  background: transparent;
}
pre::-webkit-scrollbar-thumb {
  background: #4b5563;
  border-radius: 4px;
}
pre::-webkit-scrollbar-thumb:hover {
  background: #6b7280;
}
</style>
