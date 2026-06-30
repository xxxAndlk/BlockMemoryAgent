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
          :current-node-key="selectedPath"
          node-key="id"
          class="!bg-transparent text-sm"
          @node-click="onNodeClick"
        >
          <template #default="{ node, data }">
            <span class="flex items-center gap-2">
              <el-icon v-if="data.children" class="text-yellow-500"><Folder /></el-icon>
              <el-icon v-else class="text-blue-400"><Document /></el-icon>
              <span :class="{ 'text-primary': node.isCurrent }">{{ node.label }}</span>
              <span v-if="data.size !== undefined" class="text-xs text-gray-500">({{ formatSize(data.size) }})</span>
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
              {{ currentFileName }}
            </div>
            <div class="flex gap-2">
              <el-button size="small" class="!bg-dark-bg !border-dark-border !text-gray-300">在 VS Code 中打开</el-button>
              <el-button type="primary" size="small" plain>下载文件</el-button>
            </div>
          </div>
        </template>

        <div v-if="loading" class="flex items-center justify-center h-full text-sm text-gray-500">
          <el-icon class="is-loading mr-2"><Loading /></el-icon> 加载中…
        </div>
        <div v-else-if="content" class="bg-[#1e1e1e] p-4 rounded h-full overflow-y-auto font-mono text-sm text-gray-300 whitespace-pre-wrap">{{ content }}</div>
        <div v-else class="flex items-center justify-center h-full text-sm text-gray-500">请在左侧选择文件查看内容</div>

        <div class="mt-4 pt-2 border-t border-dark-border text-xs text-gray-500 flex justify-between">
          <span>文件路径：{{ currentFilePath || '-' }} | 大小：{{ currentFileSize || '-' }}</span>
          <span>{{ currentFileDate || '' }}</span>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, computed } from 'vue'
import { listFiles, getFileContent, type FileItem } from '@/api/session'

const props = defineProps<{
  sessionId: string
}>()

const files = ref<FileItem[]>([])
const selectedPath = ref('')
const content = ref('')
const loading = ref(false)

const currentFile = computed(() => files.value.find(f => f.path === selectedPath.value))
const currentFileName = computed(() => currentFile.value?.name || '--')
const currentFilePath = computed(() => currentFile.value?.path || '')
const currentFileSize = computed(() => currentFile.value ? formatSize(currentFile.value.size) : '')
const currentFileDate = computed(() => '')

const defaultProps = { children: 'children', label: 'label' }

watch(() => props.sessionId, (id) => {
  if (id) loadFiles(id)
}, { immediate: true })

async function loadFiles(sessionID: string) {
  loading.value = true
  try {
    const res = await listFiles(sessionID)
    files.value = res.files || []
    if (files.value.length && !selectedPath.value) {
      selectedPath.value = files.value[0].path
    }
  } catch {
    files.value = []
  }
  loading.value = false
}

watch(selectedPath, async (path) => {
  if (!path) {
    content.value = ''
    return
  }
  try {
    const res = await getFileContent(path)
    content.value = res.content
  } catch {
    content.value = '无法读取文件内容'
  }
})

const fileTree = ref([
  {
    id: 'files',
    label: '会话输出文件',
    children: [] as any[]
  }
])

watch(files, (fs) => {
  fileTree.value[0].children = fs.map(f => ({
    id: f.path,
    label: f.name,
    path: f.path,
    size: f.size,
  }))
}, { immediate: true })

function onNodeClick(data: any) {
  if (data.path) selectedPath.value = data.path
}

function formatSize(bytes: number) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(1) + ' MB'
}
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
