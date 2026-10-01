<script setup lang="ts">
import { computed } from 'vue'
import { ElMessage } from 'element-plus'
import { renderMd } from '@/utils/markdown'
import { useFileOpener } from '@/composables/fileOpener'

const props = defineProps<{
  content: string | null | undefined
}>()

const html = computed(() => renderMd(props.content))

/**
 * 本地文件链接点击（TODO #26 A）：data-bma-file 链接 = 打开全局全屏 FileViewer
 *（阶段 B 落地；面板形态留给 FilePreview 树内单击）。事件委托挂在容器上
 *（renderMd 输出的是静态 HTML）。无 fileOpener（会话外复用）时降级为提示路径文本。
 */
const fileOpener = useFileOpener()
function onClick(e: MouseEvent) {
  const a = (e.target as HTMLElement | null)?.closest?.('a[data-bma-file]')
  if (!a) return
  e.preventDefault()
  const p = a.getAttribute('data-bma-file') || ''
  if (fileOpener) fileOpener.openInViewer(p)
  else ElMessage.info('文件：' + p)
}
</script>

<template>
  <div class="markdown-body" v-html="html" @click="onClick"></div>
</template>

<style scoped>
.markdown-body :deep(p) { margin: 0; }
.markdown-body :deep(pre) {
  background: var(--bma-page);
  padding: 8px;
  border-radius: 4px;
  margin-top: 4px;
  overflow-x: auto;
  color: var(--bma-text);
}
.markdown-body :deep(code) { font-family: monospace; }
.markdown-body :deep(a) { color: var(--bma-primary); }
</style>
