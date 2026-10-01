import { fetchJson } from './client'

export interface FileItem {
  path: string
  size: number
  name: string
}

export interface FilesResponse {
  session_id: string
  files: FileItem[]
}

export interface FileContentResponse {
  path: string
  content: string
  /** 完整文件大小（字节），截断时仍返回真实大小 */
  size?: number
  /** 后端按扩展名探测的 MIME 类型 */
  mime?: string
  /** 文本超过 300KB 被截断时为 true，前端应提示下载查看 */
  truncated?: boolean
}

export function listFiles(sessionID: string): Promise<FilesResponse> {
  return fetchJson(`/files?session=${encodeURIComponent(sessionID)}`)
}

export function getFileContent(path: string): Promise<FileContentResponse> {
  return fetchJson(`/files/content?path=${encodeURIComponent(path)}`)
}
