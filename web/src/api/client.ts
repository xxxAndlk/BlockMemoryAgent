import { APP_CONFIG } from '@/config/app'

export class APIError extends Error {
  status: number
  statusText: string

  constructor(status: number, statusText: string, message?: string) {
    super(message || `${status} ${statusText}`)
    this.status = status
    this.statusText = statusText
  }
}

export async function fetchJson<T>(
  url: string,
  options?: RequestInit & { timeoutMs?: number },
): Promise<T> {
  const controller = new AbortController()
  const timeoutId = setTimeout(() => controller.abort(), options?.timeoutMs ?? APP_CONFIG.apiTimeout)

  try {
    const res = await fetch(`${APP_CONFIG.apiBase}${url}`, {
      headers: { 'Content-Type': 'application/json' },
      signal: options?.signal ?? controller.signal,
      ...options,
    })
    if (!res.ok) {
      // 后端错误分支用 c.String(status, msg) 返回纯文本原因（如"agent 正在执行任务，
      // 发送已禁用（可先中断或终止）"）。此前只带 statusText 上抛，调用方拿到的永远是
      // "409 Conflict"，用户看不到该等/该中断/该去监控页——三态发送框的提示因此失效。
      let detail = ''
      try {
        detail = (await res.text()).trim()
      } catch {
        // 读体失败（连接中断等）时退回 statusText，不让错误路径二次抛错。
      }
      throw new APIError(res.status, res.statusText, detail || undefined)
    }
    return res.json() as Promise<T>
  } finally {
    clearTimeout(timeoutId)
  }
}
