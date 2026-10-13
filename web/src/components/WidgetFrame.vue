<script setup lang="ts">
// WidgetFrame.vue 界面部件渲染卡（2026-10-13 Widget 机制核心组件）：
// 把 Agent 经 widget_save 写的自渲染页面（widgets/<id>/<entry>）装进沙箱 iframe 展示。
//
// 安全边界与 ArtifactCard 的 HTML 预览一致：sandbox="allow-scripts"（不给 allow-same-origin），
// 部件拿不到主应用 cookie/localStorage/父窗口 DOM；数据进出只走 postMessage。
//
// postMessage 协议（message 事件校验 e.source === iframe.contentWindow）：
//   宿主→部件：{type:'init', payload}（iframe load 或收到 ready 后发，payload=初始 data，
//               JSON.parse 失败给原文）；{type:'data', payload}（widgetBus 该 id 有更新时发）。
//   部件→宿主：{type:'ready'} / {type:'error', message} / {type:'resize', height}。
// payload 文本含 "__reload__"（widget_save 成功后后端推送）→ 重建 iframe 热刷新。
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import type { WidgetInfo } from '@/types'
import { getWidget, reportWidgetError } from '@/api/widgets'
import { useWidgetData } from '@/utils/widgetBus'
import { APP_CONFIG } from '@/config/app'

const props = defineProps<{
  widgetId: string
  title?: string
  /** 初始数据（JSON 字符串，ShowArtifact 的 data 参数透传） */
  data?: string
  sessionId: string
}>()

const info = ref<WidgetInfo | null>(null)
/** 不可用原因：missing=不存在/接口失败，disabled=已停用（不挂 iframe，只渲染提示卡） */
const unavailable = ref<'' | 'missing' | 'disabled'>('')
/** 部件 postMessage 上报的运行时错误（卡片顶部错误条） */
const errorMsg = ref('')
/** 已回报过后端的最后一条错误：同一条错误不重复 report（防刷屏） */
const lastReported = ref('')
const frameHeight = ref(420)
/** 刷新键：__reload__ 或手动刷新时置 Date.now()，src 变化即重建 iframe */
const reloadKey = ref(0)
const iframeEl = ref<HTMLIFrameElement | null>(null)

const displayTitle = computed(() => props.title || info.value?.name || props.widgetId)

/** 部件文件 URL：/api/widgets/<id>/files/<entry>（逐段编码保留斜杠，与 workspaceUrl 同法）。 */
const frameSrc = computed(() => {
  if (!info.value) return ''
  const entry = info.value.entry
    .split('/')
    .filter((s) => s.length > 0)
    .map((s) => encodeURIComponent(s))
    .join('/')
  const base = `${APP_CONFIG.apiBase}/widgets/${encodeURIComponent(props.widgetId)}/files/${entry}`
  return reloadKey.value ? `${base}?v=${reloadKey.value}` : base
})

// 实时数据总线：session/index.vue 把 SSE widget_data 帧 / 快照补发写进来，这里订阅后转给 iframe
const busData = useWidgetData(props.widgetId)

/** payload 统一尝试 JSON.parse（失败给原文）：部件侧收到的总是结构化数据或原文本。 */
function tryParse(raw: string): unknown {
  try {
    return JSON.parse(raw)
  } catch {
    return raw
  }
}

/** 沙箱 iframe 是不透明源，targetOrigin 只能给 '*'（数据本身无敏感性，部件只收不发密钥）。 */
function post(msg: unknown) {
  iframeEl.value?.contentWindow?.postMessage(msg, '*')
}

function sendInit() {
  if (props.data) post({ type: 'init', payload: tryParse(props.data) })
}

/** 重建 iframe（热刷新）：src 加 ?v= 刷新键，:key 绑定 src 保证整棵 iframe 销毁重建。 */
function rebuild() {
  errorMsg.value = ''
  reloadKey.value = Date.now()
}

watch(busData, (payload) => {
  if (!payload) return
  // widget_save 成功后后端推 {"__reload__":true}：代码已更新，整帧重建而不是发数据
  if (payload.includes('__reload__')) {
    rebuild()
    return
  }
  post({ type: 'data', payload: tryParse(payload) })
})

function onMessage(e: MessageEvent) {
  // 只收本卡 iframe 的消息（同页可能有多张部件卡；沙箱源为 null 无法按 origin 过滤）
  if (!iframeEl.value || e.source !== iframeEl.value.contentWindow) return
  const d = e.data as { type?: string; message?: string; height?: number }
  if (!d || typeof d !== 'object') return
  if (d.type === 'ready') {
    sendInit()
  } else if (d.type === 'error') {
    const msg = typeof d.message === 'string' && d.message ? d.message : '部件内部错误'
    errorMsg.value = msg
    // 回报后端（落 System 事件 + 注入 Agent 邮箱）；部件崩溃不影响主程序，失败静默
    if (props.sessionId && msg !== lastReported.value) {
      lastReported.value = msg
      reportWidgetError(props.sessionId, props.widgetId, msg).catch(() => {})
    }
  } else if (d.type === 'resize') {
    const h = Number(d.height)
    if (Number.isFinite(h)) frameHeight.value = Math.min(Math.max(Math.round(h), 120), 2000)
  }
}

function onLoad() {
  sendInit()
}

function openInNewWindow() {
  if (frameSrc.value) window.open(frameSrc.value, '_blank', 'noopener')
}

onMounted(async () => {
  window.addEventListener('message', onMessage)
  try {
    const w = await getWidget(props.widgetId)
    if (!w.enabled) {
      unavailable.value = 'disabled'
      return
    }
    info.value = w
  } catch {
    unavailable.value = 'missing'
  }
})

onUnmounted(() => {
  window.removeEventListener('message', onMessage)
})
</script>

<template>
  <div class="rounded-card border border-line bg-card overflow-hidden">
    <!-- 头部：图标 + 标题 + 部件 ID + 操作（新窗口 / 刷新） -->
    <div class="flex items-center gap-2 px-2.5 py-1.5 border-b border-line text-[11px] text-ink-2">
      <el-icon class="text-primary shrink-0"><Grid /></el-icon>
      <span class="font-bold text-ink truncate" :title="displayTitle">{{ displayTitle }}</span>
      <span class="text-ink-3 font-mono truncate shrink-0" :title="widgetId">{{ widgetId }}</span>
      <span class="ml-auto flex items-center gap-2 shrink-0">
        <template v-if="info">
          <button class="hover:text-primary" title="在新窗口打开" @click="openInNewWindow">新窗口</button>
          <button class="hover:text-primary" title="重新加载部件" @click="rebuild">刷新</button>
        </template>
      </span>
    </div>

    <!-- 部件运行时错误条（已同步回报后端，Agent 可见可自行修代码） -->
    <div v-if="errorMsg"
         class="flex items-start gap-2 px-3 py-2 text-[11px] text-red-400 bg-red-500/10 border-b border-red-500/30">
      <el-icon class="shrink-0 mt-px"><WarningFilled /></el-icon>
      <span class="flex-1 break-all">部件报错：{{ errorMsg }}</span>
      <button class="shrink-0 hover:text-red-300" title="关闭错误条" @click="errorMsg = ''">✕</button>
    </div>

    <!-- 不可用提示卡（不挂 iframe）：不存在/已删除 或 已停用 -->
    <div v-if="unavailable" class="px-3 py-6 text-center text-xs text-ink-3">
      <template v-if="unavailable === 'disabled'">
        部件 <span class="font-mono">{{ widgetId }}</span> 已停用，可在「界面部件」管理页重新启用。
      </template>
      <template v-else>
        部件 <span class="font-mono">{{ widgetId }}</span> 不存在或已被删除。
      </template>
    </div>

    <!-- 部件本体：沙箱 iframe（脚本可跑、无同源权限），高度默认 420px、支持部件 resize 消息调高 -->
    <div v-else-if="info" class="bg-white">
      <iframe :key="frameSrc" ref="iframeEl" :src="frameSrc" sandbox="allow-scripts"
              referrerpolicy="no-referrer" class="w-full border-0 block"
              :style="{ height: frameHeight + 'px' }" :title="displayTitle" @load="onLoad"></iframe>
    </div>

    <!-- 清单加载中 -->
    <div v-else class="px-3 py-6 text-center text-xs text-ink-3">部件加载中…</div>
  </div>
</template>
