<script setup lang="ts">
// GenericConnectorCard.vue 裸接件通用数据卡（2026-10-17 Widget「连接器」）：
// api/mcp/webhook 类型且无 entry 文件的部件不走 iframe——实时数据经 widgetBus 推送进来，
// 本组件直接渲染：payload 能 JSON.parse 则 2 空格 pretty-print（等宽 pre），失败显示原文本。
// 「立即刷新」对 api/mcp 手动触发一次后端拉取，返回 data 本地直显（不等 SSE 定时推送）。
import { computed, ref, watch } from 'vue'
import type { WidgetConnector } from '@/types'
import { pollWidget } from '@/api/widgets'
import { useWidgetData } from '@/utils/widgetBus'

const props = defineProps<{
  widgetId: string
  /** 连接器配置（取 info.connector；裸接件必带，缺省也能渲染空态）。 */
  connector?: WidgetConnector
}>()

/** 四种连接器类型徽标配色（url/api/mcp/webhook 四色小 tag）。 */
const CONNECTOR_TAG: Record<string, { label: string; type: 'primary' | 'success' | 'warning' | 'danger' }> = {
  url: { label: 'url', type: 'primary' },
  api: { label: 'api', type: 'success' },
  mcp: { label: 'mcp', type: 'warning' },
  webhook: { label: 'webhook', type: 'danger' },
}

// 实时数据总线：与 WidgetFrame 同一订阅入口（SSE 帧 / 快照补发由会话页写入）
const busData = useWidgetData(props.widgetId)
/** 直显文本：JSON 则 pretty-print，否则原文 */
const displayText = ref('')
/** 上次数据更新时间（本地时戳，watch busData / 手动 poll 时刷新） */
const lastUpdate = ref('')
const polling = ref(false)
const pollError = ref('')

const tag = computed(() => (props.connector ? CONNECTOR_TAG[props.connector.type] : undefined))
const target = computed(() => props.connector?.target || '')
/** poll 仅对 api/mcp 有意义（webhook 数据由外部系统推送进来，无源可拉）。 */
const canPoll = computed(() => props.connector?.type === 'api' || props.connector?.type === 'mcp')

function renderPayload(raw: string) {
  try {
    displayText.value = JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    displayText.value = raw
  }
}

function touch() {
  lastUpdate.value = new Date().toLocaleTimeString()
}

// immediate：总线里可能已有快照补发的值（组件后挂载也能直显），空串走空态
watch(busData, (payload) => {
  if (!payload) return
  renderPayload(payload)
  touch()
}, { immediate: true })

async function pollNow() {
  polling.value = true
  pollError.value = ''
  try {
    const res = await pollWidget(props.widgetId)
    if (res.ok && typeof res.data === 'string') {
      renderPayload(res.data)
      touch()
    }
  } catch (e) {
    pollError.value = e instanceof Error ? e.message : String(e)
  } finally {
    polling.value = false
  }
}
</script>

<template>
  <div class="px-3 py-2 space-y-2 min-h-[160px] max-h-[480px] overflow-y-auto">
    <!-- 顶部一行：类型徽标 + 目标（截断）+ 上次更新 + 立即刷新 -->
    <div class="flex items-center gap-2 text-[11px] text-ink-2">
      <el-tag v-if="tag" size="small" :type="tag.type" effect="plain">{{ tag.label }}</el-tag>
      <span v-if="target" class="font-mono truncate" :title="target">{{ target }}</span>
      <span v-else class="text-ink-3">裸接件</span>
      <span class="ml-auto shrink-0 text-ink-3">{{ lastUpdate ? `上次更新 ${lastUpdate}` : '' }}</span>
      <el-button v-if="canPoll" size="small" text type="primary" class="!px-1 shrink-0"
                 :loading="polling" @click="pollNow">立即刷新</el-button>
    </div>

    <!-- 数据本体：JSON pretty-print（等宽）/ 原文直显 -->
    <pre v-if="displayText"
         class="rounded-md border border-line bg-page px-3 py-2 text-[11px] font-mono text-ink whitespace-pre-wrap break-all !m-0">{{
      displayText
    }}</pre>
    <div v-else class="rounded-md border border-dashed border-line px-3 py-6 text-center text-[11px] text-ink-3">
      等待数据推送…
    </div>

    <div v-if="pollError" class="text-[11px] text-red-400 break-all">刷新失败：{{ pollError }}</div>
  </div>
</template>
