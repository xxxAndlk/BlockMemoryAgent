import { fetchJson } from './client'

/** 插件信息（与后端 plugins.Info 对齐，任务 58/59 遗留 Web 侧同步）。 */
export interface PluginInfo {
  id: string
  name: string
  version?: string
  description?: string
  kind: string
  state: string
  enabled: boolean
  tools?: string[]
  url?: string
  roles?: string[]
  missing_env?: string[]
  last_error?: string
}

export function listPlugins(): Promise<{ plugins: PluginInfo[] }> {
  return fetchJson('/plugins')
}

export function enablePlugin(id: string): Promise<PluginInfo> {
  return fetchJson(`/plugins/${id}/enable`, { method: 'POST' })
}

export function disablePlugin(id: string): Promise<PluginInfo> {
  return fetchJson(`/plugins/${id}/disable`, { method: 'POST' })
}

export function reloadPlugins(): Promise<{ status: string; plugins: PluginInfo[] }> {
  return fetchJson('/plugins/reload', { method: 'POST' })
}
