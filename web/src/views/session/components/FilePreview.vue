<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- 左列：文件列表（精美文件行） -->
    <div class="w-72 flex flex-col overflow-hidden">
      <el-card class="!border-line !bg-card h-full flex flex-col file-list-card" shadow="never">
        <template #header>
          <div class="font-bold text-sm flex items-center gap-2">
            <el-icon class="text-primary"><FolderOpened /></el-icon>
            会话输出文件
            <span class="text-xs text-ink-3 font-normal">({{ files.length }})</span>
          </div>
        </template>

        <div v-if="!files.length && !listLoading" class="text-xs text-ink-3 py-8 text-center">
          暂无 WriteFile 产物
        </div>
        <div v-else class="flex-1 overflow-y-auto -mx-3 px-1 space-y-0.5">
          <div v-for="f in files" :key="f.path"
               class="file-row group"
               :class="{ 'file-row-active': f.path === selectedPath }"
               @click="selectedPath = f.path">
            <el-icon class="shrink-0" :class="fileMeta(f.name).iconCls">
              <component :is="fileMeta(f.name).icon" />
            </el-icon>
            <div class="min-w-0 flex-1">
              <div class="truncate text-[13px]" :class="f.path === selectedPath ? 'text-primary' : 'text-ink'">
                {{ f.name }}
              </div>
              <div class="text-[11px] text-ink-3">{{ formatSize(f.size) }}</div>
            </div>
          </div>
        </div>
      </el-card>
    </div>

    <!-- 右列：Kimi Work 风格预览区 -->
    <div class="flex-1 flex flex-col overflow-hidden">
      <el-card class="!border-line !bg-card h-full flex flex-col body-flex-1" shadow="never">
        <template #header>
          <div class="flex justify-between items-center gap-3">
            <div class="min-w-0 flex items-center gap-2">
              <el-icon :class="currentMeta.iconCls"><component :is="currentMeta.icon" /></el-icon>
              <span class="font-bold text-sm truncate">{{ currentFileName }}</span>
              <el-tag v-if="selectedPath" size="small" effect="plain" class="!bg-transparent !border-line shrink-0">
                {{ currentMeta.label }}
              </el-tag>
              <span v-if="currentFile" class="text-xs text-ink-3 shrink-0">{{ formatSize(currentFile.size) }}</span>
            </div>
            <div v-if="selectedPath" class="flex gap-2 shrink-0">
              <el-button size="small" class="!bg-page !border-line !text-ink"
                         tag="a" :href="vscodeUrl" target="_blank" rel="noreferrer">
                <el-icon class="mr-1"><Position /></el-icon> 在 VS Code 中打开
              </el-button>
              <el-button type="primary" size="small" plain tag="a" :href="downloadUrl" rel="noreferrer">
                <el-icon class="mr-1"><Download /></el-icon> 下载文件
              </el-button>
            </div>
          </div>
        </template>

        <!-- 加载中 -->
        <div v-if="loading" class="flex items-center justify-center h-full text-sm text-ink-2">
          <el-icon class="is-loading mr-2"><Loading /></el-icon> 加载中…
        </div>

        <!-- 未选择 -->
        <div v-else-if="!selectedPath" class="flex flex-col items-center justify-center h-full text-sm text-ink-3 gap-2">
          <el-icon class="text-3xl"><Document /></el-icon>
          请在左侧选择文件查看内容
        </div>

        <!-- 图片：raw 端点内联渲染，深色衬底 -->
        <div v-else-if="currentMeta.kind === 'image'" class="preview-body media-body">
          <img :src="rawUrl" :alt="currentFileName" class="preview-img" />
        </div>

        <!-- Markdown：MarkdownRenderer 渲染 -->
        <div v-else-if="currentMeta.kind === 'markdown'" class="preview-body">
          <div v-if="truncated" class="truncate-banner">文件过大，仅显示前 {{ truncatedText }}，请下载查看</div>
          <MarkdownRenderer :content="content" class="text-sm" />
        </div>

        <!-- JSON：pretty-print，失败回退纯文本 -->
        <div v-else-if="currentMeta.kind === 'json'" class="preview-body">
          <div v-if="truncated" class="truncate-banner">文件过大，仅显示前 {{ truncatedText }}，请下载查看</div>
          <pre class="code-pre" v-html="jsonHtml"></pre>
        </div>

        <!-- 代码/文本：highlight.js 语法高亮 -->
        <div v-else-if="currentMeta.kind === 'code' || currentMeta.kind === 'text'" class="preview-body">
          <div v-if="truncated" class="truncate-banner">文件过大，仅显示前 {{ truncatedText }}，请下载查看</div>
          <pre class="code-pre" v-html="codeHtml"></pre>
        </div>

        <!-- 二进制/未知：不支持预览 -->
        <div v-else class="flex flex-col items-center justify-center h-full text-sm text-ink-3 gap-3">
          <el-icon class="text-3xl"><WarningFilled /></el-icon>
          <span>该文件类型暂不支持预览</span>
          <el-button type="primary" size="small" plain tag="a" :href="downloadUrl" rel="noreferrer">
            <el-icon class="mr-1"><Download /></el-icon> 下载文件
          </el-button>
        </div>

        <!-- 底部路径条 -->
        <div v-if="selectedPath" class="mt-4 pt-2 border-t border-line text-xs text-ink-2 flex justify-between shrink-0">
          <span class="truncate">文件路径：{{ currentFilePath || '-' }}</span>
          <span v-if="currentFile" class="shrink-0 ml-4">大小：{{ formatSize(currentFile.size) }}</span>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, computed } from 'vue'
import hljs from 'highlight.js/lib/core'
import javascript from 'highlight.js/lib/languages/javascript'
import typescript from 'highlight.js/lib/languages/typescript'
import jsonLang from 'highlight.js/lib/languages/json'
import python from 'highlight.js/lib/languages/python'
import go from 'highlight.js/lib/languages/go'
import java from 'highlight.js/lib/languages/java'
import cpp from 'highlight.js/lib/languages/cpp'
import xml from 'highlight.js/lib/languages/xml'
import css from 'highlight.js/lib/languages/css'
import scss from 'highlight.js/lib/languages/scss'
import bash from 'highlight.js/lib/languages/bash'
import yaml from 'highlight.js/lib/languages/yaml'
import markdown from 'highlight.js/lib/languages/markdown'
import sql from 'highlight.js/lib/languages/sql'
import ini from 'highlight.js/lib/languages/ini'
import {
  PictureFilled, Document, Notebook, Coin, Tickets, QuestionFilled,
  FolderOpened, Loading, Download, Position, WarningFilled,
} from '@element-plus/icons-vue'
import { listFiles, getFileContent, type FileItem } from '@/api/files'
import MarkdownRenderer from '@/components/MarkdownRenderer.vue'

// 按需注册常用语言子集，控制打包体积（未覆盖语言走纯文本）。
hljs.registerLanguage('javascript', javascript)
hljs.registerLanguage('typescript', typescript)
hljs.registerLanguage('json', jsonLang)
hljs.registerLanguage('python', python)
hljs.registerLanguage('go', go)
hljs.registerLanguage('java', java)
hljs.registerLanguage('cpp', cpp)
hljs.registerLanguage('xml', xml)
hljs.registerLanguage('css', css)
hljs.registerLanguage('scss', scss)
hljs.registerLanguage('bash', bash)
hljs.registerLanguage('yaml', yaml)
hljs.registerLanguage('markdown', markdown)
hljs.registerLanguage('sql', sql)
hljs.registerLanguage('ini', ini)

const props = defineProps<{
  sessionId: string
}>()

type FileKind = 'image' | 'markdown' | 'json' | 'code' | 'text' | 'other'

interface FileMeta {
  kind: FileKind
  label: string
  icon: unknown
  iconCls: string
  /** highlight.js 语言名；undefined = 不高亮（纯文本） */
  hlLang?: string
}

const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'ico'])
const MARKDOWN_EXTS = new Set(['md', 'markdown'])
const CODE_EXTS: Record<string, string> = {
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  ts: 'typescript', tsx: 'typescript', mts: 'typescript',
  py: 'python', go: 'go', java: 'java',
  c: 'cpp', cc: 'cpp', cpp: 'cpp', h: 'cpp', hpp: 'cpp', cs: 'cpp', rs: 'rust',
  html: 'xml', htm: 'xml', vue: 'xml', xml: 'xml',
  css: 'css', scss: 'scss', less: 'scss',
  sh: 'bash', bash: 'bash', zsh: 'bash',
  yml: 'yaml', yaml: 'yaml',
  sql: 'sql', ini: 'ini', toml: 'ini',
}
const TEXT_EXTS = new Set(['txt', 'log', 'csv', 'env'])

// hljs 未注册的语言（rust 等）不传入，走纯文本展示。
const HLJS_SUPPORTED = new Set(['javascript', 'typescript', 'python', 'go', 'java', 'cpp', 'xml', 'css', 'scss', 'bash', 'yaml', 'sql', 'ini'])

const files = ref<FileItem[]>([])
const listLoading = ref(false)
const selectedPath = ref('')
const content = ref('')
const truncated = ref(false)
const truncatedSize = ref(0)
const loading = ref(false)

const currentFile = computed(() => files.value.find(f => f.path === selectedPath.value))
const currentFileName = computed(() => currentFile.value?.name || '--')
const currentFilePath = computed(() => currentFile.value?.path || '')

const currentMeta = computed<FileMeta>(() =>
  currentFile.value ? fileMeta(currentFile.value.name) : fileMeta(''),
)

/** 原始字节端点：图片内联渲染与下载共用（后端限 WriteFile 产物 + 20MB）。 */
const rawUrl = computed(() => `/api/files/raw?path=${encodeURIComponent(selectedPath.value)}`)
const downloadUrl = computed(() => `${rawUrl.value}&download=1`)
const vscodeUrl = computed(() => `vscode://file/${selectedPath.value}`)

/** 截断提示文案：按完整大小换算「前 X MB/KB」。 */
const truncatedText = computed(() => formatSize(truncatedSize.value || content.value.length))

function fileMeta(name: string): FileMeta {
  const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : ''
  if (IMAGE_EXTS.has(ext)) {
    return { kind: 'image', label: '图片', icon: PictureFilled, iconCls: 'text-violet-400' }
  }
  if (MARKDOWN_EXTS.has(ext)) {
    return { kind: 'markdown', label: 'Markdown', icon: Notebook, iconCls: 'text-blue-400' }
  }
  if (ext === 'json') {
    return { kind: 'json', label: 'JSON', icon: Coin, iconCls: 'text-amber-400' }
  }
  if (ext in CODE_EXTS) {
    const lang = CODE_EXTS[ext]
    return {
      kind: 'code', label: `代码 · ${ext}`, icon: Document,
      iconCls: 'text-sky-400',
      hlLang: HLJS_SUPPORTED.has(lang) ? lang : undefined,
    }
  }
  if (TEXT_EXTS.has(ext) || ext === '') {
    return { kind: 'text', label: '文本', icon: Tickets, iconCls: 'text-ink-2' }
  }
  return { kind: 'other', label: ext ? ext.toUpperCase() : '文件', icon: QuestionFilled, iconCls: 'text-ink-3' }
}

watch(() => props.sessionId, (id) => {
  if (id) loadFiles(id)
}, { immediate: true })

async function loadFiles(sessionID: string) {
  listLoading.value = true
  selectedPath.value = ''
  try {
    const res = await listFiles(sessionID)
    files.value = res.files || []
    if (files.value.length) {
      selectedPath.value = files.value[0].path
    }
  } catch {
    files.value = []
  }
  listLoading.value = false
}

watch(selectedPath, async (path) => {
  content.value = ''
  truncated.value = false
  if (!path || currentMeta.value.kind === 'image') return // 图片走 raw 端点，不取文本
  loading.value = true
  try {
    const res = await getFileContent(path)
    content.value = res.content
    truncated.value = !!res.truncated
    truncatedSize.value = res.size ?? res.content.length
  } catch {
    content.value = '无法读取文件内容'
  } finally {
    loading.value = false
  }
})

/** JSON pretty-print（2 空格缩进），解析失败回退纯文本展示。 */
const jsonHtml = computed(() => {
  try {
    return hljs.highlight(JSON.stringify(JSON.parse(content.value), null, 2),
      { language: 'json' }).value
  } catch {
    return escapeHtml(content.value)
  }
})

/** 代码高亮：注册了语言的按语言高亮，其余纯文本转义展示。 */
const codeHtml = computed(() => {
  const lang = currentMeta.value.hlLang
  if (lang && hljs.getLanguage(lang)) {
    try {
      return hljs.highlight(content.value, { language: lang }).value
    } catch {
      // 高亮失败（如非法字符）回退纯文本
    }
  }
  return escapeHtml(content.value)
})

function escapeHtml(s: string) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

function formatSize(bytes: number) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(1) + ' MB'
}
</script>

<style scoped>
/* 文件行：hover 高亮 + 选中态（Kimi Work 列表风格） */
.file-list-card :deep(.el-card__body) {
  flex: 1;
  overflow-y: auto;
  padding-top: 8px;
  padding-bottom: 8px;
}
.file-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 8px;
  border-radius: 6px;
  cursor: pointer;
  transition: background-color 0.12s ease;
}
.file-row:hover {
  background: var(--bma-primary-soft);
}
.file-row-active {
  background: var(--bma-primary-soft);
}

/* 预览主体 */
.preview-body {
  flex: 1;
  overflow: auto;
  background: var(--bma-page);
  border-radius: 6px;
  padding: 12px 16px;
  min-height: 0;
}
.media-body {
  display: flex;
  align-items: center;
  justify-content: center;
  background: #16181d; /* 深色衬底，图片边界一目了然 */
  padding: 24px;
}
.preview-img {
  max-width: 100%;
  max-height: 100%;
  object-fit: contain;
  border-radius: 4px;
  box-shadow: 0 4px 24px rgba(0, 0, 0, 0.35);
}

/* 代码/JSON pre */
.code-pre {
  margin: 0;
  font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre;
  overflow: auto;
  color: var(--bma-text);
}

/* 截断提示条 */
.truncate-banner {
  margin-bottom: 8px;
  padding: 6px 10px;
  border-radius: 6px;
  font-size: 12px;
  color: #b45309;
  background: rgba(245, 158, 11, 0.12);
  border: 1px solid rgba(245, 158, 11, 0.35);
}

/* 滚动条 */
pre::-webkit-scrollbar, .preview-body::-webkit-scrollbar {
  height: 8px;
  width: 8px;
}
pre::-webkit-scrollbar-track, .preview-body::-webkit-scrollbar-track {
  background: transparent;
}
pre::-webkit-scrollbar-thumb, .preview-body::-webkit-scrollbar-thumb {
  background: var(--bma-text-3);
  border-radius: 4px;
}
pre::-webkit-scrollbar-thumb:hover, .preview-body::-webkit-scrollbar-thumb:hover {
  background: var(--bma-text-2);
}

/* highlight.js 配色：跟随设计 token（浅色底用默认 github 风，深色底反色） */
:deep(.hljs-keyword), :deep(.hljs-selector-tag), :deep(.hljs-built_in) { color: #d73a49; }
:deep(.hljs-string), :deep(.hljs-attr), :deep(.hljs-template-string) { color: #22863a; }
:deep(.hljs-comment), :deep(.hljs-quote) { color: #6a737d; font-style: italic; }
:deep(.hljs-number), :deep(.hljs-literal) { color: #005cc5; }
:deep(.hljs-title), :deep(.hljs-function), :deep(.hljs-title.function_) { color: #6f42c1; }
:deep(.hljs-type), :deep(.hljs-class), :deep(.hljs-title.class_) { color: #e36209; }
:deep(.hljs-meta), :deep(.hljs-variable), :deep(.hljs-name) { color: #005cc5; }
:deep(.hljs-tag) { color: #22863a; }
</style>
