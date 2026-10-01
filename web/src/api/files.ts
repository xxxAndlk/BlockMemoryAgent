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
  /** 完整文件大小（字节），截断/分段时仍返回真实大小 */
  size?: number
  /** 完整文件大小（字节，TODO #26 D 分段读取新增；与 size 同值） */
  total_size?: number
  /** 后端按扩展名探测的 MIME 类型 */
  mime?: string
  /** 还有未返回的数据时为 true，前端应提示「加载更多」或下载查看 */
  truncated?: boolean
  /** 下一字节偏移（TODO #26 D 分段读取）：还有数据时非空，取尽时为 null */
  next_offset?: number | null
  /** 磁盘 mtime（Unix 毫秒）：前端打开文件时记录，保存时作 base_mtime 冲突检测 */
  mtime?: number
}

/** PUT /api/files/content 保存响应（TODO #26 阶段 G）。 */
export interface SaveFileResponse {
  ok: boolean
  /** 保存后的磁盘 mtime（Unix 毫秒），作为下一次保存的 base_mtime */
  mtime: number
  size: number
}

/** GET /api/fs/tree 目录树节点（TODO #26 阶段 E）。目录恒带 children（可能为空），文件无 children。 */
export interface FSTreeNode {
  name: string
  /** 绝对路径，必位于会话 WorkDir 内 */
  path: string
  type: 'dir' | 'file'
  size?: number
  /** Unix 毫秒 */
  mtime: number
  children?: FSTreeNode[]
}

export interface FSTreeResponse {
  /** 规范化（Clean+Abs）后的工作区根 */
  root: string
  /** 节点数超上限被截断时为 true */
  truncated: boolean
  tree: FSTreeNode
}

/**
 * 会话工作区整棵目录树（TODO #26 阶段 E 树干）。
 * 无工作区/会话无效返回 404 {code:"no_workspace"|"no_session"}，调用方降级为 WriteFile 列表。
 * depth 默认 8、上限 12；节点总数硬上限 5000（truncated=true）。
 */
export function getFSTree(sessionID: string, depth?: number): Promise<FSTreeResponse> {
  const q = depth ? `&depth=${depth}` : ''
  return fetchJson(`/fs/tree?session=${encodeURIComponent(sessionID)}${q}`)
}

export function listFiles(sessionID: string): Promise<FilesResponse> {
  return fetchJson(`/files?session=${encodeURIComponent(sessionID)}`)
}

/**
 * 读取文件内容。offset/limit（TODO #26 阶段 D 分段读取）：offset 为字节偏移，
 * limit 单次上限 1MB（默认 300KB）；截断文件用返回的 next_offset 续拉追加。
 */
export function getFileContent(
  path: string,
  opts?: { offset?: number; limit?: number },
): Promise<FileContentResponse> {
  let q = `path=${encodeURIComponent(path)}`
  if (opts?.offset !== undefined) q += `&offset=${opts.offset}`
  if (opts?.limit !== undefined) q += `&limit=${opts.limit}`
  return fetchJson(`/files/content?${q}`)
}

/**
 * 在线编辑保存（TODO #26 阶段 G）。path 必须在会话工作区内（否则 404）；
 * 单文件上限 5MB（413）。传 baseMtime 时后端做冲突检测：磁盘被外部改过返回
 * 409 {code:"conflict", current_mtime}，由调用方弹「覆盖/放弃」。
 * force（覆盖）时不带 base_mtime 重发。
 */
export function saveFileContent(
  path: string,
  content: string,
  baseMtime?: number,
): Promise<SaveFileResponse> {
  return fetchJson(`/files/content`, {
    method: 'PUT',
    body: JSON.stringify({ path, content, base_mtime: baseMtime }),
  })
}

/**
 * 在系统文件管理器中定位文件（TODO #26 阶段 C「在文件夹中显示」）。
 * path 限会话 WriteFile 产物或工作区内（否则 404）；无桌面/拉起失败返回
 * 5xx（501/500）+ 错误信息，调用方 ElMessage 提示。
 */
export function revealFile(path: string): Promise<{ ok: boolean }> {
  return fetchJson(`/files/reveal?path=${encodeURIComponent(path)}`)
}
