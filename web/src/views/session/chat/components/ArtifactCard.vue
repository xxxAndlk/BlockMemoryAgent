<script setup lang="ts">
// ArtifactCard.vue 对话栏媒体卡片：把 Agent 产出的效果图/视频/音频/HTML 原型直接渲染出来，
// 文档/代码类文件（pdf/markdown/code/json/csv/xlsx/txt/unknown）渲染紧凑文件卡
//（类型图标 + 文件名 + 大小 + 预览/下载/在文件夹中显示，TODO #26 阶段 A）。
//
// 数据只带工作区相对路径（后端 ShowArtifact 登记 / 工具输出兜底识别），媒体本体走
// /api/sessions/:id/workspace/*path 流式读取（带 Range，视频可拖进度）；文件卡的
// 下载/在文件夹中显示走 /api/files/raw|reveal（需绝对路径，经 fileOpener 拼会话工作区）。
//
// HTML 用 sandbox="allow-scripts"（**不给** allow-same-origin）：产物里的 CSS/JS 正常执行，
// 但拿不到主应用的 cookie/localStorage/父窗口 DOM——效果图要能跑起来，同时不越权。
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import type { ArtifactRef } from '@/types'
import { workspaceUrl } from '@/api/session'
import { revealFile } from '@/api/files'
import { APIError } from '@/api/client'
import { useFileOpener } from '@/composables/fileOpener'

const props = defineProps<{
  artifact: ArtifactRef
  sessionId: string
}>()

/** 内嵌预览的四类媒体；其余 kind 一律走紧凑文件卡（图片/视频/音频/HTML 行为不动）。 */
const MEDIA_KINDS = new Set(['image', 'video', 'audio', 'html'])

const url = computed(() => workspaceUrl(props.sessionId, props.artifact.path))
const title = computed(() => props.artifact.title || props.artifact.path.split('/').pop() || props.artifact.path)
const isHtml = computed(() => props.artifact.kind === 'html')
const isMedia = computed(() => MEDIA_KINDS.has(props.artifact.kind))
const imageFailed = ref(false)

/** 文件打开器（session/index.vue provide）：预览定位/绝对路径解析；会话外复用时为 null。 */
const fileOpener = useFileOpener()

/** 工作区绝对路径：raw/reveal 端点只认绝对路径；无工作区且相对路径时返回 null。
 *  读 workDir 建立响应式依赖：切换会话（工作区变化）时重算。 */
const absPath = computed(() => {
  void fileOpener?.workDir.value
  return fileOpener?.resolveAbsolute(props.artifact.path) ?? null
})

/** 下载走 /api/files/raw（download=1 给 attachment 头），同源相对路径即可。 */
const downloadUrl = computed(() =>
  absPath.value ? `/api/files/raw?path=${encodeURIComponent(absPath.value)}&download=1` : '')

/** 紧凑卡类型元数据：图标（全局注册的 Element Plus 图标名）+ 徽标 + 颜色。 */
const KIND_META: Record<string, { icon: string; label: string; cls: string }> = {
  pdf: { icon: 'Tickets', label: 'PDF', cls: 'text-red-400' },
  markdown: { icon: 'Notebook', label: 'Markdown', cls: 'text-blue-400' },
  json: { icon: 'Coin', label: 'JSON', cls: 'text-amber-400' },
  csv: { icon: 'Grid', label: '表格', cls: 'text-emerald-400' },
  xlsx: { icon: 'Grid', label: 'Excel', cls: 'text-emerald-400' },
  code: { icon: 'Document', label: '代码', cls: 'text-sky-400' },
  txt: { icon: 'Tickets', label: '文本', cls: 'text-ink-2' },
}
const kindMeta = computed(() =>
  KIND_META[props.artifact.kind] || { icon: 'QuestionFilled', label: props.artifact.kind || '文件', cls: 'text-ink-3' })

function formatSize(bytes?: number) {
  if (bytes == null) return ''
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / 1024 / 1024).toFixed(1) + ' MB'
}

/** HTML 预览默认收起：长报告类产物不至于把对话流撑到几屏。 */
const htmlExpanded = ref(false)

/** 新窗口打开（模板里不能直接引用 window）。 */
function openInNewWindow() {
  window.open(url.value, '_blank', 'noopener')
}

async function copyPath() {
  try {
    await navigator.clipboard.writeText(props.artifact.path)
    ElMessage.success('路径已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择路径')
  }
}

/** 预览 = 打开全局全屏 FileViewer（TODO #26 阶段 B 统一全屏层）。 */
function previewInTree() {
  if (!fileOpener) {
    ElMessage.info('文件：' + props.artifact.path)
    return
  }
  fileOpener.openInViewer(props.artifact.path, { size: props.artifact.size ?? undefined })
}

/** 在文件夹中显示：绝对路径调 reveal；无工作区/无桌面时给可懂提示。 */
async function revealInFolder() {
  if (!absPath.value) {
    ElMessage.warning('该会话没有工作区，无法定位文件')
    return
  }
  try {
    await revealFile(absPath.value)
    ElMessage.success('已在文件管理器中定位')
  } catch (e) {
    const noDesktop = e instanceof APIError && (e.status === 501 || e.status === 500)
    ElMessage.error(noDesktop ? '当前环境无桌面，无法打开文件管理器' : `定位失败：${e instanceof Error ? e.message : String(e)}`)
  }
}
</script>

<template>
  <div class="rounded-card border border-line bg-card overflow-hidden" :data-bma-file-card="absPath ?? undefined">
    <!-- 头部：图标 + 文件名 + 操作（媒体卡与文件卡共用） -->
    <div class="flex items-center gap-2 px-2.5 py-1.5 border-b border-line text-[11px] text-ink-2">
      <el-icon class="text-primary shrink-0">
        <component :is="artifact.kind === 'image' ? 'Picture' : artifact.kind === 'video' ? 'VideoPlay' : artifact.kind === 'audio' ? 'Headset' : isMedia ? 'Monitor' : kindMeta.icon" />
      </el-icon>
      <span class="font-bold text-ink truncate" :title="title">{{ title }}</span>
      <span v-if="artifact.caption" class="text-ink-3 truncate" :title="artifact.caption">· {{ artifact.caption }}</span>
      <span v-if="artifact.inferred" class="text-[10px] px-1 rounded bg-page border border-line shrink-0"
            title="从工具输出里识别出的产物路径（Agent 未显式调用 ShowArtifact）">自动识别</span>
      <span class="ml-auto flex items-center gap-2 shrink-0">
        <template v-if="isMedia">
          <button class="hover:text-primary" title="在新窗口打开" @click="openInNewWindow">新窗口</button>
          <button class="hover:text-primary" title="复制工作区相对路径" @click="copyPath">复制路径</button>
          <template v-if="isHtml">
            <button class="hover:text-primary" @click="htmlExpanded = !htmlExpanded">
              {{ htmlExpanded ? '收起预览' : '展开预览' }}
            </button>
          </template>
        </template>
        <template v-else>
          <button class="hover:text-primary" title="在右侧文件树中打开预览" @click="previewInTree">预览</button>
          <a v-if="downloadUrl" class="hover:text-primary" :href="downloadUrl" rel="noreferrer" title="下载文件">下载</a>
          <button class="hover:text-primary" title="在系统文件管理器中定位" @click="revealInFolder">在文件夹中显示</button>
        </template>
      </span>
    </div>

    <!-- 图片：点击新窗口看原图（对话栏里限高，避免长图撑爆） -->
    <div v-if="artifact.kind === 'image'" class="bg-page">
      <img v-if="!imageFailed" :src="url" :alt="title"
           class="max-h-[420px] w-auto max-w-full mx-auto cursor-zoom-in block"
           loading="lazy" @error="imageFailed = true"
           @click="openInNewWindow" />
      <div v-else class="px-3 py-6 text-center text-xs text-ink-3">
        图片加载失败（可能已被移动或删除）：<span class="font-mono">{{ artifact.path }}</span>
      </div>
    </div>

    <!-- 视频/音频：原生控件（Range 由后端 ServeContent 提供，可拖进度） -->
    <div v-else-if="artifact.kind === 'video'" class="bg-black/90">
      <video :src="url" controls preload="metadata" class="max-h-[420px] w-full"></video>
    </div>
    <div v-else-if="artifact.kind === 'audio'" class="px-3 py-3 bg-page">
      <audio :src="url" controls preload="metadata" class="w-full"></audio>
    </div>

    <!-- HTML：沙箱 iframe 渲染（脚本可跑、无同源权限） -->
    <div v-else-if="isHtml" class="bg-white">
      <iframe v-if="htmlExpanded" :src="url" sandbox="allow-scripts" referrerpolicy="no-referrer"
              class="w-full h-[420px] border-0 block" :title="title"></iframe>
      <button v-else class="w-full px-3 py-6 text-xs text-ink-3 hover:text-primary" @click="htmlExpanded = true">
        点击展开网页预览（沙箱内渲染，脚本可运行）
      </button>
    </div>

    <!-- 紧凑文件卡（TODO #26 A）：类型图标 + 大小/类型徽标，操作在头部 -->
    <div v-else class="flex items-center gap-2.5 px-3 py-2.5 bg-page">
      <el-icon class="text-xl shrink-0" :class="kindMeta.cls">
        <component :is="kindMeta.icon" />
      </el-icon>
      <div class="min-w-0 flex-1">
        <div class="truncate text-[13px] text-ink" :title="absPath || artifact.path">{{ title }}</div>
        <div class="text-[11px] text-ink-3">
          <span>{{ kindMeta.label }}</span>
          <span v-if="artifact.size != null"> · {{ formatSize(artifact.size) }}</span>
        </div>
      </div>
      <div class="text-[11px] text-ink-3 font-mono truncate max-w-[45%]" :title="artifact.path">{{ artifact.path }}</div>
    </div>
  </div>
</template>
