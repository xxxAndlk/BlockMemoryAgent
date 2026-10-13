import { fetchJson } from './client'
import type { WidgetInfo } from '@/types'

/** 界面部件 API（与后端 widgets_http.go 对齐，2026-10-13 Widget 机制）。 */

export function listWidgets(): Promise<{ widgets: WidgetInfo[] }> {
  return fetchJson('/widgets')
}

export function getWidget(id: string): Promise<WidgetInfo> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}`)
}

export function enableWidget(id: string): Promise<WidgetInfo> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}/enable`, { method: 'POST' })
}

export function disableWidget(id: string): Promise<WidgetInfo> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}/disable`, { method: 'POST' })
}

export function reloadWidgets(): Promise<{ widgets: WidgetInfo[] }> {
  return fetchJson('/widgets/reload', { method: 'POST' })
}

/** 部件运行错误回报：后端落 System 会话事件（用户可见），会话运行中时同时注入 MetaAgent 邮箱。 */
export function reportWidgetError(sessionId: string, widgetId: string, error: string): Promise<void> {
  return fetchJson('/widgets/report', {
    method: 'POST',
    body: JSON.stringify({ session_id: sessionId, widget_id: widgetId, error }),
  })
}
