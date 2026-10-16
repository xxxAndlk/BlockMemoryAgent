import { fetchJson } from './client'
import type { WidgetInfo, WidgetTrashEntry } from '@/types'

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

/** 部件上行 action 上报：后端落 System 事件 + 运行中会话注入 Agent 邮箱（协议仿 report，payload 序列化为 JSON 文本）。 */
export function sendWidgetAction(
  sessionId: string,
  widgetId: string,
  action: string,
  payload: unknown,
): Promise<void> {
  let payloadText = 'null'
  try {
    payloadText = JSON.stringify(payload ?? null)
  } catch {
    // 循环引用等不可序列化 payload：置 null，不让上报通道挂掉
  }
  return fetchJson('/widgets/action', {
    method: 'POST',
    body: JSON.stringify({
      session_id: sessionId,
      widget_id: widgetId,
      action,
      payload: payloadText,
    }),
  })
}

/** 删除部件（软删进后端回收站 widgets/.trash/<id>.<timestamp>/，可恢复）。 */
export function deleteWidget(id: string): Promise<void> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/** 回收站列表（软删目录条目，name 为恢复用的目录名）。 */
export function listTrash(): Promise<{ trash: WidgetTrashEntry[] }> {
  return fetchJson('/widgets/trash')
}

/** 从回收站恢复部件（name = 回收站目录名）。 */
export function restoreWidget(name: string): Promise<WidgetInfo> {
  return fetchJson(`/widgets/trash/${encodeURIComponent(name)}/restore`, { method: 'POST' })
}
