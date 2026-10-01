// 文件类型分类器（TODO #26 阶段 B FileViewer）：按扩展名/文件名判定视图类型、
// 徽标文案与 highlight.js 语言。与 FilePreview 面板的 fileMeta 口径保持一致
//（图片/markdown/json/代码/文本的扩展名集合同源），并补全全屏层新增的
// pdf/video/audio/csv/tsv/xlsx/ipynb/html 类型。

export type ViewerKind =
  | 'image' | 'pdf' | 'video' | 'audio' | 'markdown' | 'code' | 'text'
  | 'json' | 'csv' | 'xlsx' | 'ipynb' | 'html' | 'binary'

export interface ViewerMeta {
  kind: ViewerKind
  /** 工具条上的类型徽标文案 */
  label: string
  /** 小写扩展名（无扩展名为 ''） */
  ext: string
  /** highlight.js 语言名；undefined = 不高亮（纯文本） */
  hlLang?: string
}

export const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'ico', 'bmp', 'avif'])
const PDF_EXTS = new Set(['pdf'])
const VIDEO_EXTS = new Set(['mp4', 'webm', 'mov', 'mkv', 'avi', 'm4v'])
const AUDIO_EXTS = new Set(['mp3', 'wav', 'ogg', 'flac', 'm4a', 'opus', 'aac'])
const MARKDOWN_EXTS = new Set(['md', 'markdown'])
const CSV_EXTS = new Set(['csv', 'tsv'])
const SHEET_EXTS = new Set(['xlsx', 'xls'])
const HTML_EXTS = new Set(['html', 'htm'])
const TEXT_EXTS = new Set(['txt', 'log', 'env'])

/** 代码扩展名 → highlight.js 语言（与 FilePreview 的 CODE_EXTS 同源）。 */
export const CODE_EXTS: Record<string, string> = {
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  ts: 'typescript', tsx: 'typescript', mts: 'typescript',
  py: 'python', go: 'go', java: 'java',
  c: 'cpp', cc: 'cpp', cpp: 'cpp', h: 'cpp', hpp: 'cpp', cs: 'cpp', rs: 'rust',
  html: 'xml', htm: 'xml', vue: 'xml', xml: 'xml', // hljs 无官方 vue 语法，SFC 用 xml 兜底
  css: 'css', scss: 'scss', less: 'scss',
  sh: 'bash', bash: 'bash', zsh: 'bash',
  yml: 'yaml', yaml: 'yaml',
  sql: 'sql', ini: 'ini', toml: 'ini', // hljs 无官方 toml 语法，ini 结构最接近作兜底
  lua: 'lua', diff: 'diff', patch: 'diff',
}

/** 已注册语言（HLJS_SUPPORTED 的子集口径在 hljs.ts，避免循环依赖这里用同类判断）。 */
const HLJS_READY = new Set(['javascript', 'typescript', 'python', 'go', 'java', 'cpp',
  'xml', 'css', 'scss', 'bash', 'yaml', 'sql', 'ini', 'diff', 'dockerfile', 'lua', 'markdown', 'json'])

export function classifyFile(name: string): ViewerMeta {
  const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : ''
  const lowerName = name.toLowerCase()
  // Dockerfile 无扩展名（或以 .dockerfile 结尾）：单独走 dockerfile 高亮
  if (lowerName === 'dockerfile' || ext === 'dockerfile') {
    return { kind: 'code', label: 'Dockerfile', ext, hlLang: 'dockerfile' }
  }
  if (IMAGE_EXTS.has(ext)) return { kind: 'image', label: '图片', ext }
  if (PDF_EXTS.has(ext)) return { kind: 'pdf', label: 'PDF', ext }
  if (VIDEO_EXTS.has(ext)) return { kind: 'video', label: '视频', ext }
  if (AUDIO_EXTS.has(ext)) return { kind: 'audio', label: '音频', ext }
  if (MARKDOWN_EXTS.has(ext)) return { kind: 'markdown', label: 'Markdown', ext }
  if (ext === 'ipynb') return { kind: 'ipynb', label: 'Notebook', ext }
  if (ext === 'json') return { kind: 'json', label: 'JSON', ext, hlLang: 'json' }
  if (CSV_EXTS.has(ext)) return { kind: 'csv', label: ext === 'tsv' ? 'TSV' : 'CSV', ext }
  if (SHEET_EXTS.has(ext)) return { kind: 'xlsx', label: 'Excel', ext }
  if (HTML_EXTS.has(ext)) return { kind: 'html', label: 'HTML', ext }
  if (ext in CODE_EXTS) {
    const lang = CODE_EXTS[ext]
    return {
      kind: 'code', label: ext.toUpperCase(), ext,
      hlLang: HLJS_READY.has(lang) ? lang : undefined,
    }
  }
  if (TEXT_EXTS.has(ext) || ext === '') return { kind: 'text', label: '文本', ext }
  return { kind: 'binary', label: ext ? ext.toUpperCase() : '文件', ext }
}

/** 该类型是否走文本加载链路（content 端点分段拉取）。 */
export function isTextualKind(kind: ViewerKind): boolean {
  return ['markdown', 'code', 'text', 'json', 'csv', 'ipynb'].includes(kind)
}

/**
 * 引号感知的 CSV/TSV 解析（RFC4180 子集）：支持 "..." 包裹、内嵌分隔符/换行、"" 转义。
 * 行尾允许 \r\n。返回二维字符串数组（已去掉引号）。
 */
export function parseDelimited(text: string, delimiter: string): string[][] {
  const rows: string[][] = []
  let row: string[] = []
  let field = ''
  let inQuotes = false
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (inQuotes) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          field += '"'
          i++
        } else {
          inQuotes = false
        }
      } else {
        field += ch
      }
    } else if (ch === '"' && field === '') {
      inQuotes = true
    } else if (ch === delimiter) {
      row.push(field)
      field = ''
    } else if (ch === '\n') {
      row.push(field)
      rows.push(row)
      row = []
      field = ''
    } else if (ch === '\r') {
      // 跳过，\r\n 的 \r 不入字段
    } else {
      field += ch
    }
  }
  // 收尾：末字段/末行（无换行结尾）
  if (field !== '' || row.length) {
    row.push(field)
    rows.push(row)
  }
  return rows
}
