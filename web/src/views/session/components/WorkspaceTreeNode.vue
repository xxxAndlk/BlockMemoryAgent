<template>
  <div class="tree-node" :data-tree-path="node.path">
    <!-- 目录行：箭头 + 文件夹图标 + 名；点击展开/收起 -->
    <div v-if="node.type === 'dir'" class="tree-row group" :class="{ 'tree-row-active': highlightPath === node.path }"
         :style="{ paddingLeft: 8 + depth * 14 + 'px' }"
         @click="$emit('toggle', node)">
      <el-icon class="tree-arrow shrink-0" :class="{ 'is-open': isOpen }">
        <ArrowRight />
      </el-icon>
      <el-icon class="shrink-0 text-amber-400"><Folder /></el-icon>
      <span class="tree-name">{{ node.name }}</span>
    </div>
    <!-- 展开后的子级（缩进层级线由子行左边线表达） -->
    <template v-if="node.type === 'dir' && isOpen">
      <template v-if="node.children && node.children.length">
        <WorkspaceTreeNode v-for="child in node.children" :key="child.path"
                           :node="child" :depth="depth + 1"
                           :expanded="expanded" :expand-all="expandAll"
                           :selected-path="selectedPath" :highlight-path="highlightPath"
                           :meta-of="metaOf" :format-size="formatSize"
                           @toggle="$emit('toggle', $event)"
                           @select="$emit('select', $event)"
                           @locate="$emit('locate', $event)" />
      </template>
      <div v-else class="tree-row tree-row-empty" :style="{ paddingLeft: 8 + (depth + 1) * 14 + 18 + 'px' }">
        <span class="text-[11px]">（空目录）</span>
      </div>
    </template>

    <!-- 文件行：类型图标 + 名 + 大小 -->
    <div v-else-if="node.type === 'file'" class="tree-row group"
         :class="{ 'tree-row-active': selectedPath === node.path, 'tree-row-highlight': highlightPath === node.path }"
         :style="{ paddingLeft: 8 + depth * 14 + 18 + 'px' }"
         @click="$emit('select', node)">
      <el-icon class="shrink-0" :class="metaOf(node.name).iconCls">
        <component :is="metaOf(node.name).icon" />
      </el-icon>
      <span class="tree-name" :class="selectedPath === node.path ? 'text-primary' : 'text-ink'">{{ node.name }}</span>
      <span class="tree-size">{{ formatSize(node.size || 0) }}</span>
      <el-tooltip content="在对话中定位" placement="top" :show-after="300">
        <el-icon class="tree-locate shrink-0 opacity-0 group-hover:opacity-100 transition-opacity"
                 @click.stop="$emit('locate', node)">
          <ChatDotSquare />
        </el-icon>
      </el-tooltip>
    </div>
  </div>
</template>

<script setup lang="ts">
// WorkspaceTreeNode.vue 工作区目录树递归节点（TODO #26 阶段 E）。
// 自绘递归（而非 el-tree）：缩进层级线/着色图标/大小列完全可控，且与面板 token 风格一致。
// 展开状态由父级 expanded 字典持有（跨过滤/重渲染保持）；expand-all 供筛选时强制展开命中链。
import { computed } from 'vue'
import { ArrowRight, ChatDotSquare, Folder } from '@element-plus/icons-vue'
import type { FSTreeNode } from '@/api/files'

interface NodeMeta {
  icon: unknown
  iconCls: string
}

const props = defineProps<{
  node: FSTreeNode
  depth: number
  /** dir path → 是否展开（expand-all 时忽略并视为展开） */
  expanded: Record<string, boolean>
  expandAll: boolean
  selectedPath: string
  highlightPath: string
  metaOf: (name: string) => NodeMeta
  formatSize: (n: number) => string
}>()

defineEmits<{
  toggle: [node: FSTreeNode]
  select: [node: FSTreeNode]
  locate: [node: FSTreeNode]
}>()

const isOpen = computed(() => props.expandAll || !!props.expanded[props.node.path])
</script>

<style scoped>
.tree-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding-top: 4px;
  padding-bottom: 4px;
  padding-right: 8px;
  border-radius: 6px;
  cursor: pointer;
  transition: background-color 0.12s ease;
}
.tree-row:hover {
  background: var(--bma-primary-soft);
}
/* 选中态 / 面包屑定位高亮 */
.tree-row-active {
  background: var(--bma-primary-soft);
}
.tree-row-highlight {
  outline: 1px dashed var(--bma-primary);
  outline-offset: -1px;
}
/* 缩进层级线：行左边一条细竖线（首层无） */
.tree-node .tree-row {
  position: relative;
}
.tree-arrow {
  color: var(--bma-text-3, #8a8f99);
  transition: transform 0.15s ease;
  font-size: 12px;
}
.tree-arrow.is-open {
  transform: rotate(90deg);
}
.tree-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
}
.tree-size {
  flex-shrink: 0;
  font-size: 11px;
  color: var(--bma-text-3, #8a8f99);
}
.tree-row-empty {
  cursor: default;
  color: var(--bma-text-3, #8a8f99);
}
.tree-row-empty:hover {
  background: transparent;
}
</style>
