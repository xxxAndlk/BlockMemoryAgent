<script setup lang="ts">
// ArtifactCard.vue 对话栏媒体卡片：把 Agent 产出的效果图/视频/音频/HTML 原型直接渲染出来。
//
// 数据只带工作区相对路径（后端 ShowArtifact 登记 / 工具输出兜底识别），媒体本体走
// /api/sessions/:id/workspace/*path 流式读取（带 Range，视频可拖进度）。
//
// HTML 用 sandbox="allow-scripts"（**不给** allow-same-origin）：产物里的 CSS/JS 正常执行，
// 但拿不到主应用的 cookie/localStorage/父窗口 DOM——效果图要能跑起来，同时不越权。
import { computed, ref } from 'vue'
import { ElMessage } from 'element-plus'
import type { ArtifactRef } from '@/types'
import { workspaceUrl } from '@/api/session'

const props = defineProps<{
  artifact: ArtifactRef
  sessionId: string
}>()

const url = computed(() => workspaceUrl(props.sessionId, props.artifact.path))
const title = computed(() => props.artifact.title || props.artifact.path.split('/').pop() || props.artifact.path)
const isHtml = computed(() => props.artifact.kind === 'html')
const imageFailed = ref(false)

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
</script>

<template>
  <div class="rounded-card border border-line bg-card overflow-hidden">
    <div class="flex items-center gap-2 px-2.5 py-1.5 border-b border-line text-[11px] text-ink-2">
      <el-icon class="text-primary shrink-0">
        <component :is="artifact.kind === 'image' ? 'Picture' : artifact.kind === 'video' ? 'VideoPlay' : artifact.kind === 'audio' ? 'Headset' : 'Monitor'" />
      </el-icon>
      <span class="font-bold text-ink truncate" :title="title">{{ title }}</span>
      <span v-if="artifact.caption" class="text-ink-3 truncate" :title="artifact.caption">· {{ artifact.caption }}</span>
      <span v-if="artifact.inferred" class="text-[10px] px-1 rounded bg-page border border-line shrink-0"
            title="从工具输出里识别出的产物路径（Agent 未显式调用 ShowArtifact）">自动识别</span>
      <span class="ml-auto flex items-center gap-2 shrink-0">
        <button class="hover:text-primary" title="在新窗口打开" @click="openInNewWindow">新窗口</button>
        <button class="hover:text-primary" title="复制工作区相对路径" @click="copyPath">复制路径</button>
        <template v-if="isHtml">
          <button class="hover:text-primary" @click="htmlExpanded = !htmlExpanded">
            {{ htmlExpanded ? '收起预览' : '展开预览' }}
          </button>
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

    <!-- 兜底：未知类型（理论上被推断正则挡住） -->
    <div v-else class="px-3 py-3 text-xs text-ink-3 font-mono">{{ artifact.path }}</div>
  </div>
</template>
