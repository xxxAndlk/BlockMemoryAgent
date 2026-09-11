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
renderer.link = function (this: Renderer, { href, title, tokens }) {
  const safe = /^https?:\/\//i.test(href || '') ? href : '#'
  const t = title ? ` title="${String(title).replace(/"/g, '&quot;')}"` : ''
  const text = this.parser ? this.parser.parseInline(tokens) : escapePlain(tokens)
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
  ALLOWED_ATTR: ['href', 'src', 'alt', 'title', 'class', 'target', 'rel', 'colspan', 'rowspan'],
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
