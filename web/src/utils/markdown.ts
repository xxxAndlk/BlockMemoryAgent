import { marked, Renderer } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({
  gfm: true,
  breaks: true,
})

const renderer = new marked.Renderer()
// 注意：必须用 function + this.parser，不能读模块级 renderer.parser——marked.use()
// 会把自定义渲染器包进新实例，parser 只挂在被 marked 管理的实例上；读模块变量拿到
// undefined，任何裸链接（GFM autolink，如 **http://x**）触发即抛 "parseInline of
// undefined"，Vue 组件渲染整体失败空白（2026-09-11 实证：回复含粗体裸链接时
// 对话框该回合/日志该行全部空白）。
/** 本地文件链接识别（TODO #26 A）：无 scheme 且长得像文件路径（含分隔符或有文件扩展名）。
 *  Windows 盘符路径（C:\...）先于 scheme 判断（"C:" 会被 scheme 正则误吞）。 */
const FILE_LINK_EXTS = new Set([
  'md', 'markdown', 'txt', 'log', 'pdf', 'json', 'csv', 'tsv', 'xlsx', 'xls', 'docx', 'doc',
  'png', 'jpg', 'jpeg', 'gif', 'webp', 'svg', 'ico', 'mp4', 'webm', 'mov', 'mp3', 'wav',
  'html', 'htm', 'xml', 'yaml', 'yml', 'toml', 'ini', 'sql', 'zip',
  'go', 'py', 'js', 'mjs', 'cjs', 'jsx', 'ts', 'mts', 'tsx', 'vue', 'java', 'c', 'cc', 'cpp',
  'h', 'hpp', 'cs', 'rs', 'rb', 'php', 'sh', 'bash', 'css', 'scss', 'less', 'ps1', 'bat',
])

/** href 是本地工作区文件链接时返回原路径串，否则 null。 */
export function localFileLinkPath(href: string | null | undefined): string | null {
  const h = (href || '').trim()
  if (!h || h.startsWith('#')) return null
  if (/^[a-zA-Z]:[\\/]/.test(h)) return h // Windows 绝对路径
  if (/^[a-z][a-z0-9+.-]*:/i.test(h)) return null // http/mailto/javascript 等 scheme
  if (h.startsWith('//')) return null // 协议相对
  const ext = h.includes('.') ? h.split('.').pop()!.toLowerCase().split(/[?#]/)[0] : ''
  if (h.includes('/') || h.includes('\\') || FILE_LINK_EXTS.has(ext)) return h
  return null
}

renderer.link = function (this: Renderer, { href, title, tokens }) {
  const t = title ? ` title="${String(title).replace(/"/g, '&quot;')}"` : ''
  const text = this.parser ? this.parser.parseInline(tokens) : escapePlain(tokens)
  // 本地文件链接（工作区相对/绝对路径）：渲染成 📄 文件入口，点击由容器事件委托
  // 调 fileOpener.openInTree 在文件树中打开（AssistantTurn.onMdAction /
  // MarkdownRenderer 的 click）；外部 http 链接行为不变。
  const localPath = localFileLinkPath(href)
  if (localPath) {
    const p = localPath.replace(/&/g, '&amp;').replace(/"/g, '&quot;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    return `<a href="#" data-bma-file="${p}" class="md-file-link">📄 ${text}</a>`
  }
  const safe = /^https?:\/\//i.test(href || '') ? href : '#'
  return `<a href="${safe}" target="_blank" rel="noopener noreferrer"${t}>${text}</a>`
}

// escapePlain 是 parser 缺失时的兜底：把 link 子 token 原文转义为纯文本。
function escapePlain(tokens: { raw?: string }[] | undefined): string {
  return (tokens || [])
    .map((tk) => (tk?.raw || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'))
    .join('')
}
renderer.html = () => ''
// DeepSeek 风格代码块：语言标签 + 复制/下载头栏。按钮交互由容器事件委托处理
// （AssistantTurn onMdAction：点击后从最近 pre 取 textContent），无需 data 属性携带代码。
renderer.code = ({ text, lang, escaped }) => {
  const body = escaped
    ? text
    : text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
  const label = lang ? String(lang).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;') : '代码'
  return (
    `<div class="md-code">` +
    `<div class="md-code-head"><span class="md-code-lang">${label}</span>` +
    `<span class="md-code-actions">` +
    `<button type="button" class="md-copy">复制</button>` +
    `<button type="button" class="md-download">下载</button>` +
    `</span></div>` +
    `<pre><code>${body}</code></pre></div>`
  )
}
marked.use({ renderer })

// DOMPurify 配置：白名单严格化，禁止 data: URI、所有脚本事件、危险标签。
// 原 sanitize() 用正则剥离 on*/javascript:，易被 data: URI / style / iframe 绕过（F1 修复）。
const purifyConfig = {
  ALLOWED_TAGS: [
    'a', 'b', 'i', 'em', 'strong', 'code', 'pre', 'blockquote', 'ul', 'ol', 'li',
    'p', 'br', 'hr', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'span', 'div', 'img',
    'table', 'thead', 'tbody', 'tr', 'th', 'td', 'del', 'ins', 'sub', 'sup', 'kbd',
    // 仅代码块渲染器输出 button（原始 HTML 已被 renderer.html 剥除），供复制/下载
    'button',
  ],
  ALLOWED_ATTR: ['href', 'src', 'alt', 'title', 'class', 'target', 'rel', 'colspan', 'rowspan',
    // 本地文件链接入口（TODO #26 A）：点击由容器事件委托调 fileOpener.openInTree
    'data-bma-file'],
  ALLOW_DATA_ATTR: false,
  FORBID_TAGS: ['script', 'iframe', 'object', 'embed', 'form', 'input', 'style', 'link', 'meta', 'base'],
  FORBID_ATTR: ['style', 'onerror', 'onload', 'onclick', 'onmouseover'],
  ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto):|[^:/?#]+(?:[/?#]|$))/i,
}

export function renderMd(src: string | undefined | null): string {
  if (!src) return ''
  let html: string
  try {
    html = marked.parse(src, { async: false }) as string
  } catch (e) {
    // 渲染器异常兜底：宁可降级纯文本，也不能让一个 markdown 报错把整个组件渲染带崩
    //（Vue 渲染函数抛错会整块空白——2026-09-11 link renderer 事故）。
    console.error('[markdown] parse failed, fallback to plain text:', e)
    html = `<pre class="md-fallback">${escapePlainText(src)}</pre>`
  }
  return DOMPurify.sanitize(html, purifyConfig) as unknown as string
}

// escapePlainText 把任意文本转义为可安全入 HTML 的纯文本。
function escapePlainText(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
}

export function esc(s: string | undefined | null): string {
  if (!s) return ''
  const d = document.createElement('div')
  d.textContent = s
  return d.innerHTML
}
