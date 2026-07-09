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
}

export function listFiles(sessionID: string): Promise<FilesResponse> {
  return fetchJson(`/files?session=${encodeURIComponent(sessionID)}`)
}

export function getFileContent(path: string): Promise<FileContentResponse> {
  return fetchJson(`/files/content?path=${encodeURIComponent(path)}`)
}
