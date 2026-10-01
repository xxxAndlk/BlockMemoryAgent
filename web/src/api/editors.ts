import { fetchJson } from './client'

export interface EditorInfo {
  /** 稳定小写标识（notepad/vscode/trae/...），作为 editor_id 传给 open */
  id: string
  /** 给人看的名字 */
  name: string
  /** 可执行文件绝对路径（菜单里截断展示） */
  exe: string
}

/** 扫码本机已安装编辑器（后端 60s 进程内缓存；每次展开下拉都重新调，装完编辑器无需刷新页面）。 */
export function listEditors(): Promise<EditorInfo[]> {
  return fetchJson<EditorInfo[]>(`/editors`)
}

/** 打开文件：editorId 为空 = 系统默认打开；否则以该编辑器拉起。 */
export function openInEditor(editorId: string | null, path: string): Promise<{ ok: boolean; editor_id: string }> {
  return fetchJson(`/editors/open`, {
    method: 'POST',
    body: JSON.stringify({ editor_id: editorId ?? '', path }),
  })
}
