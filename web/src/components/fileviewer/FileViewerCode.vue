<script setup lang="ts">
// FileViewer 代码/文本/JSON 视图（TODO #26 阶段 B）：highlight.js 语法高亮 + 行号
//（整文件逐行高亮，行号与 hljs 标记不会错位；多行注释等跨行 token 按行独立高亮，
// 颜色可能不完整但绝不破版）。JSON 先 pretty-print 再按行高亮。
import { computed } from 'vue'
import hljs from '@/utils/hljs'
import { classifyFile } from '@/utils/fileKind'

const props = defineProps<{
  name: string
  content: string
}>()

/** JSON pretty-print（2 空格缩进），解析失败回退原文。 */
const effectiveText = computed(() => {
  const meta = classifyFile(props.name)
  if (meta.kind !== 'json') return props.content
  try {
    return JSON.stringify(JSON.parse(props.content), null, 2)
  } catch {
    return props.content
  }
})

const linesHtml = computed<string[]>(() => {
  const meta = classifyFile(props.name)
  const lang = meta.kind === 'json' ? 'json' : meta.hlLang
  const text = effectiveText.value
  if (text === '') return []
  const canHighlight = !!lang && hljs.getLanguage(lang)
  return text.split('\n').map(line => {
    if (!canHighlight || !line) return escapeHtml(line)
    try {
      return hljs.highlight(line, { language: lang! }).value
    } catch {
      return escapeHtml(line)
    }
  })
})

function escapeHtml(s: string) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}
</script>

<template>
  <div class="code-view">
    <div v-if="!linesHtml.length" class="code-empty">（空文件）</div>
    <div v-else class="code-scroll">
      <table class="code-table">
        <tbody>
          <tr v-for="(html, i) in linesHtml" :key="i">
            <td class="line-no">{{ i + 1 }}</td>
            <td class="line-code" v-html="html || '&nbsp;'"></td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.code-view {
  height: 100%;
  min-height: 0;
  display: flex;
  flex-direction: column;
  background: #11141a;
}
.code-empty {
  display: flex;
  align-items: center;
  justify-content: center;
  flex: 1;
  color: #6b7280;
  font-size: 12.5px;
}
.code-scroll {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 8px 0;
}
.code-table {
  border-collapse: collapse;
  width: 100%;
  font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
}
.line-no {
  width: 1%;
  min-width: 44px;
  padding: 0 12px 0 16px;
  text-align: right;
  color: #4b5261;
  user-select: none;
  vertical-align: top;
  white-space: nowrap;
}
.line-code {
  padding: 0 16px 0 8px;
  white-space: pre;
  color: #d6dae2;
}
/* 深色底高亮配色 */
:deep(.hljs-keyword), :deep(.hljs-selector-tag), :deep(.hljs-built_in) { color: #ff7b72; }
:deep(.hljs-string), :deep(.hljs-attr), :deep(.hljs-template-string) { color: #a5d6a7; }
:deep(.hljs-comment), :deep(.hljs-quote) { color: #7d8590; font-style: italic; }
:deep(.hljs-number), :deep(.hljs-literal) { color: #79c0ff; }
:deep(.hljs-title), :deep(.hljs-function), :deep(.hljs-title.function_) { color: #d2a8ff; }
:deep(.hljs-type), :deep(.hljs-class), :deep(.hljs-title.class_) { color: #ffa657; }
:deep(.hljs-meta), :deep(.hljs-variable), :deep(.hljs-name) { color: #79c0ff; }
:deep(.hljs-tag) { color: #a5d6a7; }
</style>
