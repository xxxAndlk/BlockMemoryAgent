import { fetchJson } from './client'

export interface BrowseResult {
  path: string
  parent: string
  dirs: { name: string; path: string }[]
}

export function browseFS(path: string): Promise<BrowseResult> {
  return fetchJson(`/fs/browse?path=${encodeURIComponent(path)}`)
}
