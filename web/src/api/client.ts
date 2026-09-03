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
    if (!res.ok) throw new APIError(res.status, res.statusText)
    return res.json() as Promise<T>
  } finally {
    clearTimeout(timeoutId)
  }
}
