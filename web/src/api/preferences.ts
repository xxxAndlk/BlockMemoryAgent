import { fetchJson } from './client'

/** 全文档案（用户画像 / 项目偏好），与后端 GET/PUT 对齐。 */
export interface DocProfile {
  path?: string
  content: string
}

export function getProfile(): Promise<DocProfile> {
  return fetchJson('/profile')
}

export function saveProfile(content: string): Promise<{ ok: boolean }> {
  return fetchJson('/profile', { method: 'PUT', body: JSON.stringify({ content }) })
}

export function getProjectPreferences(workDir?: string): Promise<DocProfile> {
  // 可选 workDir：按会话目录解析 .bma/project_preferences.md（不传=server 进程目录）
  const q = workDir ? `?work_dir=${encodeURIComponent(workDir)}` : ''
  return fetchJson(`/project/preferences${q}`)
}

export function saveProjectPreferences(content: string, workDir?: string): Promise<{ ok: boolean }> {
  return fetchJson('/project/preferences', {
    method: 'PUT',
    body: JSON.stringify({ content, work_dir: workDir || undefined }),
  })
}
