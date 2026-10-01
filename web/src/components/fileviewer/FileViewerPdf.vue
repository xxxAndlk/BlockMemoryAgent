<script setup lang="ts">
// FileViewer PDF 视图（TODO #26 阶段 B）：pdfjs-dist 按需动态加载（点击 PDF 才下载
// 该 chunk），文档渐进加载（pdfjs 走 HTTP Range 分段拉取，后端 /api/files/raw 已支持
// Range/ETag），可视区附近分页懒渲染到 canvas，页码跳转，适应宽度 + 手动缩放。
import { ref, onMounted, onBeforeUnmount } from 'vue'
import type { PDFDocumentProxy, PDFPageProxy, PDFDocumentLoadingTask } from 'pdfjs-dist'
import { ArrowLeft, ArrowRight, ZoomIn, ZoomOut, Aim } from '@element-plus/icons-vue'

const props = defineProps<{
  /** raw 端点 URL（pdfjs 内部 Range 渐进加载） */
  url: string
}>()

const SCALE_MIN = 0.3
const SCALE_MAX = 4

const wrapRef = ref<HTMLElement | null>(null)
const pageCount = ref(0)
const currentPage = ref(1)
const loading = ref(true)
const error = ref('')
/** fit = 适应容器宽度；manual = 用户缩放倍率 */
const scaleMode = ref<'fit' | 'manual'>('fit')
const manualScale = ref(1.2)
const jumpText = ref('1')

let pdfDoc: PDFDocumentProxy | null = null
let loadTask: PDFDocumentLoadingTask | null = null
let observer: IntersectionObserver | null = null
let resizeObserver: ResizeObserver | null = null
/** 已渲染页缓存（重缩放时重绘） */
const renderedPages = new Map<number, { page: PDFPageProxy; canvas: HTMLCanvasElement; wrap: HTMLElement }>()

function baseScale(): number {
  const el = wrapRef.value
  if (!el) return 1
  return Math.max(0.2, (el.clientWidth - 48) / 1000)
}
function currentScale(): number {
  return scaleMode.value === 'fit' ? baseScale() : manualScale.value
}

async function ensurePdf() {
  if (pdfDoc) return pdfDoc
  const pdfjs = await import('pdfjs-dist')
  pdfjs.GlobalWorkerOptions.workerSrc = new URL('pdfjs-dist/build/pdf.worker.min.mjs', import.meta.url).toString()
  loadTask = pdfjs.getDocument({ url: props.url })
  pdfDoc = await loadTask.promise
  return pdfDoc
}

async function renderPage(n: number) {
  const doc = pdfDoc
  const wrapEl = wrapRef.value
  if (!doc || !wrapEl) return
  const holder = wrapEl.querySelector<HTMLElement>(`[data-page="${n}"]`)
  if (!holder) return
  let entry = renderedPages.get(n)
  if (!entry) {
    const canvas = document.createElement('canvas')
    canvas.style.display = 'block'
    canvas.style.margin = '0 auto'
    holder.appendChild(canvas)
    const page = await doc.getPage(n)
    entry = { page, canvas, wrap: holder }
    renderedPages.set(n, entry)
  }
  const { page, canvas } = entry
  const viewport = page.getViewport({ scale: 1 })
  const scale = currentScale()
  const dpr = window.devicePixelRatio || 1
  canvas.width = Math.floor(viewport.width * scale * dpr)
  canvas.height = Math.floor(viewport.height * scale * dpr)
  canvas.style.width = `${Math.floor(viewport.width * scale)}px`
  canvas.style.height = `${Math.floor(viewport.height * scale)}px`
  const ctx = canvas.getContext('2d')
  if (!ctx) return
  await page.render({
    canvas,
    canvasContext: ctx,
    viewport: page.getViewport({ scale: scale * dpr }),
  }).promise
}

/** 重绘已渲染页（缩放变化/窗口 resize 时）。 */
async function rerenderAll() {
  await Promise.all([...renderedPages.keys()].map(n => renderPage(n)))
}

async function init() {
  loading.value = true
  error.value = ''
  try {
    const doc = await ensurePdf()
    pageCount.value = doc.numPages
    currentPage.value = 1
    jumpText.value = '1'
    // 占位容器：每页一个 div，IntersectionObserver 驱动懒渲染
    await renderPlaceholderPages(doc.numPages)
    setupObserver()
    // 首屏立即渲染
    for (let n = 1; n <= Math.min(3, doc.numPages); n++) {
      void renderPage(n)
    }
  } catch (e) {
    error.value = `PDF 加载失败：${e instanceof Error ? e.message : String(e)}`
  } finally {
    loading.value = false
  }
}

async function renderPlaceholderPages(count: number) {
  const wrapEl = wrapRef.value
  if (!wrapEl) return
  wrapEl.innerHTML = ''
  renderedPages.clear()
  for (let n = 1; n <= count; n++) {
    const holder = document.createElement('div')
    holder.dataset.page = String(n)
    holder.className = 'pdf-page-holder'
    const tip = document.createElement('div')
    tip.className = 'pdf-page-tip'
    tip.textContent = `第 ${n} 页`
    holder.appendChild(tip)
    wrapEl.appendChild(holder)
  }
}

function setupObserver() {
  observer?.disconnect()
  observer = new IntersectionObserver((entries) => {
    for (const en of entries) {
      if (!en.isIntersecting) continue
      const n = Number((en.target as HTMLElement).dataset.page)
      if (n) void renderPage(n)
    }
  }, { root: wrapRef.value, rootMargin: '600px 0px' })
  wrapRef.value?.querySelectorAll('.pdf-page-holder').forEach(el => observer!.observe(el))
}

/** 滚动跟踪当前页（取视口最上方的已渲染页）。 */
function onScroll() {
  const wrapEl = wrapRef.value
  if (!wrapEl) return
  const holders = wrapEl.querySelectorAll<HTMLElement>('.pdf-page-holder')
  const top = wrapEl.getBoundingClientRect().top
  for (const h of holders) {
    if (h.getBoundingClientRect().bottom > top + 40) {
      const n = Number(h.dataset.page)
      if (n) {
        currentPage.value = n
        jumpText.value = String(n)
      }
      break
    }
  }
}

function gotoPage() {
  const n = Math.min(pageCount.value, Math.max(1, parseInt(jumpText.value, 10) || 1))
  jumpText.value = String(n)
  currentPage.value = n
  wrapRef.value?.querySelector(`[data-page="${n}"]`)?.scrollIntoView({ block: 'start' })
  void renderPage(n)
}

function stepPage(delta: number) {
  jumpText.value = String(Math.min(pageCount.value, Math.max(1, currentPage.value + delta)))
  gotoPage()
}

function zoomIn() {
  scaleMode.value = 'manual'
  manualScale.value = Math.min(SCALE_MAX, manualScale.value * 1.2)
  void rerenderAll()
}
function zoomOut() {
  scaleMode.value = 'manual'
  manualScale.value = Math.max(SCALE_MIN, manualScale.value / 1.2)
  void rerenderAll()
}
function fitWidth() {
  scaleMode.value = 'fit'
  void rerenderAll()
}

onMounted(() => {
  void init()
  if (wrapRef.value) {
    resizeObserver = new ResizeObserver(() => {
      if (scaleMode.value === 'fit') void rerenderAll()
    })
    resizeObserver.observe(wrapRef.value)
  }
})

onBeforeUnmount(() => {
  observer?.disconnect()
  resizeObserver?.disconnect()
  renderedPages.clear()
  // 关闭释放：销毁 pdfjs 加载任务（取消进行中的 Range 请求、释放文档与 worker 资源）
  void loadTask?.destroy()
  loadTask = null
  pdfDoc = null
})
</script>

<template>
  <div class="pdf-view">
    <!-- 工具条：页码跳转 + 缩放 -->
    <div class="pdf-bar">
      <button type="button" class="bar-btn" :disabled="currentPage <= 1" title="上一页" @click="stepPage(-1)">
        <el-icon><ArrowLeft /></el-icon>
      </button>
      <input v-model="jumpText" type="number" min="1" :max="pageCount || 1" class="page-input"
             @keydown.enter="gotoPage" @change="gotoPage" />
      <span class="bar-total">/ {{ pageCount || '?' }}</span>
      <button type="button" class="bar-btn" :disabled="currentPage >= pageCount" title="下一页" @click="stepPage(1)">
        <el-icon><ArrowRight /></el-icon>
      </button>
      <span class="bar-sep"></span>
      <button type="button" class="bar-btn" title="缩小" @click="zoomOut">
        <el-icon><ZoomOut /></el-icon>
      </button>
      <button type="button" class="bar-btn" title="放大" @click="zoomIn">
        <el-icon><ZoomIn /></el-icon>
      </button>
      <button type="button" class="bar-btn" :class="{ active: scaleMode === 'fit' }"
              title="适应宽度" @click="fitWidth">
        <el-icon><Aim /></el-icon>
      </button>
    </div>

    <!-- 分页滚动区：占位 div + IntersectionObserver 懒渲染 canvas -->
    <div class="pdf-scroll-body">
      <div ref="wrapRef" class="pdf-scroll" @scroll.passive="onScroll"></div>
      <div v-if="loading" class="pdf-state">
        <span class="state-spin"></span> PDF 加载中…
      </div>
      <div v-else-if="error" class="pdf-state error">{{ error }}</div>
    </div>
  </div>
</template>

<style scoped>
.pdf-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
  position: relative;
}
.pdf-scroll-body {
  position: relative;
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.pdf-bar {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 8px 12px;
  background: #12151c;
  border-bottom: 1px solid #23262e;
  flex-shrink: 0;
}
.bar-btn {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  border: 1px solid #2a2d35;
  border-radius: 6px;
  background: #1a1d24;
  color: #a8afba;
  cursor: pointer;
}
.bar-btn:hover:not(:disabled) {
  color: #fff;
  border-color: #4f5dff;
}
.bar-btn:disabled {
  opacity: 0.4;
  cursor: default;
}
.bar-btn.active {
  color: #8a94ff;
  border-color: #4f5dff;
  background: #232947;
}
.bar-total {
  font-size: 12px;
  color: #a8afba;
  font-variant-numeric: tabular-nums;
}
.bar-sep {
  width: 1px;
  height: 18px;
  background: #2a2d35;
  margin: 0 6px;
}
.page-input {
  width: 56px;
  height: 30px;
  padding: 0 8px;
  border: 1px solid #2a2d35;
  border-radius: 6px;
  background: #1a1d24;
  color: #e5e7eb;
  font-size: 12.5px;
  text-align: center;
  font-variant-numeric: tabular-nums;
}
.page-input:focus {
  outline: none;
  border-color: #4f5dff;
}
.pdf-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  background: #3a3f4a;
  padding: 16px 0 32px;
}
.pdf-state {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  color: #cfd4dc;
  font-size: 13px;
  pointer-events: none;
}
.pdf-state.error {
  color: #f87171;
}
.state-spin {
  width: 16px;
  height: 16px;
  border: 2px solid #4f5dff33;
  border-top-color: #8a94ff;
  border-radius: 50%;
  animation: fv-spin 0.8s linear infinite;
}
@keyframes fv-spin {
  to { transform: rotate(360deg); }
}
:deep(.pdf-page-holder) {
  margin: 0 auto 12px;
  background: #fff;
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.4);
  width: fit-content;
  position: relative;
  min-width: 200px;
  min-height: 120px;
}
:deep(.pdf-page-tip) {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #9ca3af;
  font-size: 12px;
}
</style>
