import type { Session, SessionEvent, AgentNode, TaskBoardData, WireImage, TrustMode } from '@/types'
import { fetchJson } from './client'
import { APP_CONFIG } from '@/config/app'

export function listSessions(): Promise<Session[]> {
  return fetchJson('/sessions')
}

export function createSession(goal: string, images?: WireImage[], workDir?: string): Promise<Session> {
  const body: Record<string, unknown> = { goal }
  if (images?.length) body.images = images
  if (workDir) body.work_dir = workDir
  return fetchJson('/sessions', { method: 'POST', body: JSON.stringify(body) })
}

export function getSession(id: string): Promise<Session> {
  return fetchJson(`/sessions/${id}`)
}

export function getSessionBoard(id: string): Promise<{ session_id: string; board: TaskBoardData }> {
  return fetchJson(`/sessions/${id}/board`)
}

export function getSessionAgents(
  id: string
): Promise<{ session_id: string; agents: AgentNode[]; tree: AgentNode[] }> {
  return fetchJson(`/sessions/${id}/agents`)
}

export function sendMessage(id: string, content: string, images?: WireImage[]): Promise<void> {
  return fetchJson(`/sessions/${id}/message`, {
    method: 'POST',
    body: JSON.stringify(images?.length ? { content, images } : { content }),
  })
}

export function cancelSession(id: string): Promise<{ session_id: string; status: string }> {
  return fetchJson(`/sessions/${id}/cancel`, { method: 'POST' })
}

/** 软停止（TODO #37）：停止当前子任务、销毁倒计时窗口内可续跑。 */
export function stopSession(id: string): Promise<{ session_id: string; status: string }> {
  return fetchJson(`/sessions/${id}/stop`, { method: 'POST' })
}

/** 抢占中断：停止当前 LLM 调用并把内容作为新指令注入。 */
export function interruptSession(id: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/interrupt`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

/** 切换会话信任模式（TODO 第10⑥ 三级信任，对标 Codex）：POST /sessions/{id}/trust-mode，下一工具调用生效。 */
export function setTrustMode(id: string, mode: TrustMode): Promise<{ session_id: string; trust_mode: string }> {
  return fetchJson(`/sessions/${id}/trust-mode`, {
    method: 'POST',
    body: JSON.stringify({ mode }),
  })
}

/** 运行中会话任务入队：不打断当前执行，当前轮结束后依次消费。 */
export function enqueueSession(id: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/enqueue`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

export function clarifySession(id: string, answer: string): Promise<void> {
  return fetchJson(`/sessions/${id}/clarify`, {
    method: 'POST',
    body: JSON.stringify({ answer }),
  })
}

/** SSE live 帧：模型实时汇报/思考文本（仅运行中，变化才推）。 */
export interface LiveTextFrame {
  type: 'live'
  streaming_text?: string
  thinking_text?: string
}

export function streamSession(
  id: string,
  onEvent: (ev: SessionEvent) => void,
  onDone?: (finalStatus?: string) => void,
  onError?: (err: Error) => void,
  onLive?: (d: LiveTextFrame) => void
): () => void {
  let closed = false
  let attempt = 0
  let es: EventSource | null = null
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null

  const connect = () => {
    if (closed) return
    es = new EventSource(`${APP_CONFIG.apiBase}/sessions/${id}/stream`)
    es.onopen = () => {
      attempt = 0
    }
    es.onmessage = (e) => {
      try {
        const d = JSON.parse(e.data)
        if (d.type === 'done') {
          es?.close()
          onDone?.(d.status)
          return
        }
        if (d.type === 'live') {
          onLive?.(d as LiveTextFrame)
          return
        }
        onEvent(d as SessionEvent)
      } catch (err) {
        onError?.(err as Error)
      }
    }
    es.onerror = () => {
      es?.close()
      es = null
      if (closed) return
      if (attempt >= APP_CONFIG.sseRetry) {
        onError?.(new Error('SSE 重连失败，已超过最大重试次数'))
        return
      }
      const delay = Math.min(1000 * Math.pow(2, attempt), APP_CONFIG.sseRetryMaxDelayMs)
      attempt++
      reconnectTimer = setTimeout(connect, delay)
    }
  }
  connect()

  return () => {
    closed = true
    if (reconnectTimer) clearTimeout(reconnectTimer)
    es?.close()
  }
}

export interface SessionLog {
  id: number
  session_id: string
  agent: string
  level: string
  phase: string
  message: string
  prompt?: string
  response?: string
  input_tokens: number
  output_tokens: number
  model: string
  latency_ms: number
  created_at: string
  meta?: Record<string, unknown>
}

export interface SessionLogsResponse {
  session_id: string
  logs: SessionLog[]
  count: number
}

export function getSessionLogs(
  id: string,
  params?: { agent?: string; level?: string; limit?: number; offset?: number }
): Promise<SessionLogsResponse> {
  const qs = new URLSearchParams()
  if (params?.agent) qs.set('agent', params.agent)
  if (params?.level) qs.set('level', params.level)
  if (params?.limit !== undefined) qs.set('limit', String(params.limit))
  if (params?.offset !== undefined) qs.set('offset', String(params.offset))
  const q = qs.toString() ? `?${qs.toString()}` : ''
  return fetchJson(`/sessions/${id}/logs${q}`)
}

export interface WatchdogDecision {
  agent_id: string
  tokens: number
  level: string
  reason: string
  suggested: string
  occurred_at: string
}

export interface SessionWatchdogResponse {
  session_id: string
  decisions: WatchdogDecision[]
}

export function getSessionWatchdog(id: string): Promise<SessionWatchdogResponse> {
  return fetchJson(`/sessions/${id}/watchdog`)
}

export interface MailboxMessage {
  id: string
  from: string
  to: string
  type: string
  subject: string
  body: string
  priority: number
  status: string
  created_at: string
  broadcast?: boolean
}

export interface SessionMailboxResponse {
  session_id: string
  messages: MailboxMessage[]
}

export function getSessionMailbox(id: string): Promise<SessionMailboxResponse> {
  return fetchJson(`/sessions/${id}/mailbox`)
}
