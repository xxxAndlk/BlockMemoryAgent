import { APP_CONFIG } from '@/config/app'

export class APIError extends Error {
  status: number
  statusText: string
  /** 服务端返回的 JSON 错误体（若有）；纯文本错误体为 undefined，正文在 message */
  data?: unknown

  constructor(status: number, statusText: string, message?: string, data?: unknown) {
    super(message || `${status} ${statusText}`)
    this.status = status
    this.statusText = statusText
    this.data = data
  }
}

/**
 * 网络层失败：请求根本没到达服务端，或响应没回来（后端重启/连接中断/本地超时）。
 * 与 APIError（服务端明确返回 4xx/5xx 业务原因）分开——调用方据此决定"重试"还是"报错"：
 * 提交类动作（澄清答复等）在网络层失败时可以安全重试，业务拒绝重试也没用。
 * 此前这类失败直接把浏览器的 "Failed to fetch" 抛给用户，既看不懂也无从补救。
 */
export class NetworkError extends Error {
  /** timeout=本地超时（无响应）；offline=连接被拒绝/中断（服务未监听、正在重启） */
  reason: 'timeout' | 'offline'

  constructor(reason: 'timeout' | 'offline', message: string) {
    super(message)
    this.name = 'NetworkError'
    this.reason = reason
  }
}

export async function fetchJson<T>(
  url: string,
  options?: RequestInit & { timeoutMs?: number },
): Promise<T> {
  const controller = new AbortController()
  const timeoutMs = options?.timeoutMs ?? APP_CONFIG.apiTimeout
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs)

  try {
    let res: Response
    try {
      res = await fetch(`${APP_CONFIG.apiBase}${url}`, {
        headers: { 'Content-Type': 'application/json' },
        signal: options?.signal ?? controller.signal,
        ...options,
      })
    } catch (e) {
      // fetch 只在网络层失败时抛错：超时（我们自己的 AbortController）或连接层错误
      //（TypeError: Failed to fetch / ERR_CONNECTION_REFUSED）。翻译成可读原因再上抛。
      if (e instanceof DOMException && e.name === 'AbortError') {
        throw new NetworkError('timeout', `请求超时（${Math.round(timeoutMs / 1000)} 秒无响应）`)
      }
      throw new NetworkError('offline', `无法连接后端服务（${e instanceof Error ? e.message : String(e)}）`)
    }
    if (!res.ok) {
      // 后端错误分支用 c.String(status, msg) 返回纯文本原因（如"agent 正在执行任务，
      // 发送已禁用（可先中断或终止）"）。此前只带 statusText 上抛，调用方拿到的永远是
      // "409 Conflict"，用户看不到该等/该中断/该去监控页——三态发送框的提示因此失效。
      // JSON 错误体（如 PUT /api/files/content 的 409 {code, current_mtime}）整体挂到
      // error.data，调用方按 code 分支（文件保存冲突弹覆盖/放弃）。
      let detail = ''
      let data: unknown
      let raw = ''
      try {
        raw = (await res.text()).trim()
      } catch {
        // 读体失败（连接中断等）时退回 statusText，不让错误路径二次抛错。
      }
      if (raw) {
        try {
          data = JSON.parse(raw)
          const m = (data as { message?: unknown }).message
          detail = typeof m === 'string' ? m : raw
        } catch {
          detail = raw
        }
      }
      throw new APIError(res.status, res.statusText, detail || undefined, data)
    }
    return res.json() as Promise<T>
  } finally {
    clearTimeout(timeoutId)
  }
}
