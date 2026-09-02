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

export function getProjectPreferences(): Promise<DocProfile> {
  return fetchJson('/project/preferences')
}

export function saveProjectPreferences(content: string): Promise<{ ok: boolean }> {
  return fetchJson('/project/preferences', { method: 'PUT', body: JSON.stringify({ content }) })
}
