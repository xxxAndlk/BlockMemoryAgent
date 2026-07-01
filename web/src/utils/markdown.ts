import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({
  gfm: true,
  breaks: true,
})

const renderer = new marked.Renderer()
renderer.link = ({ href, title, tokens }) => {
  const safe = /^https?:\/\//i.test(href || '') ? href : '#'
  const t = title ? ` title="${String(title).replace(/"/g, '&quot;')}"` : ''
  const text = (renderer.parser.parseInline(tokens) as string)
  return `<a href="${safe}" target="_blank" rel="noopener noreferrer"${t}>${text}</a>`
}
renderer.html = () => ''
marked.use({ renderer })

// DOMPurify 配置：白名单严格化，禁止 data: URI、所有脚本事件、危险标签。
// 原 sanitize() 用正则剥离 on*/javascript:，易被 data: URI / style / iframe 绕过（F1 修复）。
const purifyConfig = {
  ALLOWED_TAGS: [
    'a', 'b', 'i', 'em', 'strong', 'code', 'pre', 'blockquote', 'ul', 'ol', 'li',
    'p', 'br', 'hr', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'span', 'div', 'img',
    'table', 'thead', 'tbody', 'tr', 'th', 'td', 'del', 'ins', 'sub', 'sup', 'kbd',
  ],
  ALLOWED_ATTR: ['href', 'src', 'alt', 'title', 'class', 'target', 'rel', 'colspan', 'rowspan'],
  ALLOW_DATA_ATTR: false,
  FORBID_TAGS: ['script', 'iframe', 'object', 'embed', 'form', 'input', 'style', 'link', 'meta', 'base'],
  FORBID_ATTR: ['style', 'onerror', 'onload', 'onclick', 'onmouseover'],
  ALLOWED_URI_REGEXP: /^(?:(?:https?|mailto):|[^:/?#]+(?:[/?#]|$))/i,
}

export function renderMd(src: string | undefined | null): string {
  if (!src) return ''
  const html = marked.parse(src, { async: false }) as string
  return DOMPurify.sanitize(html, purifyConfig) as unknown as string
}

export function esc(s: string | undefined | null): string {
  if (!s) return ''
  const d = document.createElement('div')
  d.textContent = s
  return d.innerHTML
}
