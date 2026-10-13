import { ref, type Ref } from 'vue'

/**
 * 界面部件实时数据总线（2026-10-13 Widget 机制，零依赖）。
 *
 * 后端 widget_data 瞬时通道（SSE 帧 / 会话快照补发）→ 会话页 setWidgetData 写入这里；
 * WidgetFrame 组件 useWidgetData 订阅同一 widgetId 的 Ref，变化即 postMessage 给部件 iframe。
 * 模块级 Map：widgetId → 最新 payload 文本（最新值覆盖，与后端语义一致）。
 */
const store = new Map<string, Ref<string>>()

/** 写入某部件的最新 payload（不存在则先建空 Ref）。 */
export function setWidgetData(widgetId: string, payload: string) {
  let r = store.get(widgetId)
  if (!r) {
    r = ref('')
    store.set(widgetId, r)
  }
  r.value = payload
}

/** 订阅某部件的实时数据 Ref（不存在则建空 Ref，首帧到达前为空串）。 */
export function useWidgetData(widgetId: string): Ref<string> {
  let r = store.get(widgetId)
  if (!r) {
    r = ref('')
    store.set(widgetId, r)
  }
  return r
}
