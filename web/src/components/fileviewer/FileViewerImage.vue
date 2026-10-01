<script setup lang="ts">
// FileViewer 图片画廊视图（TODO #26 阶段 B）：同目录图片左右切换（←→ 键由外层
// FileViewer 派发到这里）、滚轮缩放（0.1x–5x）、适应窗口/原始尺寸切换、放大后拖动平移。
import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue'
import { ArrowLeft, ArrowRight, ZoomIn, ZoomOut, ScaleToOriginal, Aim } from '@element-plus/icons-vue'

const props = defineProps<{
  /** 同目录图片绝对路径列表（含当前文件） */
  images: string[]
  /** 当前图片下标 */
  index: number
}>()
const emit = defineEmits<{ (e: 'update:index', v: number): void }>()

const SCALE_MIN = 0.1
const SCALE_MAX = 5

const containerRef = ref<HTMLElement | null>(null)
const stageRef = ref<HTMLElement | null>(null)
const naturalW = ref(0)
const naturalH = ref(0)
const scale = ref(1)
/** true = 跟随窗口缩放（适应窗口）；false = 用户手动缩放/原始尺寸 */
const fitMode = ref(true)
const offset = ref({ x: 0, y: 0 })
const dragging = ref(false)

const current = computed(() => props.images[props.index] || '')
const rawUrl = (p: string) => `/api/files/raw?path=${encodeURIComponent(p)}`
const hasPrev = computed(() => props.index > 0)
const hasNext = computed(() => props.index < props.images.length - 1)

function prev() {
  if (hasPrev.value) emit('update:index', props.index - 1)
}
function next() {
  if (hasNext.value) emit('update:index', props.index + 1)
}

/** 图片切换：重置缩放/平移，重新走适应窗口。 */
watch(current, () => resetView())

function resetView() {
  scale.value = 1
  offset.value = { x: 0, y: 0 }
  fitMode.value = true
  applyTransform()
}

function onImgLoad(e: Event) {
  const img = e.target as HTMLImageElement
  naturalW.value = img.naturalWidth
  naturalH.value = img.naturalHeight
  applyTransform()
}

/** 计算适应窗口的缩放比（留 24px 边距）。 */
function fitScale(): number {
  const el = containerRef.value
  if (!el || !naturalW.value || !naturalH.value) return 1
  const w = (el.clientWidth - 48) / naturalW.value
  const h = (el.clientHeight - 48) / naturalH.value
  return Math.min(w, h, 1) // 适应窗口不放大小图
}

function applyTransform() {
  const el = stageRef.value
  if (!el) return
  const s = fitMode.value ? fitScale() : scale.value
  el.style.transform = `translate(${offset.value.x}px, ${offset.value.y}px) scale(${s})`
}

function zoomBy(factor: number) {
  fitMode.value = false
  scale.value = Math.min(SCALE_MAX, Math.max(SCALE_MIN, scale.value * factor))
  applyTransform()
}

function zoomIn() { zoomBy(1.25) }
function zoomOut() { zoomBy(0.8) }

/** 适应窗口 / 原始尺寸切换。 */
function toggleFit() {
  if (fitMode.value) {
    fitMode.value = false
    scale.value = 1 // 原始尺寸
    offset.value = { x: 0, y: 0 }
  } else {
    resetView()
    return
  }
  applyTransform()
}

/** 滚轮缩放（阻止页面滚动穿透到遮罩层外）。 */
function onWheel(e: WheelEvent) {
  e.preventDefault()
  zoomBy(e.deltaY < 0 ? 1.12 : 1 / 1.12)
}

// ── 拖动平移 ──
let dragStart = { x: 0, y: 0, ox: 0, oy: 0 }

function onMouseDown(e: MouseEvent) {
  dragging.value = true
  dragStart = { x: e.clientX, y: e.clientY, ox: offset.value.x, oy: offset.value.y }
  window.addEventListener('mousemove', onMouseMove)
  window.addEventListener('mouseup', onMouseUp)
}
function onMouseMove(e: MouseEvent) {
  if (!dragging.value) return
  offset.value = { x: dragStart.ox + e.clientX - dragStart.x, y: dragStart.oy + e.clientY - dragStart.y }
  applyTransform()
}
function onMouseUp() {
  dragging.value = false
  window.removeEventListener('mousemove', onMouseMove)
  window.removeEventListener('mouseup', onMouseUp)
}

// ── 窗口尺寸变化：适应窗口模式下重算 ──
let resizeObserver: ResizeObserver | null = null
onMounted(() => {
  if (containerRef.value) {
    resizeObserver = new ResizeObserver(() => { if (fitMode.value) applyTransform() })
    resizeObserver.observe(containerRef.value)
  }
})
onBeforeUnmount(() => {
  resizeObserver?.disconnect()
  window.removeEventListener('mousemove', onMouseMove)
  window.removeEventListener('mouseup', onMouseUp)
})

defineExpose({ prev, next })
</script>

<template>
  <div class="img-view">
    <!-- 主区：图片 + 左右切换 -->
    <div ref="containerRef" class="img-stage-wrap" :class="{ grabbing: dragging }"
         @wheel="onWheel" @mousedown="onMouseDown">
      <div ref="stageRef" class="img-stage">
        <img v-if="current" :src="rawUrl(current)" :alt="current" draggable="false"
             class="img-main" @load="onImgLoad" />
      </div>
      <!-- 多图时的左右切换按钮 -->
      <button v-if="hasPrev" type="button" class="nav-btn left" title="上一张（←）" @click.stop="prev">
        <el-icon :size="20"><ArrowLeft /></el-icon>
      </button>
      <button v-if="hasNext" type="button" class="nav-btn right" title="下一张（→）" @click.stop="next">
        <el-icon :size="20"><ArrowRight /></el-icon>
      </button>
    </div>

    <!-- 底部控制条 -->
    <div class="img-bar">
      <button type="button" class="bar-btn" :disabled="!hasPrev" title="上一张（←）" @click="prev">
        <el-icon><ArrowLeft /></el-icon>
      </button>
      <span v-if="images.length > 1" class="bar-counter">{{ index + 1 }} / {{ images.length }}</span>
      <button type="button" class="bar-btn" :disabled="!hasNext" title="下一张（→）" @click="next">
        <el-icon><ArrowRight /></el-icon>
      </button>
      <span class="bar-sep"></span>
      <button type="button" class="bar-btn" title="缩小" @click="zoomOut">
        <el-icon><ZoomOut /></el-icon>
      </button>
      <span class="bar-zoom">{{ ((fitMode ? fitScale() : scale) * 100).toFixed(0) }}%</span>
      <button type="button" class="bar-btn" title="放大" @click="zoomIn">
        <el-icon><ZoomIn /></el-icon>
      </button>
      <button type="button" class="bar-btn" :class="{ active: fitMode }"
              :title="fitMode ? '适应窗口中（点击切换原始尺寸）' : '原始尺寸（点击切换适应窗口）'"
              @click="toggleFit">
        <el-icon v-if="fitMode"><Aim /></el-icon>
        <el-icon v-else><ScaleToOriginal /></el-icon>
      </button>
    </div>
  </div>
</template>

<style scoped>
.img-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}
.img-stage-wrap {
  position: relative;
  flex: 1;
  min-height: 0;
  overflow: hidden;
  display: flex;
  align-items: center;
  justify-content: center;
  background: #0b0d12;
  cursor: grab;
}
.img-stage-wrap.grabbing {
  cursor: grabbing;
}
.img-stage {
  display: flex;
  align-items: center;
  justify-content: center;
  will-change: transform;
}
.img-main {
  max-width: none;
  user-select: none;
  box-shadow: 0 8px 40px rgba(0, 0, 0, 0.5);
}
.nav-btn {
  position: absolute;
  top: 50%;
  transform: translateY(-50%);
  width: 44px;
  height: 64px;
  display: flex;
  align-items: center;
  justify-content: center;
  border: none;
  border-radius: 8px;
  background: rgba(20, 23, 30, 0.65);
  color: #cfd4dc;
  cursor: pointer;
  transition: background-color 0.15s ease;
}
.nav-btn:hover {
  background: rgba(60, 70, 100, 0.75);
  color: #fff;
}
.nav-btn.left { left: 16px; }
.nav-btn.right { right: 16px; }

.img-bar {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  padding: 8px 12px;
  background: #12151c;
  border-top: 1px solid #23262e;
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
.bar-counter {
  min-width: 56px;
  text-align: center;
  font-size: 12px;
  color: #a8afba;
  font-variant-numeric: tabular-nums;
}
.bar-zoom {
  min-width: 48px;
  text-align: center;
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
</style>
