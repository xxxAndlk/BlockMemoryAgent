import { fetchJson } from './client'

export interface BrowseResult {
  path: string
  parent: string
  dirs: { name: string; path: string }[]
}

export function browseFS(path: string): Promise<BrowseResult> {
  return fetchJson(`/fs/browse?path=${encodeURIComponent(path)}`)
}

/**
 * 调起**系统原生**目录选择框（在运行服务的这台电脑上弹出，即用户眼前的资源管理器式选择器）。
 * 返回值：用户选中的绝对路径；`path === ''` 表示取消。
 * 平台不支持（501）/ 超时（504）/ 对话框被占用（409）时抛 APIError，调用方应退回网页版选择器。
 */
export function pickSystemDir(): Promise<{ path: string }> {
  return fetchJson('/fs/pick-dir', { method: 'POST', timeoutMs: 5 * 60 * 1000 })
}
