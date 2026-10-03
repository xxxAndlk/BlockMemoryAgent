<script setup lang="ts">
// 「打开 ▾」下拉（TODO #26 阶段 F 逻辑抽出，FilePreview 面板与 FileViewer 全屏层共用）：
// 系统默认打开 + 每次展开重新扫码本机编辑器（后端 60s 缓存防抖）。自定义下拉
//（非 el-dropdown）——FileViewer 的全屏遮罩是局部深色作用域，el-dropdown 的弹出层
// teleport 到 body 会逃出该作用域，自定义菜单始终在按钮旁渲染。
import { ref } from 'vue'
import { ElMessage } from 'element-plus'
import { onClickOutside } from '@vueuse/core'
import { Position, ArrowDown } from '@element-plus/icons-vue'
import { listEditors, openInEditor, type EditorInfo } from '@/api/editors'

const props = defineProps<{
  /** 绝对路径（raw/open 端点只认绝对路径） */
  path: string
}>()

const open = ref(false)
const loading = ref(false)
const editors = ref<EditorInfo[]>([])
const rootRef = ref<HTMLElement | null>(null)

onClickOutside(rootRef, () => { open.value = false })

/** 每次展开重新扫码：用户装完编辑器不用刷新页面。 */
async function toggleMenu() {
  open.value = !open.value
  if (!open.value) return
  try {
    editors.value = await listEditors()
  } catch {
    editors.value = [] // 扫码失败降级：菜单只剩「系统默认打开」
  }
}

async function choose(editorId: string) {
  open.value = false
  const ed = editors.value.find(e => e.id === editorId)
  const label = ed ? ed.name : '系统默认程序'
  loading.value = true
  try {
    await openInEditor(editorId || null, props.path)
    ElMessage.success(`已用${label}打开`)
  } catch (e) {
    ElMessage.error(`打开失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div ref="rootRef" class="relative inline-block">
    <button type="button" class="fv-btn" :disabled="loading" @click="toggleMenu">
      <el-icon :size="13"><Position /></el-icon> 打开
      <el-icon :size="12"><ArrowDown /></el-icon>
    </button>
    <div v-if="open" class="editor-menu">
      <button type="button" class="editor-item" :title="path" @click="choose('')">
        <span class="text-[13px]">系统默认打开</span>
      </button>
      <template v-if="editors.length">
        <div class="editor-sep"></div>
        <button v-for="ed in editors" :key="ed.id" type="button" class="editor-item" :title="ed.exe"
                @click="choose(ed.id)">
          <span class="text-[13px]">{{ ed.name }}</span>
          <span class="editor-exe">{{ ed.exe }}</span>
        </button>
      </template>
    </div>
  </div>
</template>

<style scoped>
/* fv-btn 样式内聚在本组件：此前依赖 FileViewer.vue 的 scoped .fv-btn，而 Vue scoped
   样式不会穿透进子组件内部（子组件仅根节点带父 scope id），按钮在任何使用处都是
   浏览器默认样式——FilePreview 头部夹在 el-button 之间错位重叠（2026-10-01 用户实证）。
   尺寸对齐 el-button small（24px）；颜色走 bma token——FileViewer 深色作用域在
   .bma-viewer 覆写了同一组 token，两处使用各自跟随上下文主题。 */
.fv-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 24px;
  padding: 0 9px;
  border: 1px solid var(--bma-border, #2a2d35);
  border-radius: 4px;
  background: var(--bma-page, #1a1d24);
  color: var(--bma-text-2, #a8afba);
  font-size: 12px;
  white-space: nowrap;
  cursor: pointer;
  transition: color 0.12s ease, border-color 0.12s ease;
}
.fv-btn:hover:not(:disabled) {
  color: var(--bma-primary, #4f5dff);
  border-color: var(--bma-primary, #4f5dff);
}
.fv-btn:disabled {
  opacity: 0.6;
  cursor: default;
}
.editor-menu {
  position: absolute;
  top: calc(100% + 4px);
  right: 0;
  z-index: 30;
  min-width: 240px;
  max-width: 380px;
  padding: 4px;
  border-radius: 8px;
  background: var(--el-bg-color-overlay, #1f232b);
  border: 1px solid var(--el-border-color, #2a2d35);
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.35);
}
.editor-item {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 1px;
  width: 100%;
  padding: 6px 10px;
  border: none;
  border-radius: 6px;
  background: none;
  color: var(--bma-text, #e5e7eb);
  cursor: pointer;
  text-align: left;
}
.editor-item:hover {
  background: var(--bma-primary-soft, #232947);
}
.editor-exe {
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 11px;
  color: var(--bma-text-3, #8a8f99);
  font-family: 'JetBrains Mono', Consolas, monospace;
}
.editor-sep {
  height: 1px;
  margin: 4px 6px;
  background: var(--bma-border, #2a2d35);
}
</style>
