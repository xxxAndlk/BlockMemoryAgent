import { inject, type ComputedRef, type InjectionKey, type Ref } from 'vue'

/**
 * 文件打开器（TODO #26 阶段 A/C）：消息卡片 / Markdown 本地文件链接 →
 * 右侧文件 tab 的树中定位并打开预览。由 session/index.vue provide，
 * 消费方（ArtifactCard / MarkdownRenderer / AssistantTurn / FilePreview）inject。
 *
 * FileViewer 全屏层落地后（阶段 B），openInTree 仅换目标组件，接口形状保持不变。
 */
export interface FileOpener {
  /** 在右侧文件 tab 中定位并打开该文件：切 rightTab=files、展开侧栏，
   * 树展开祖先链并新建/激活预览 tab（面板形态，文本编辑态留在面板内）。
   * path 允许工作区相对或绝对（相对路径按当前会话 work_dir 解析，解析不出时前端提示）。 */
  openInTree: (path: string) => void
  /**
   * 打开全局全屏 FileViewer（TODO #26 阶段 B）：ArtifactCard 预览按钮、
   * Markdown 本地文件链接、FilePreview「全屏」按钮统一入口。
   * siblings 仅图片画廊用（同目录图片绝对路径列表，可左右切换）。
   */
  openInViewer: (path: string, opts?: { siblings?: string[]; size?: number }) => void
  /**
   * 把工作区相对路径解析为绝对路径（供 raw/reveal 端点用）；
   * 已是绝对路径原样返回；无工作区且无法解析时返回 null。
   */
  resolveAbsolute: (path: string) => string | null
  /** 当前会话工作区根（绝对路径）；无工作区为空串。 */
  workDir: ComputedRef<string>
  /**
   * 待定位请求（FilePreview 消费，seq 保证同路径连续触发也能触发 watch）。
   * FilePreview 处理后置回 null。
   */
  locateRequest: Ref<{ path: string; seq: number } | null>
}

export const FILE_OPENER_KEY: InjectionKey<FileOpener> = Symbol('bma-file-opener')

/** 注入文件打开器；提供者不存在（组件被复用到会话外）时返回 null，调用方自行降级。 */
export function useFileOpener(): FileOpener | null {
  return inject(FILE_OPENER_KEY, null)
}

/** Windows 盘符绝对路径（C:\... / C:/...）或 POSIX 绝对路径（/...）。 */
export function isAbsolutePath(p: string): boolean {
  return /^([a-zA-Z]:[\\/]|\/)/.test(p)
}

/** 纯函数版路径解析（FilePreview 内部与 provide 实现共用同一口径）。 */
export function resolveWorkspacePath(path: string, workDir: string): string | null {
  const t = (path || '').trim()
  if (!t) return null
  if (isAbsolutePath(t)) return t
  const wd = (workDir || '').trim()
  if (!wd) return null
  return `${wd.replace(/[\\/]+$/, '')}\\${t.replace(/\//g, '\\')}`
}
