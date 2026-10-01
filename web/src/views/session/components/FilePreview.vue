<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- 左列：工作区目录树（无工作区会话降级为 WriteFile 产物列表） -->
    <div class="w-72 flex flex-col overflow-hidden">
      <el-card class="!border-line !bg-card h-full flex flex-col file-list-card" shadow="never">
        <template #header>
          <div class="font-bold text-sm flex items-center gap-2">
            <el-icon class="text-primary"><FolderOpened /></el-icon>
            <span class="truncate">{{ treeMode ? rootName : '会话输出文件' }}</span>
            <span v-if="treeMode && treeTruncated" class="text-[11px] text-amber-600 font-normal shrink-0"
                  title="目录过大，仅展示前 5000 个节点">已截断</span>
            <span v-if="!treeMode" class="text-xs text-ink-3 font-normal">({{ files.length }})</span>
          </div>
        </template>

        <!-- 树模式：筛选框 + 递归树 -->
        <template v-if="treeMode">
          <el-input ref="filterInputRef" v-model="filterText" size="small" clearable
                    placeholder="筛选文件…" class="!mb-2 tree-filter"
                    @input="onFilterInput">
            <template #prefix>
              <el-icon class="text-ink-3"><Search /></el-icon>
            </template>
          </el-input>
          <div v-if="treeLoading" class="text-xs text-ink-3 py-8 text-center">
            <el-icon class="is-loading mr-1"><Loading /></el-icon>加载文件树…
          </div>
          <div v-else-if="!visibleChildren.length" class="text-xs text-ink-3 py-8 text-center">
            {{ filterText ? '无匹配文件' : '（空工作区）' }}
          </div>
          <div v-else class="flex-1 overflow-y-auto -mx-3 px-1">
            <WorkspaceTreeNode v-for="child in visibleChildren" :key="child.path"
                               :node="child" :depth="0"
                               :expanded="expanded" :expand-all="filtering"
                               :selected-path="activeTabPath" :highlight-path="highlightPath"
                               :meta-of="fileMeta" :format-size="formatSize"
                               @toggle="toggleDir" @select="openFile" @locate="locateInChat" />
          </div>
        </template>

        <!-- 降级模式：原 WriteFile 产物平铺列表 -->
        <template v-else>
          <div v-if="!files.length && !listLoading" class="text-xs text-ink-3 py-8 text-center">
            暂无 WriteFile 产物
          </div>
          <div v-else class="flex-1 overflow-y-auto -mx-3 px-1 space-y-0.5">
            <div v-for="f in files" :key="f.path"
                 class="file-row group"
                 :class="{ 'file-row-active': f.path === activeTabPath }"
                 @click="openFile(f)">
              <el-icon class="shrink-0" :class="fileMeta(f.name).iconCls">
                <component :is="fileMeta(f.name).icon" />
              </el-icon>
              <div class="min-w-0 flex-1">
                <div class="truncate text-[13px]" :class="f.path === activeTabPath ? 'text-primary' : 'text-ink'">
                  {{ f.name }}
                </div>
                <div class="text-[11px] text-ink-3">{{ formatSize(f.size) }}</div>
              </div>
            </div>
          </div>
        </template>
      </el-card>
    </div>

    <!-- 右列：Kimi Work 风格预览区（tab 多开） -->
    <div class="flex-1 flex flex-col overflow-hidden min-w-0">
      <el-card class="!border-line !bg-card h-full flex flex-col body-flex-1" shadow="never">
        <template #header>
          <div class="flex justify-between items-center gap-3">
            <!-- 面包屑：根名 > 子目录 > 文件名，目录段可点击（树内定位高亮） -->
            <div class="min-w-0 flex items-center gap-1 text-sm flex-1 breadcrumb-bar">
              <template v-if="activeTab">
                <template v-for="(seg, i) in breadcrumb" :key="i">
                  <span v-if="i > 0" class="crumb-sep">›</span>
                  <button v-if="seg.path" class="crumb-link truncate" :title="seg.path"
                          @click="revealInTree(seg.path)">{{ seg.name }}</button>
                  <span v-else class="font-bold truncate" :title="activeTab.path">{{ seg.name }}</span>
                </template>
              </template>
              <span v-else class="font-bold text-ink-2">文件预览</span>
              <span v-if="activeDirty" class="dirty-dot shrink-0" title="有未保存的修改">●</span>
              <el-tag v-if="activeTab" size="small" effect="plain" class="!bg-transparent !border-line shrink-0">
                {{ activeMeta.label }}
              </el-tag>
              <span v-if="activeTab?.editing" class="text-xs text-ink-3 shrink-0">编辑中</span>
              <span v-if="activeTab" class="text-xs text-ink-3 shrink-0">{{ formatSize(activeTab.size) }}</span>
            </div>
            <div v-if="activeTab" class="flex gap-2 shrink-0">
              <!-- 编辑态：保存（Ctrl+S）/ 取消 -->
              <template v-if="activeTab.editing">
                <el-button type="primary" size="small" :loading="saving" @click="saveEdit(false)">
                  <el-icon v-if="!saving" class="mr-1"><Select /></el-icon> 保存
                </el-button>
                <el-button size="small" :disabled="saving" @click="cancelEdit">取消</el-button>
              </template>
              <!-- 只读态：文本类文件给「编辑」入口（截断文件不可编辑，避免存丢内容） -->
              <el-button v-else-if="isTextKind && !activeTab.truncated" size="small"
                         class="!bg-page !border-line !text-ink" @click="enterEdit">
                <el-icon class="mr-1"><EditPen /></el-icon> 编辑
              </el-button>
              <!-- 文本类文件：「打开 ▾」下拉（系统默认 + 扫码到的编辑器，抽为共享组件） -->
              <OpenInEditorMenu v-if="isTextKind && !activeTab.editing" :path="activeTab.path" />
              <!-- 图片/视频等非文本类：保持原 VS Code 协议按钮 -->
              <el-button v-else-if="!activeTab.editing" size="small" class="!bg-page !border-line !text-ink"
                         tag="a" :href="vscodeUrl" target="_blank" rel="noreferrer">
                <el-icon class="mr-1"><Position /></el-icon> 在 VS Code 中打开
              </el-button>
              <!-- 在文件夹中显示（TODO #26 C）：树模式与降级模式都可用（产物都在默认工作区） -->
              <el-button size="small" class="!bg-page !border-line !text-ink" :loading="revealing"
                         @click="revealActive">
                <el-icon v-if="!revealing" class="mr-1"><Folder /></el-icon> 在文件夹中显示
              </el-button>
              <!-- 全屏预览（TODO #26 B）：进全局 FileViewer 只读查看；文本编辑态仍留面板 -->
              <el-button size="small" class="!bg-page !border-line !text-ink" @click="openFullscreen">
                <el-icon class="mr-1"><FullScreen /></el-icon> 全屏
              </el-button>
              <el-button type="primary" size="small" plain tag="a" :href="downloadUrl" rel="noreferrer">
                <el-icon class="mr-1"><Download /></el-icon> 下载文件
              </el-button>
            </div>
          </div>
        </template>

        <!-- Tab 栏：文件名 + 关闭 ×，+ 回到树 -->
        <div v-if="tabs.length" class="tab-strip shrink-0">
          <div v-for="t in tabs" :key="t.path"
               class="tab-item" :class="{ 'tab-active': t.path === activeTabPath }"
               :title="t.path" @click="activateTab(t)">
            <el-icon :size="13" :class="fileMeta(t.name).iconCls">
              <component :is="fileMeta(t.name).icon" />
            </el-icon>
            <span class="tab-name">{{ t.name }}</span>
            <span v-if="dirtyOf(t)" class="dirty-dot" title="有未保存的修改">●</span>
            <el-icon class="tab-close" :size="12" @click.stop="closeTab(t)"><Close /></el-icon>
          </div>
          <el-tooltip content="回到文件树" placement="bottom">
            <button class="tab-plus" @click="backToTree"><el-icon :size="14"><Plus /></el-icon></button>
          </el-tooltip>
        </div>

        <!-- 无 tab：空态 -->
        <div v-if="!activeTab"
             class="flex flex-col items-center justify-center flex-1 text-sm text-ink-3 gap-2 min-h-0">
          <el-icon class="text-3xl"><Document /></el-icon>
          {{ treeMode ? '从左侧文件树选择文件查看内容' : '请在左侧选择文件查看内容' }}
        </div>

        <!-- 每个 tab 独立 DOM（v-show 保留滚动/编辑态） -->
        <div v-for="t in tabs" v-show="activeTab && activeTab.path === t.path" :key="t.path"
             class="tab-pane flex-1 min-h-0 flex flex-col">
          <!-- 加载中 -->
          <div v-if="t.loading" class="flex items-center justify-center flex-1 text-sm text-ink-2">
            <el-icon class="is-loading mr-2"><Loading /></el-icon> 加载中…
          </div>

          <!-- 加载失败（与真空文件区分：只有请求报错才显示错误文案） -->
          <div v-else-if="t.loadError" class="flex items-center justify-center flex-1 text-sm text-ink-3 gap-2">
            <el-icon><WarningFilled /></el-icon> {{ t.loadError }}
          </div>

          <!-- 编辑态：等宽 textarea（markdown/json 同样原样保存，不强制校验） -->
          <div v-else-if="t.editing" class="preview-body edit-body">
            <textarea v-model="t.editContent" class="edit-textarea" spellcheck="false"
                      :disabled="saving" @keydown="onEditKeydown($event, t)"></textarea>
          </div>

          <!-- 图片：raw 端点内联渲染，深色衬底 -->
          <div v-else-if="fileMeta(t.name).kind === 'image'" class="preview-body media-body">
            <img :src="rawUrlOf(t)" :alt="t.name" class="preview-img" />
          </div>

          <!-- Markdown：MarkdownRenderer 渲染 -->
          <div v-else-if="fileMeta(t.name).kind === 'markdown'" class="preview-body">
            <div v-if="t.truncated" class="truncate-banner">
              <span>文件过大，仅显示前 {{ truncatedTextOf(t) }}</span>
              <el-button v-if="t.nextOffset != null" size="small" type="primary" text
                         :loading="t.loadingMore" @click="loadMore(t)">加载更多</el-button>
              <el-button size="small" text tag="a" :href="rawUrlOf(t) + '&download=1'" rel="noreferrer">下载查看</el-button>
            </div>
            <MarkdownRenderer :content="t.content" class="text-sm" />
          </div>

          <!-- JSON：pretty-print，失败回退纯文本 -->
          <div v-else-if="fileMeta(t.name).kind === 'json'" class="preview-body">
            <div v-if="t.truncated" class="truncate-banner">
              <span>文件过大，仅显示前 {{ truncatedTextOf(t) }}</span>
              <el-button v-if="t.nextOffset != null" size="small" type="primary" text
                         :loading="t.loadingMore" @click="loadMore(t)">加载更多</el-button>
              <el-button size="small" text tag="a" :href="rawUrlOf(t) + '&download=1'" rel="noreferrer">下载查看</el-button>
            </div>
            <pre class="code-pre" v-html="jsonHtmlOf(t)"></pre>
          </div>

          <!-- 代码/文本：highlight.js 语法高亮；真空文件给明确空态而非错误文案 -->
          <div v-else-if="fileMeta(t.name).kind === 'code' || fileMeta(t.name).kind === 'text'" class="preview-body">
            <div v-if="t.truncated" class="truncate-banner">
              <span>文件过大，仅显示前 {{ truncatedTextOf(t) }}</span>
              <el-button v-if="t.nextOffset != null" size="small" type="primary" text
                         :loading="t.loadingMore" @click="loadMore(t)">加载更多</el-button>
              <el-button size="small" text tag="a" :href="rawUrlOf(t) + '&download=1'" rel="noreferrer">下载查看</el-button>
            </div>
            <div v-if="t.content === ''" class="empty-file">（空文件）</div>
            <pre v-else class="code-pre" v-html="codeHtmlOf(t)"></pre>
          </div>

          <!-- 二进制/未知：不支持预览 -->
          <div v-else class="flex flex-col items-center justify-center flex-1 text-sm text-ink-3 gap-3">
            <el-icon class="text-3xl"><WarningFilled /></el-icon>
            <span>该文件类型暂不支持预览</span>
            <el-button type="primary" size="small" plain tag="a" :href="rawUrlOf(t) + '&download=1'" rel="noreferrer">
              <el-icon class="mr-1"><Download /></el-icon> 下载文件
            </el-button>
          </div>
        </div>

        <!-- 底部路径条 -->
        <div v-if="activeTab" class="mt-4 pt-2 border-t border-line text-xs text-ink-2 flex justify-between shrink-0">
          <span class="truncate">文件路径：{{ activeTab.path || '-' }}</span>
          <span class="shrink-0 ml-4">大小：{{ formatSize(activeTab.size) }}</span>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, nextTick } from 'vue'
import hljs, { HLJS_SUPPORTED } from '@/utils/hljs'
import { IMAGE_EXTS } from '@/utils/fileKind'
import {
  PictureFilled, Document, Notebook, Coin, Tickets, QuestionFilled,
  FolderOpened, Folder, Loading, Download, Position, WarningFilled,
  EditPen, Select, Search, Close, Plus, FullScreen,
} from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { APIError } from '@/api/client'
import {
  listFiles, getFileContent, saveFileContent, getFSTree, revealFile,
  type FileItem, type FSTreeNode,
} from '@/api/files'
import { useFileOpener } from '@/composables/fileOpener'
import MarkdownRenderer from '@/components/MarkdownRenderer.vue'
import OpenInEditorMenu from '@/components/OpenInEditorMenu.vue'
import WorkspaceTreeNode from './WorkspaceTreeNode.vue'

const props = defineProps<{
  sessionId: string
}>()

/** 文件打开器（session/index.vue provide）：消费消息卡片/本地链接发来的定位请求。 */
const fileOpener = useFileOpener()

// 定位请求可能在 FilePreview 未挂载（rightTab 非 files）时发出，immediate 让挂载即补处理；
// 处理完置回 null，避免会话切换后组件重挂载又把旧请求开一遍。seq 保证同路径连点也触发。
watch(() => fileOpener?.locateRequest.value ?? null, (req) => {
  if (!req || !fileOpener) return
  fileOpener.locateRequest.value = null
  const abs = fileOpener.resolveAbsolute(req.path)
  if (!abs) {
    ElMessage.warning('该会话没有工作区，无法在文件树中定位')
    return
  }
  const name = abs.split(/[\\/]/).pop() || abs
  void openFile({ path: abs, name })
}, { immediate: true })

type FileKind = 'image' | 'markdown' | 'json' | 'code' | 'text' | 'other'

interface FileMeta {
  kind: FileKind
  label: string
  icon: unknown
  iconCls: string
  /** highlight.js 语言名；undefined = 不高亮（纯文本） */
  hlLang?: string
}

/** 预览 tab：每个打开的文件独立保留内容/滚动/编辑态（TODO #26 阶段 E）。 */
interface PreviewTab {
  path: string
  name: string
  size: number
  content: string
  truncated: boolean
  truncatedSize: number
  /** 下一字节偏移（分段续拉，TODO #26 D）；null = 已取尽 */
  nextOffset: number | null
  /** 「加载更多」请求进行中 */
  loadingMore: boolean
  /** 磁盘 mtime（Unix 毫秒），保存时作 base_mtime 冲突检测 */
  baseMtime: number | null
  editing: boolean
  editContent: string
  loading: boolean
  loaded: boolean
  /** 请求报错文案；真空文件 loadError 为空串（显示「（空文件）」而非错误） */
  loadError: string
}

const MARKDOWN_EXTS = new Set(['md', 'markdown'])
const CODE_EXTS: Record<string, string> = {
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  ts: 'typescript', tsx: 'typescript', mts: 'typescript',
  py: 'python', go: 'go', java: 'java',
  c: 'cpp', cc: 'cpp', cpp: 'cpp', h: 'cpp', hpp: 'cpp', cs: 'cpp', rs: 'rust',
  html: 'xml', htm: 'xml', vue: 'xml', xml: 'xml', // hljs 无官方 vue 语法，SFC 用 xml 兜底
  css: 'css', scss: 'scss', less: 'scss',
  sh: 'bash', bash: 'bash', zsh: 'bash',
  yml: 'yaml', yaml: 'yaml',
  sql: 'sql', ini: 'ini', toml: 'ini', // hljs 无官方 toml 语法，ini 结构最接近作兜底
  lua: 'lua', diff: 'diff', patch: 'diff',
}
const TEXT_EXTS = new Set(['txt', 'log', 'csv', 'env'])

// ── 数据源：树模式（工作区目录树）/ 降级模式（WriteFile 产物平铺） ──
const treeMode = ref(false)
const treeLoading = ref(false)
const fsRoot = ref('')
const treeTruncated = ref(false)
const tree = ref<FSTreeNode | null>(null)
const files = ref<FileItem[]>([])
const listLoading = ref(false)

// ── 树交互 ──
const expanded = ref<Record<string, boolean>>({})
const filterText = ref('')
const highlightPath = ref('')
const filterInputRef = ref<{ focus: () => void } | null>(null)

// ── Tab 多开 ──
const tabs = ref<PreviewTab[]>([])
const activeTabPath = ref('')
const saving = ref(false)

const activeTab = computed(() => tabs.value.find(t => t.path === activeTabPath.value) || null)
const activeDirty = computed(() => (activeTab.value ? dirtyOf(activeTab.value) : false))
const activeMeta = computed<FileMeta>(() => fileMeta(activeTab.value?.name || ''))

/** 文本类文件（markdown/json/code/text）显示编辑器下拉；图片/视频等保持原 VS Code 按钮。 */
const isTextKind = computed(() =>
  ['markdown', 'json', 'code', 'text'].includes(activeMeta.value.kind))

const filtering = computed(() => filterText.value.trim().length > 0)

const rootName = computed(() => {
  if (tree.value) return tree.value.name
  const r = fsRoot.value
  if (!r) return '工作区'
  const parts = r.split(/[\\/]/).filter(Boolean)
  return parts[parts.length - 1] || r
})

/** 筛选后的可见树：文件按名子串命中保留；目录名命中则整棵子树可见，否则只保留命中后代。 */
const visibleChildren = computed<FSTreeNode[]>(() => {
  const root = tree.value
  if (!root?.children) return []
  if (!filtering.value) return root.children
  const q = filterText.value.trim().toLowerCase()
  const match = (n: FSTreeNode): FSTreeNode | null => {
    if (n.type === 'file') {
      return n.name.toLowerCase().includes(q) ? n : null
    }
    if (n.name.toLowerCase().includes(q)) return n // 目录名命中：整棵子树可见
    const children = (n.children || []).map(match).filter((c): c is FSTreeNode => !!c)
    return children.length ? { ...n, children } : null
  }
  return root.children.map(match).filter((c): c is FSTreeNode => !!c)
})

/** 面包屑：根名 › 子目录 › 文件名；目录段带 path（点击在树中定位高亮），文件名段无 path。 */
const breadcrumb = computed<Array<{ name: string; path?: string }>>(() => {
  const t = activeTab.value
  if (!t) return []
  const segs: Array<{ name: string; path?: string }> = [{ name: rootName.value, path: fsRoot.value || undefined }]
  const root = fsRoot.value
  if (root && (t.path === root || t.path.startsWith(root))) {
    const rel = t.path.slice(root.length).replace(/^[\\/]/, '')
    const parts = rel.split(/[\\/]/).filter(Boolean)
    let cur = root
    for (let i = 0; i < parts.length; i++) {
      cur = cur.replace(/[\\/]$/, '') + '\\' + parts[i]
      if (i === parts.length - 1) {
        segs.push({ name: parts[i] }) // 文件名段：纯展示
      } else {
        segs.push({ name: parts[i], path: cur })
      }
    }
  } else {
    segs.push({ name: t.name })
  }
  return segs
})

/** 原始字节端点：图片内联渲染与下载共用（后端限 WriteFile 产物 + 20MB）。 */
function rawUrlOf(t: PreviewTab) {
  return `/api/files/raw?path=${encodeURIComponent(t.path)}`
}
const downloadUrl = computed(() => (activeTab.value ? `${rawUrlOf(activeTab.value)}&download=1` : '#'))
const vscodeUrl = computed(() => (activeTab.value ? `vscode://file/${activeTab.value.path}` : '#'))

// ── 「在文件夹中显示」（TODO #26 阶段 C）：调 /api/files/reveal 由系统文件管理器定位 ──
const revealing = ref(false)

async function revealActive() {
  const t = activeTab.value
  if (!t || revealing.value) return
  revealing.value = true
  try {
    await revealFile(t.path)
    ElMessage.success('已在文件管理器中定位')
  } catch (e) {
    const noDesktop = e instanceof APIError && (e.status === 501 || e.status === 500)
    ElMessage.error(noDesktop ? '当前环境无桌面，无法打开文件管理器' : `定位失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    revealing.value = false
  }
}

/** 同目录图片列表（FileViewer 画廊左右切换用）：树模式下取当前文件所在目录的
 *  全部图片文件；非图片/降级模式只给当前文件本身。 */
const viewerSiblings = computed<string[]>(() => {
  const t = activeTab.value
  if (!t) return []
  const only = [t.path]
  if (!treeMode.value || !tree.value) return only
  const parent = t.path.replace(/[\\/][^\\/]*$/, '')
  const norm = (s: string) => s.replace(/[\\/]+$/, '').toLowerCase()
  const findDir = (node: FSTreeNode): FSTreeNode[] | null => {
    if (node.type === 'dir' && norm(node.path) === norm(parent)) return node.children || []
    for (const c of node.children || []) {
      const r = findDir(c)
      if (r) return r
    }
    return null
  }
  const kids = findDir(tree.value)
  if (!kids) return only
  const imgs = kids
    .filter(n => n.type === 'file' && IMAGE_EXTS.has(n.name.includes('.') ? n.name.split('.').pop()!.toLowerCase() : ''))
    .map(n => n.path)
  return imgs.some(p => norm(p) === norm(t.path)) ? imgs : [...imgs, t.path]
})

/** 全屏（TODO #26 B）：打开全局 FileViewer 只读查看；图片带同目录 siblings 进画廊。 */
function openFullscreen() {
  const t = activeTab.value
  if (!t || !fileOpener) return
  fileOpener.openInViewer(t.path, { siblings: viewerSiblings.value, size: t.size || undefined })
}

/** 截断提示文案：按完整大小换算「前 X MB/KB」。 */
function truncatedTextOf(t: PreviewTab) {
  return formatSize(t.truncatedSize || t.content.length)
}

function fileMeta(name: string): FileMeta {
  const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : ''
  const lowerName = name.toLowerCase()
  // Dockerfile 无扩展名（或以 .dockerfile 结尾）：单独走 dockerfile 高亮
  if (lowerName === 'dockerfile' || ext === 'dockerfile') {
    return { kind: 'code', label: '代码 · dockerfile', icon: Document, iconCls: 'text-sky-400', hlLang: 'dockerfile' }
  }
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
  if (id) void loadWorkspace(id)
}, { immediate: true })

function resetWorkspace() {
  tabs.value = []
  activeTabPath.value = ''
  filterText.value = ''
  highlightPath.value = ''
  expanded.value = {}
  tree.value = null
  fsRoot.value = ''
  treeTruncated.value = false
  files.value = []
}

async function loadWorkspace(sessionID: string) {
  if (!(await confirmDiscardDirty())) return // 有脏编辑时留在当前文件
  resetWorkspace()
  treeLoading.value = true
  try {
    const res = await getFSTree(sessionID)
    treeMode.value = true
    fsRoot.value = res.root
    tree.value = res.tree
    treeTruncated.value = !!res.truncated
    if (res.tree) expanded.value = { [res.tree.path]: true }
  } catch {
    // 无工作区/会话无效/网络失败 → 降级为 WriteFile 产物列表（原形态）
    treeMode.value = false
    treeLoading.value = false
    listLoading.value = true
    try {
      const res = await listFiles(sessionID)
      files.value = res.files || []
    } catch {
      files.value = []
    }
    listLoading.value = false
    return
  }
  treeLoading.value = false
}

function dirtyOf(t: PreviewTab) {
  return t.editing && t.editContent !== t.content
}

/** 有未保存修改时弹确认；返回 true = 可以继续（丢弃修改）。 */
async function confirmDiscardDirty(): Promise<boolean> {
  if (!activeDirty.value) return true
  try {
    await ElMessageBox.confirm('当前文件有未保存的修改，切换后将丢失。', '未保存修改', {
      confirmButtonText: '丢弃并继续',
      cancelButtonText: '取消',
      type: 'warning',
    })
    return true
  } catch {
    return false
  }
}

/** 打开文件（树节点或降级列表行共用）：已开则切 tab，未开则新建 tab 并拉内容。 */
async function openFile(node: { path: string; name: string; size?: number }) {
  if (!node.path) return
  const exist = tabs.value.find(t => t.path === node.path)
  if (exist) {
    if (exist.path !== activeTabPath.value) {
      if (!(await confirmDiscardDirty())) return
      activeTabPath.value = exist.path
    }
    return
  }
  if (!(await confirmDiscardDirty())) return
  const tab: PreviewTab = {
    path: node.path,
    name: node.name,
    size: node.size ?? 0,
    content: '',
    truncated: false,
    truncatedSize: 0,
    nextOffset: null,
    loadingMore: false,
    baseMtime: null,
    editing: false,
    editContent: '',
    loading: true,
    loaded: false,
    loadError: '',
  }
  tabs.value.push(tab)
  activeTabPath.value = tab.path
  revealInTree(node.path)
  if (fileMeta(node.name).kind === 'image') {
    // 图片走 raw 端点，不取文本
    tab.loading = false
    tab.loaded = true
    return
  }
  try {
    const res = await getFileContent(node.path)
    // 拉取期间用户可能已切走/关闭该 tab：仅当 tab 仍在列表时才回填
    const cur = tabs.value.find(t => t.path === tab.path)
    if (!cur) return
    cur.content = res.content
    cur.truncated = !!res.truncated
    cur.nextOffset = res.next_offset ?? null
    cur.truncatedSize = res.total_size ?? res.size ?? res.content.length
    cur.baseMtime = res.mtime ?? null
    cur.size = res.size ?? cur.size
  } catch (e) {
    const cur = tabs.value.find(t => t.path === tab.path)
    if (cur) cur.loadError = `无法读取文件内容${e instanceof Error ? `：${e.message}` : ''}`
  } finally {
    const cur = tabs.value.find(t => t.path === tab.path)
    if (cur) {
      cur.loading = false
      cur.loaded = true
    }
  }
}

function activateTab(t: PreviewTab) {
  activeTabPath.value = t.path
}

/**
 * 「加载更多」（TODO #26 阶段 D）：按 nextOffset 续拉分段内容追加到末尾，
 * 直到后端 next_offset 为空。截断文件编辑入口天然关闭，全部加载完后
 * truncated=false 才允许进入编辑态（内容已与磁盘一致）。
 */
async function loadMore(t: PreviewTab) {
  if (t.nextOffset == null || t.loadingMore) return
  t.loadingMore = true
  try {
    const res = await getFileContent(t.path, { offset: t.nextOffset })
    const cur = tabs.value.find(x => x.path === t.path)
    if (!cur) return // 拉取期间 tab 已关闭
    cur.content += res.content
    cur.truncated = !!res.truncated
    cur.nextOffset = res.next_offset ?? null
    cur.truncatedSize = res.total_size ?? res.size ?? cur.content.length
    cur.baseMtime = res.mtime ?? cur.baseMtime
    cur.size = res.size ?? cur.size
  } catch (e) {
    ElMessage.error(`加载失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    const cur = tabs.value.find(x => x.path === t.path)
    if (cur) cur.loadingMore = false
  }
}

/** 关闭 tab：dirty 先确认；关掉的恰是活跃 tab 时切到邻居。 */
async function closeTab(t: PreviewTab) {
  if (dirtyOf(t)) {
    try {
      await ElMessageBox.confirm(`「${t.name}」有未保存的修改，关闭后将丢失。`, '未保存修改', {
        confirmButtonText: '丢弃并关闭',
        cancelButtonText: '取消',
        type: 'warning',
      })
    } catch {
      return
    }
  }
  const idx = tabs.value.findIndex(x => x.path === t.path)
  tabs.value.splice(idx, 1)
  if (activeTabPath.value === t.path) {
    const neighbor = tabs.value[idx] || tabs.value[idx - 1]
    activeTabPath.value = neighbor ? neighbor.path : ''
  }
}

/** 「+」回到树：收起筛选焦点，聚焦筛选输入框便于重新定位文件。 */
function backToTree() {
  filterInputRef.value?.focus()
}

// ── 树交互 ──

// 树 → 消息双向定位（TODO #26 C 遗留子项）：在对话流里找引用该文件的消息元素
//（Markdown 本地链接 a[data-bma-file] 与非媒体 ArtifactCard 根[data-bma-file-card]），
// 命中则滚动定位 + 高亮闪烁；resolveAbsolute 归一相对/绝对路径后大小写不敏感比对。
function locateInChat(node: FSTreeNode) {
  const target = (fileOpener?.resolveAbsolute(node.path) ?? node.path).toLowerCase()
  const anchors = document.querySelectorAll<HTMLElement>('a[data-bma-file], [data-bma-file-card]')
  for (const el of anchors) {
    const raw = el.getAttribute('data-bma-file') || el.getAttribute('data-bma-file-card') || ''
    const abs = (fileOpener?.resolveAbsolute(raw) ?? raw).toLowerCase()
    if (abs && abs === target) {
      el.scrollIntoView({ behavior: 'smooth', block: 'center' })
      el.classList.add('bma-locate-flash')
      setTimeout(() => el.classList.remove('bma-locate-flash'), 1800)
      return
    }
  }
  ElMessage.info('对话中没有引用该文件的消息')
}

function toggleDir(node: FSTreeNode) {
  expanded.value[node.path] = !expanded.value[node.path]
}

function onFilterInput() {
  // 筛选时自动展开命中链（渲染层 expand-all 处理），无需额外状态
  highlightPath.value = ''
}

/** 取 path 的祖先目录链（从直接父级直到工作区根，含端点），用于打开/定位时自动展开。 */
function ancestorDirs(path: string): string[] {
  const root = fsRoot.value
  const out: string[] = []
  let cur = path.replace(/[\\/][^\\/]*$/, '')
  const norm = (s: string) => s.replace(/[\\/]+$/, '').toLowerCase()
  const rootN = norm(root)
  while (cur && norm(cur) !== rootN && cur.length >= root.length) {
    out.unshift(cur)
    const next = cur.replace(/[\\/][^\\/]*$/, '')
    if (next === cur) break
    cur = next
  }
  return out
}

/** 面包屑/打开文件时：展开祖先目录并在树中闪显定位。 */
function revealInTree(path: string) {
  if (!treeMode.value || !path) return
  for (const dir of ancestorDirs(path)) {
    expanded.value[dir] = true
  }
  if (path !== fsRoot.value) expanded.value[fsRoot.value] = true
  highlightPath.value = path
  void nextTick(() => {
    document
      .querySelector(`[data-tree-path="${CSS.escape(path)}"]`)
      ?.scrollIntoView({ block: 'nearest' })
  })
}

// ── 编辑 / 保存（TODO #26 阶段 G，按 tab 隔离） ──

function enterEdit() {
  const t = activeTab.value
  if (!t) return
  t.editContent = t.content
  t.editing = true
}

function cancelEdit() {
  const t = activeTab.value
  if (!t) return
  t.editing = false
  t.editContent = ''
}

/** textarea 快捷键：Ctrl/Cmd+S 拦截默认行为直接保存。 */
function onEditKeydown(e: KeyboardEvent, t: PreviewTab) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault()
    void saveEditFor(t, false)
  }
}

function saveEdit(force: boolean) {
  const t = activeTab.value
  if (t) void saveEditFor(t, force)
}

/**
 * 保存：默认带 baseMtime 冲突检测；409 时弹「覆盖/放弃」，
 * 覆盖 = 不带 base_mtime 重发。成功后刷新 mtime/大小并清 dirty，保持编辑态。
 */
async function saveEditFor(t: PreviewTab, force: boolean) {
  if (saving.value) return
  if (!dirtyOf(t) && !force) return // 无改动不打扰后端
  saving.value = true
  try {
    const res = await saveFileContent(t.path, t.editContent, force ? undefined : (t.baseMtime ?? undefined))
    const cur = tabs.value.find(x => x.path === t.path)
    if (!cur) return
    cur.baseMtime = res.mtime
    cur.content = cur.editContent // 同步基准 → dirty 清除
    cur.size = res.size // 头部大小即时刷新
    ElMessage.success('已保存')
  } catch (e) {
    if (e instanceof APIError && e.status === 409) {
      try {
        await ElMessageBox.confirm('文件已被外部修改，继续保存将覆盖外部改动。', '保存冲突', {
          confirmButtonText: '覆盖',
          cancelButtonText: '放弃',
          type: 'warning',
        })
        await saveEditFor(t, true) // 覆盖：不带 base_mtime 重发
      } catch {
        // 放弃：留在编辑态，用户可自行再改或取消
      }
    } else {
      ElMessage.error(`保存失败：${e instanceof Error ? e.message : String(e)}`)
    }
  } finally {
    saving.value = false
  }
}

/** JSON pretty-print（2 空格缩进），解析失败回退纯文本展示。 */
function jsonHtmlOf(t: PreviewTab) {
  try {
    return hljs.highlight(JSON.stringify(JSON.parse(t.content), null, 2),
      { language: 'json' }).value
  } catch {
    return escapeHtml(t.content)
  }
}

/** 代码高亮：注册了语言的按语言高亮，其余纯文本转义展示。 */
function codeHtmlOf(t: PreviewTab) {
  const lang = fileMeta(t.name).hlLang
  if (lang && hljs.getLanguage(lang)) {
    try {
      return hljs.highlight(t.content, { language: lang }).value
    } catch {
      // 高亮失败（如非法字符）回退纯文本
    }
  }
  return escapeHtml(t.content)
}

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
/* 文件行（降级模式沿用）：hover 高亮 + 选中态（Kimi Work 列表风格） */
.file-list-card :deep(.el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
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

/* 树筛选框：贴面板 token */
.tree-filter :deep(.el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
}

/* 面包屑 */
.breadcrumb-bar {
  min-width: 0;
}
.crumb-sep {
  color: var(--bma-text-3, #8a8f99);
  flex-shrink: 0;
}
.crumb-link {
  color: var(--bma-text-2, #6b7280);
  background: none;
  border: none;
  padding: 0 2px;
  cursor: pointer;
  font-size: 13px;
  border-radius: 4px;
}
.crumb-link:hover {
  color: var(--bma-primary);
  background: var(--bma-primary-soft);
}

/* Tab 栏 */
.tab-strip {
  display: flex;
  align-items: center;
  gap: 2px;
  overflow-x: auto;
  padding: 4px 2px;
  border-bottom: 1px solid var(--bma-border);
  margin-bottom: 8px;
}
.tab-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  max-width: 180px;
  padding: 4px 8px;
  border-radius: 6px;
  font-size: 12.5px;
  color: var(--bma-text-2, #6b7280);
  cursor: pointer;
  border: 1px solid transparent;
  white-space: nowrap;
  transition: background-color 0.12s ease;
}
.tab-item:hover {
  background: var(--bma-primary-soft);
}
.tab-active {
  color: var(--bma-primary);
  background: var(--bma-primary-soft);
  border-color: var(--bma-primary);
}
.tab-name {
  overflow: hidden;
  text-overflow: ellipsis;
}
.tab-close {
  border-radius: 4px;
  color: var(--bma-text-3, #8a8f99);
}
.tab-close:hover {
  color: var(--bma-text, #1f2329);
  background: rgba(0, 0, 0, 0.08);
}
.tab-plus {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border-radius: 6px;
  color: var(--bma-text-3, #8a8f99);
  border: 1px dashed var(--bma-border);
  background: none;
  cursor: pointer;
  flex-shrink: 0;
}
.tab-plus:hover {
  color: var(--bma-primary);
  border-color: var(--bma-primary);
}

/* 预览主体 */
.tab-pane {
  min-height: 0;
}
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

/* 空文件提示：区别于加载失败 */
.empty-file {
  display: flex;
  align-items: center;
  justify-content: center;
  height: 100%;
  color: var(--bma-text-3, #8a8f99);
  font-size: 12.5px;
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
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  padding: 6px 10px;
  border-radius: 6px;
  font-size: 12px;
  color: #b45309;
  background: rgba(245, 158, 11, 0.12);
  border: 1px solid rgba(245, 158, 11, 0.35);
}

/* 编辑态：textarea 铺满内容区，等宽字体 */
.edit-body {
  display: flex;
  padding: 8px;
}
.edit-textarea {
  flex: 1;
  min-height: 0;
  resize: none;
  border: none;
  outline: none;
  background: transparent;
  color: var(--bma-text);
  font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
  white-space: pre;
  overflow: auto;
}
.edit-textarea:disabled {
  opacity: 0.6;
}

/* dirty 标记：文件名旁橙色圆点 */
.dirty-dot {
  color: #d97706;
  font-size: 12px;
  line-height: 1;
}

/* 「打开 ▾」菜单项：编辑器名 + 截断的 exe 路径 */
.open-menu :deep(.el-dropdown-menu__item) {
  max-width: 360px;
}
.open-item {
  display: flex;
  align-items: baseline;
  gap: 10px;
  min-width: 0;
}
.open-name {
  flex-shrink: 0;
  font-size: 13px;
}
.open-path {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 11px;
  color: var(--bma-text-3, #8a8f99);
}

/* 滚动条 */
pre::-webkit-scrollbar, .preview-body::-webkit-scrollbar, .tab-strip::-webkit-scrollbar {
  height: 8px;
  width: 8px;
}
pre::-webkit-scrollbar-track, .preview-body::-webkit-scrollbar-track, .tab-strip::-webkit-scrollbar-track {
  background: transparent;
}
pre::-webkit-scrollbar-thumb, .preview-body::-webkit-scrollbar-thumb, .tab-strip::-webkit-scrollbar-thumb {
  background: var(--bma-text-3);
  border-radius: 4px;
}
pre::-webkit-scrollbar-thumb:hover, .preview-body::-webkit-scrollbar-thumb:hover, .tab-strip::-webkit-scrollbar-thumb:hover {
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
