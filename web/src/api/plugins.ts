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

/** docker 依赖自检结果（与后端 plugins.DockerReport 对齐）。 */
export interface DockerContainerStatus {
  name: string
  service?: string
  state: string
  status?: string
  image?: string
  workdirs?: string[]
  plugin_keeper?: boolean
}

export interface DockerReport {
  available: boolean
  version?: string
  error?: string
  /** 是否有启用中的 docker 插件（无依赖时 docker 不可用不算问题） */
  required?: boolean
  images?: { plugin: string; image: string; present: boolean }[]
  containers?: DockerContainerStatus[]
  missing_images?: string[]
  stopped_containers?: string[]
}

export interface DockerCleanupResult {
  removed_containers?: string[]
  pruned_images?: number
  reclaimed?: string
  errors?: string[]
}

/** docker 依赖自检（只读）：镜像就绪 + bma 项目容器状态。 */
export function fetchDockerReport(): Promise<DockerReport> {
  return fetchJson('/plugins/docker')
}

/** 清理 docker 残留：默认只删已停止的 bma-plugin-* 容器 + 悬空镜像；all=true 含运行中 keeper。 */
export function cleanupDocker(all = false): Promise<{ status: string; cleanup: DockerCleanupResult; docker: DockerReport }> {
  return fetchJson(`/plugins/cleanup${all ? '?all=true' : ''}`, { method: 'POST' })
}
