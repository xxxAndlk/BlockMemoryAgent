import { fetchJson } from './client'
import type { WidgetConnector, WidgetInfo, WidgetTrashEntry } from '@/types'

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

// ---- 连接器（2026-10-17 Widget「连接器」：万物皆可外接）----

/** 新建部件（含连接器）请求体；不带 files 时 api/mcp/webhook 为裸接件（后端不生成 index.html）。 */
export interface CreateWidgetBody {
  /** 缺省由后端生成。 */
  id?: string
  name: string
  description?: string
  connector: WidgetConnector
  files?: { path: string; content: string }[]
}

/**
 * 新建部件（含连接器）。webhook 类型响应 connector.token 为明文，且带 hook_url
 * （"/api/widgets/<id>/hook/<token>"，展示时拼 origin 成完整回调地址）。
 */
export function createWidget(body: CreateWidgetBody): Promise<WidgetInfo> {
  return fetchJson('/widgets', { method: 'POST', body: JSON.stringify(body) })
}

/** 更新连接器请求体：connector 字段 + regen_token（true 时后端重新生成 webhook token，响应带新 hook_url）。 */
export type UpdateConnectorBody = WidgetConnector & { regen_token?: boolean }

export function updateWidgetConnector(id: string, body: UpdateConnectorBody): Promise<WidgetInfo> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}/connector`, { method: 'PUT', body: JSON.stringify(body) })
}

/** 立即执行一次连接器（api/mcp），返回 {"ok": true, "data": "..."}（data 为最新拉取结果文本）。 */
export function pollWidget(id: string): Promise<{ ok: boolean; data?: string }> {
  return fetchJson(`/widgets/${encodeURIComponent(id)}/poll`, { method: 'POST' })
}
