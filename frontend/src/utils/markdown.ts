import { marked } from 'marked'

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

function sanitize(html: string): string {
  return html
    .replace(/\son\w+\s*=\s*"[^"]*"/gi, '')
    .replace(/\son\w+\s*=\s*'[^']*'/gi, '')
    .replace(/\son\w+\s*=\s*[^\s>]+/gi, '')
    .replace(/(href|src)\s*=\s*"javascript:[^"]*"/gi, '$1="#"')
    .replace(/(href|src)\s*=\s*'javascript:[^']*'/gi, '$1="#"')
}

export function renderMd(src: string | undefined | null): string {
  if (!src) return ''
  const html = marked.parse(src, { async: false }) as string
  return sanitize(html)
}

export function esc(s: string | undefined | null): string {
  if (!s) return ''
  const d = document.createElement('div')
  d.textContent = s
  return d.innerHTML
}
