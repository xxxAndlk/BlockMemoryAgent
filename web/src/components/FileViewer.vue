<script setup lang="ts">
// FileViewer 全局全屏预览层（TODO #26 阶段 B）：固定单例遮罩，session/index.vue 挂载
// 并经 fileOpener.openInViewer 打开（ArtifactCard 预览按钮 / Markdown data-bma-file 链接 /
// FilePreview「全屏」按钮统一入口）。分类型视图派发见 components/fileviewer/*。
//
// 内存释放：关闭时清空文本内容、子视图各自卸载（PdfView 销毁 pdfjs 文档、
// ImageView 断开事件监听、SheetView 清空表格行）；图片/视频/音频直接走 raw URL，
// 浏览器缓存自然回收，无 object URL 需要 revoke。
import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue'
import { ElMessage } from 'element-plus'
import {
  Close, Download, Position, Folder, FullScreen, Document, PictureFilled,
  VideoPlay, Headset, Monitor, Notebook, Coin, Grid, Tickets, QuestionFilled,
  Loading, WarningFilled, ArrowLeft, ArrowRight,
} from '@element-plus/icons-vue'
import { APIError } from '@/api/client'
import { getFileContent, revealFile } from '@/api/files'
import { useFileOpener, isAbsolutePath } from '@/composables/fileOpener'
import { classifyFile, isTextualKind, type ViewerMeta } from '@/utils/fileKind'
import MarkdownRenderer from '@/components/MarkdownRenderer.vue'
import OpenInEditorMenu from '@/components/OpenInEditorMenu.vue'
import FileViewerImage from './fileviewer/FileViewerImage.vue'
import FileViewerPdf from './fileviewer/FileViewerPdf.vue'
import FileViewerSheet from './fileviewer/FileViewerSheet.vue'
import FileViewerNotebook from './fileviewer/FileViewerNotebook.vue'
import FileViewerCode from './fileviewer/FileViewerCode.vue'

/** 打开中的文件状态（全屏层单例，同时只展示一个文件）。 */
interface ViewerFile {
  path: string
  name: string
  meta: ViewerMeta
  /** 已知大小（字节）；未知为 null */
  size: number | null
  /** 文本链路加载状态 */
  loading: boolean
  error: string
  content: string
  truncated: boolean
  nextOffset: number | null
  loadingMore: boolean
}

const fileOpener = useFileOpener()

const visible = ref(false)
const file = ref<ViewerFile | null>(null)
/** 图片画廊：同目录图片绝对路径列表（含当前文件） */
const gallery = ref<string[]>([])
const galleryIndex = ref(0)
const rootRef = ref<HTMLElement | null>(null)
const isFullscreen = ref(false)

const KIND_ICONS: Record<string, unknown> = {
  image: PictureFilled, pdf: Tickets, video: VideoPlay, audio: Headset,
  markdown: Notebook, json: Coin, csv: Grid, xlsx: Grid, ipynb: Notebook,
  html: Monitor, code: Document, text: Tickets, binary: QuestionFilled,
}

const meta = computed(() => file.value?.meta || null)
const kind = computed(() => meta.value?.kind || 'binary')
const kindIcon = computed(() => KIND_ICONS[kind.value] || QuestionFilled)

const rawUrl = computed(() =>
  file.value ? `/api/files/raw?path=${encodeURIComponent(file.value.path)}` : '')
const downloadUrl = computed(() => `${rawUrl.value}&download=1`)

/** 是否文本加载链路（content 端点分段拉取）。 */
const textual = computed(() => !!meta.value && isTextualKind(meta.value.kind))

/** 面包屑：工作区根名 › 子目录 › 文件名（只读展示，文件名带全路径 title）。 */
const breadcrumb = computed<Array<{ name: string }>>(() => {
  const f = file.value
  if (!f) return []
  const workDir = (fileOpener?.workDir.value || '').replace(/[\\/]+$/, '')
  const segs: Array<{ name: string }> = []
  if (workDir && (f.path === workDir || f.path.startsWith(workDir + '\\') || f.path.startsWith(workDir + '/'))) {
    const rootParts = workDir.split(/[\\/]/).filter(Boolean)
    segs.push({ name: rootParts[rootParts.length - 1] || workDir })
    const rel = f.path.slice(workDir.length).replace(/^[\\/]/, '')
    const parts = rel.split(/[\\/]/).filter(Boolean)
    for (let i = 0; i < parts.length; i++) {
      segs.push({ name: parts[i] }) // 最后一段是文件名
    }
  } else {
    const parts = f.path.split(/[\\/]/).filter(Boolean)
    // 非工作区内路径：展示后三段即可，避免顶出工具条
    for (const p of parts.slice(-3)) segs.push({ name: p })
  }
  return segs
})

function formatSize(bytes: number | null): string {
  if (bytes == null) return ''
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(1) + ' MB'
}

// ── 打开 / 关闭 ──

function open(path: string, opts?: { siblings?: string[]; size?: number }) {
  const abs = isAbsolutePath(path)
    ? path
    : (fileOpener?.resolveAbsolute(path) ?? null)
  if (!abs) {
    ElMessage.warning('该会话没有工作区，无法预览文件')
    return
  }
  const name = abs.split(/[\\/]/).pop() || abs
  const meta = classifyFile(name)
  file.value = {
    path: abs,
    name,
    meta,
    size: opts?.size ?? null,
    loading: false,
    error: '',
    content: '',
    truncated: false,
    nextOffset: null,
    loadingMore: false,
  }
  // 图片画廊：siblings 传了用传参（FilePreview 同目录图），否则单图
  if (meta.kind === 'image') {
    const list = (opts?.siblings || [abs])
      .map(p => (isAbsolutePath(p) ? p : (fileOpener?.resolveAbsolute(p) ?? '')))
      .filter(Boolean)
    gallery.value = list.includes(abs) ? list : [...list, abs]
    galleryIndex.value = Math.max(0, gallery.value.indexOf(abs))
  } else {
    gallery.value = []
    galleryIndex.value = 0
  }
  visible.value = true
  if (isTextualKind(meta.kind)) void loadContent(file.value)
}

function close() {
  visible.value = false
  // 释放：清空文本内容（pdfjs 销毁 / 事件解绑在子视图 unmount 完成）
  file.value = null
  gallery.value = []
  if (isFullscreen.value && document.fullscreenElement) {
    void document.exitFullscreen()
  }
}

// ── 文本链路加载（分段，超大文件截断提示 + 加载更多） ──

async function loadContent(f: ViewerFile) {
  f.loading = true
  f.error = ''
  try {
    const res = await getFileContent(f.path)
    const cur = file.value
    if (!cur || cur.path !== f.path) return // 关闭/切换后丢弃
    cur.content = res.content
    cur.truncated = !!res.truncated
    cur.nextOffset = res.next_offset ?? null
    cur.size = res.size ?? res.total_size ?? cur.size
  } catch (e) {
    const cur = file.value
    if (cur && cur.path === f.path) {
      cur.error = `无法读取文件内容${e instanceof Error ? `：${e.message}` : ''}`
    }
  } finally {
    const cur = file.value
    if (cur && cur.path === f.path) cur.loading = false
  }
}

async function loadMore() {
  const f = file.value
  if (!f || f.nextOffset == null || f.loadingMore) return
  f.loadingMore = true
  try {
    const res = await getFileContent(f.path, { offset: f.nextOffset })
    const cur = file.value
    if (!cur || cur.path !== f.path) return
    cur.content += res.content
    cur.truncated = !!res.truncated
    cur.nextOffset = res.next_offset ?? null
    cur.size = res.size ?? cur.size
  } catch (e) {
    ElMessage.error(`加载失败：${e instanceof Error ? e.message : String(e)}`)
  } finally {
    const cur = file.value
    if (cur && cur.path === f.path) cur.loadingMore = false
  }
}

// ── 画廊左右切换（图片路径变化 → 重建 file 状态） ──

watch(galleryIndex, (i) => {
  const p = gallery.value[i]
  const f = file.value
  if (!p || !f || f.path === p) return
  const name = p.split(/[\\/]/).pop() || p
  const meta = classifyFile(name)
  file.value = {
    path: p, name, meta, size: null,
    loading: false, error: '', content: '',
    truncated: false, nextOffset: null, loadingMore: false,
  }
})

// ── 工具条操作 ──

function openInNewWindow() {
  if (rawUrl.value) window.open(rawUrl.value, '_blank', 'noopener')
}

async function revealInFolder() {
  const f = file.value
  if (!f) return
  try {
    await revealFile(f.path)
    ElMessage.success('已在文件管理器中定位')
  } catch (e) {
    const noDesktop = e instanceof APIError && (e.status === 501 || e.status === 500)
    ElMessage.error(noDesktop ? '当前环境无桌面，无法打开文件管理器' : `定位失败：${e instanceof Error ? e.message : String(e)}`)
  }
}

function toggleFullscreen() {
  if (document.fullscreenElement) {
    void document.exitFullscreen()
  } else {
    void rootRef.value?.requestFullscreen()
  }
}

function onFullscreenChange() {
  isFullscreen.value = !!document.fullscreenElement
}

// ── 键盘导航 ──

function isTypingTarget(t: EventTarget | null): boolean {
  return t instanceof HTMLInputElement || t instanceof HTMLTextAreaElement
    || (t instanceof HTMLElement && t.isContentEditable)
}

function onKeydown(e: KeyboardEvent) {
  if (!visible.value) return
  if (e.key === 'Escape') {
    e.preventDefault()
    close()
    return
  }
  if (isTypingTarget(e.target)) return
  if (e.key === 'ArrowLeft' && kind.value === 'image' && galleryIndex.value > 0) {
    e.preventDefault()
    galleryIndex.value--
  } else if (e.key === 'ArrowRight' && kind.value === 'image' && galleryIndex.value < gallery.value.length - 1) {
    e.preventDefault()
    galleryIndex.value++
  } else if (e.key.toLowerCase() === 'f' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault()
    toggleFullscreen()
  }
}

onMounted(() => {
  window.addEventListener('keydown', onKeydown)
  document.addEventListener('fullscreenchange', onFullscreenChange)
})
onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKeydown)
  document.removeEventListener('fullscreenchange', onFullscreenChange)
})

defineExpose({ open, close })
</script>

<template>
  <Teleport to="body">
    <Transition name="fv-fade">
      <div v-if="visible" ref="rootRef" class="bma-viewer" role="dialog" aria-modal="true">
        <!-- 顶部工具条 -->
        <div class="fv-toolbar">
          <div class="fv-title min-w-0">
            <el-icon :size="16" class="fv-kind-icon shrink-0">
              <component :is="kindIcon" />
            </el-icon>
            <span class="fv-name truncate" :title="file?.path">{{ file?.name }}</span>
            <el-tag v-if="meta" size="small" effect="plain" class="fv-tag shrink-0">
              {{ meta.label }}
            </el-tag>
            <span v-if="file?.size != null" class="fv-size shrink-0">{{ formatSize(file.size) }}</span>
            <!-- 面包屑路径 -->
            <span class="fv-crumbs min-w-0 truncate">
              <template v-for="(seg, i) in breadcrumb" :key="i">
                <span v-if="i > 0" class="fv-crumb-sep">›</span>
                <span class="fv-crumb" :title="file?.path">{{ seg.name }}</span>
              </template>
            </span>
          </div>
          <div class="fv-actions shrink-0">
            <!-- 文本类：编辑器打开下拉 -->
            <OpenInEditorMenu v-if="textual && file" :path="file.path" class="fv-action" />
            <button type="button" class="fv-btn" title="在新窗口打开（raw 流）" @click="openInNewWindow">
              <el-icon :size="13"><Position /></el-icon> 新窗口
            </button>
            <a :href="downloadUrl" rel="noreferrer" class="fv-btn fv-btn-link" title="下载文件">
              <el-icon :size="13"><Download /></el-icon> 下载
            </a>
            <button type="button" class="fv-btn" title="在系统文件管理器中定位" @click="revealInFolder">
              <el-icon :size="13"><Folder /></el-icon> 定位
            </button>
            <button type="button" class="fv-btn" :title="isFullscreen ? '退出全屏（F）' : '全屏（F）'"
                    @click="toggleFullscreen">
              <el-icon :size="13"><FullScreen /></el-icon>
            </button>
            <button type="button" class="fv-btn fv-close" title="关闭（ESC）" @click="close">
              <el-icon :size="15"><Close /></el-icon>
            </button>
          </div>
        </div>

        <!-- 内容区：按类型派发 -->
        <div class="fv-body">
          <template v-if="file">
            <!-- 图片画廊 -->
            <FileViewerImage v-if="kind === 'image'" v-model:index="galleryIndex" :images="gallery" />

            <!-- PDF：pdfjs 分页懒渲染 -->
            <FileViewerPdf v-else-if="kind === 'pdf'" :url="rawUrl" />

            <!-- 视频 / 音频：raw Range 流，可拖进度 -->
            <div v-else-if="kind === 'video'" class="fv-media">
              <video :src="rawUrl" controls preload="metadata" class="fv-video"></video>
            </div>
            <div v-else-if="kind === 'audio'" class="fv-media">
              <audio :src="rawUrl" controls preload="metadata" class="fv-audio"></audio>
            </div>

            <!-- Markdown：MarkdownRenderer 渲染 -->
            <div v-else-if="kind === 'markdown'" class="fv-text-body">
              <div v-if="file.loading" class="fv-state">
                <el-icon class="is-loading"><Loading /></el-icon> 加载中…
              </div>
              <div v-else-if="file.error" class="fv-state error">
                <el-icon><WarningFilled /></el-icon> {{ file.error }}
              </div>
              <template v-else>
                <div v-if="file.truncated" class="fv-truncated">
                  <span>文件过大，仅显示前 {{ formatSize(file.content.length) }}</span>
                  <button v-if="file.nextOffset != null" type="button" class="fv-link-btn"
                          :disabled="file.loadingMore" @click="loadMore">
                    {{ file.loadingMore ? '加载中…' : '加载更多' }}
                  </button>
                  <a :href="downloadUrl" rel="noreferrer" class="fv-link-btn">下载查看</a>
                </div>
                <MarkdownRenderer :content="file.content" class="fv-md" />
              </template>
            </div>

            <!-- 代码 / 文本 / JSON -->
            <FileViewerCode v-else-if="['code', 'text', 'json'].includes(kind)" :name="file.name" :content="file.content" />

            <!-- CSV / TSV / xlsx -->
            <FileViewerSheet v-else-if="['csv', 'xlsx'].includes(kind)" :name="file.name"
                             :text="kind === 'csv' ? file.content : undefined" :url="rawUrl" />

            <!-- ipynb -->
            <FileViewerNotebook v-else-if="kind === 'ipynb'" :content="file.content" />

            <!-- HTML：沙箱渲染（脚本可跑、无同源权限），语义同 ArtifactCard -->
            <div v-else-if="kind === 'html'" class="fv-html">
              <iframe :src="rawUrl" sandbox="allow-scripts" referrerpolicy="no-referrer"
                      class="fv-html-frame" :title="file.name"></iframe>
            </div>

            <!-- 未知/二进制：信息卡 + 下载 -->
            <div v-else class="fv-binary">
              <el-icon :size="40" class="fv-binary-icon"><QuestionFilled /></el-icon>
              <div class="fv-binary-name">{{ file.name }}</div>
              <div class="fv-binary-meta">
                <span>{{ meta?.label }}</span>
                <span v-if="file.size != null"> · {{ formatSize(file.size) }}</span>
              </div>
              <a :href="downloadUrl" rel="noreferrer" class="fv-download-btn">
                <el-icon :size="14"><Download /></el-icon> 下载文件
              </a>
            </div>

            <!-- 文本链路加载态兜底（kind 变化瞬间） -->
            <div v-if="textual && file.loading && !['markdown'].includes(kind)" class="fv-state overlay-state">
              <el-icon class="is-loading"><Loading /></el-icon> 加载中…
            </div>
          </template>
        </div>

        <!-- 左右切换提示（多图时显示角标） -->
        <div v-if="kind === 'image' && gallery.length > 1" class="fv-gallery-hint">
          <el-icon :size="12"><ArrowLeft /></el-icon>
          {{ galleryIndex + 1 }} / {{ gallery.length }}
          <el-icon :size="12"><ArrowRight /></el-icon>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
/* 全屏遮罩：固定深色（与项目暗色一致），局部覆写 bma/el token 使内容区恒定暗色，
   不受 html.dark 主题切换影响（Element 弹出层 teleport 到 body，故工具条内不用
   会 teleport 的组件，编辑器下拉为自绘菜单）。 */
.bma-viewer {
  position: fixed;
  inset: 0;
  z-index: 3000;
  display: flex;
  flex-direction: column;
  background: rgba(11, 13, 18, 0.96);
  --bma-text: #e5e7eb;
  --bma-text-2: #a8afba;
  --bma-text-3: #6b7280;
  --bma-page: #16181d;
  --bma-card: #1a1d24;
  --bma-border: #2a2d35;
  --bma-primary: #6b77ff;
  --bma-primary-soft: #232947;
  --el-bg-color: #1a1d24;
  --el-bg-color-overlay: #1f232b;
  --el-text-color-primary: #e5e7eb;
  --el-text-color-regular: #a8afba;
  --el-text-color-secondary: #6b7280;
  --el-border-color: #2a2d35;
  --el-border-color-light: #2a2d35;
  --el-border-color-lighter: #23262e;
  --el-fill-color-blank: #1a1d24;
  --el-fill-color-light: #23262e;
}

/* 打开/关闭动画 */
.fv-fade-enter-active, .fv-fade-leave-active {
  transition: opacity 0.18s ease;
}
.fv-fade-enter-from, .fv-fade-leave-to {
  opacity: 0;
}
.fv-fade-enter-active .bma-viewer {
  animation: fv-zoom-in 0.18s ease;
}
@keyframes fv-zoom-in {
  from { transform: scale(0.985); }
  to { transform: scale(1); }
}

/* 顶部工具条 */
.fv-toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 8px 14px;
  background: #12151c;
  border-bottom: 1px solid #23262e;
  flex-shrink: 0;
}
.fv-title {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.fv-kind-icon {
  color: #8a94ff;
}
.fv-name {
  font-size: 13.5px;
  font-weight: 600;
  color: #e5e7eb;
}
.fv-tag {
  background: transparent;
  border-color: #2a2d35;
  color: #a8afba;
}
.fv-size {
  font-size: 12px;
  color: #6b7280;
}
.fv-crumbs {
  display: inline-flex;
  align-items: center;
  font-size: 12px;
  color: #6b7280;
}
.fv-crumb-sep {
  margin: 0 3px;
  color: #4b5261;
}
.fv-crumb {
  white-space: nowrap;
}
.fv-crumb:last-child {
  color: #a8afba;
}
.fv-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}
.fv-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  height: 28px;
  padding: 0 10px;
  border: 1px solid #2a2d35;
  border-radius: 6px;
  background: #1a1d24;
  color: #a8afba;
  font-size: 12.5px;
  cursor: pointer;
  transition: color 0.12s ease, border-color 0.12s ease;
}
.fv-btn:hover {
  color: #fff;
  border-color: #4f5dff;
}
.fv-btn-link {
  text-decoration: none;
}
a.fv-btn:hover {
  color: #fff;
}
.fv-close:hover {
  color: #f87171;
  border-color: #f87171;
}

/* 内容区 */
.fv-body {
  flex: 1;
  min-height: 0;
  position: relative;
  display: flex;
  flex-direction: column;
}
.fv-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 60px 0;
  color: #a8afba;
  font-size: 13px;
}
.fv-state.error {
  color: #f87171;
}
.overlay-state {
  position: absolute;
  inset: 0;
  background: rgba(11, 13, 18, 0.85);
  z-index: 5;
}
.fv-media {
  flex: 1;
  min-height: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #000;
  padding: 24px;
}
.fv-video {
  max-width: 100%;
  max-height: 100%;
}
.fv-audio {
  width: min(640px, 90%);
}
.fv-text-body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 20px 28px 32px;
}
.fv-md {
  max-width: 860px;
  margin: 0 auto;
  color: #d6dae2;
  font-size: 14px;
  line-height: 1.75;
}
.fv-truncated {
  display: flex;
  align-items: center;
  gap: 10px;
  max-width: 860px;
  margin: 0 auto 12px;
  padding: 6px 12px;
  border-radius: 6px;
  font-size: 12px;
  color: #fbbf24;
  background: rgba(245, 158, 11, 0.1);
  border: 1px solid rgba(245, 158, 11, 0.35);
}
.fv-link-btn {
  border: none;
  background: none;
  color: #8a94ff;
  font-size: 12px;
  cursor: pointer;
  text-decoration: none;
  padding: 0;
}
.fv-link-btn:hover {
  text-decoration: underline;
}
.fv-html {
  flex: 1;
  min-height: 0;
  display: flex;
  background: #fff;
}
.fv-html-frame {
  flex: 1;
  border: 0;
}
.fv-binary {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
}
.fv-binary-icon {
  color: #4b5261;
}
.fv-binary-name {
  font-size: 15px;
  font-weight: 600;
  color: #e5e7eb;
}
.fv-binary-meta {
  font-size: 12.5px;
  color: #6b7280;
}
.fv-download-btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-top: 8px;
  padding: 8px 18px;
  border-radius: 8px;
  background: #4f5dff;
  color: #fff;
  font-size: 13px;
  text-decoration: none;
}
.fv-download-btn:hover {
  background: #6b77ff;
}
.fv-gallery-hint {
  position: absolute;
  bottom: 14px;
  right: 16px;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border-radius: 999px;
  background: rgba(20, 23, 30, 0.75);
  border: 1px solid #2a2d35;
  color: #a8afba;
  font-size: 11.5px;
  pointer-events: none;
}
</style>
