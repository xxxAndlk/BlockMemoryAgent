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

/**
 * 修改某个会话的工作目录（每会话目录，落库即时保存）。
 * `workDir` 传空串 = 清除本会话目录、回落进程默认目录。
 * 目录在每回合开始时读取，因此下一回合生效（进行中的工具调用仍按旧目录解析）。
 */
export function setSessionWorkDir(
  id: string,
  workDir: string,
): Promise<{ session_id: string; work_dir: string }> {
  return fetchJson(`/sessions/${id}/workdir`, {
    method: 'POST',
    body: JSON.stringify({ work_dir: workDir }),
  })
}

/** 硬删除单个会话（不可恢复）：运行中先终止，随后物理删除历史/事件/日志等全部数据。 */
export function deleteSession(id: string): Promise<{ session_id: string; status: string }> {
  return fetchJson(`/sessions/${id}`, { method: 'DELETE' })
}

export interface BatchDeleteResult {
  deleted: string[]
  errors: { id: string; error: string }[]
}

/** 批量硬删除会话（单次上限 200）；逐条独立处理，返回成功与失败清单。 */
export function deleteSessions(ids: string[]): Promise<BatchDeleteResult> {
  return fetchJson('/sessions/delete', { method: 'POST', body: JSON.stringify({ ids }) })
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

// ---- 编排页：单 Agent 对话与用户直连（TODO 第12项）----

/** 单条 Agent 消息（与后端 reactMessageWire/entriesToWire 线型对齐）。 */
export interface AgentMessageItem {
  seq: number
  /** 热层条目带 RFC3339 时间戳；PG 回退路径无 at（省略）。 */
  at?: string
  role: 'user' | 'assistant' | 'tool' | string
  content: string
  reasoning?: string
  tool_calls?: { id: string; name: string; input: unknown }[]
  tool_call_id?: string
}

/** 一条 mailbox 留痕（后端 type=mailbox 事件，双方各一行）。 */
export interface AgentMailItem {
  /** 留痕时间（RFC3339）。 */
  at: string
  /** 发送方（user/dispatcher/子 Agent 实例 ID）。 */
  from: string
  /** 接收方。 */
  to: string
  /** 邮件类型（milestone/request/info/escalate/reply）。 */
  msg_type: string
  /** 主题（后端 content 的首行）。 */
  subject: string
  /** 正文（后端 content 去掉首行主题后的剩余部分）。 */
  body: string
}

export interface AgentConversation {
  agent_id: string
  total: number
  messages: AgentMessageItem[]
  mails: AgentMailItem[]
  /** 头部还有更早消息（before_seq 翻页用；本次取满 limit 即视为可能还有）。 */
  has_more: boolean
}

/** 后端 mailbox 留痕线型（字段口径见 store.QueryAgentMailboxTrace）。 */
interface WireMailItem {
  role?: string
  content?: string
  tool_name?: string
  input?: string
  occurred?: string
}

/** 把后端 mailbox 留痕转成前端线型：content = subject\nbody，此处拆回首行/余下。 */
function toMailItem(m: WireMailItem): AgentMailItem {
  const raw = m.content ?? ''
  const nl = raw.indexOf('\n')
  return {
    at: m.occurred ?? '',
    from: m.role ?? '',
    to: m.input ?? '',
    msg_type: m.tool_name ?? '',
    subject: nl >= 0 ? raw.slice(0, nl) : raw,
    body: nl >= 0 ? raw.slice(nl + 1) : '',
  }
}

/**
 * 取单个 Agent 的完整对话（编排页对话面板数据源）。
 * 滚动窗口三态：`afterSeq` 增量轮询 / `beforeSeq` 上翻 / 都不传取尾部。
 * `aid='meta'` 后端映射为会话主 Agent（前端正常不传 meta）。
 */
export async function getAgentMessages(
  id: string,
  aid: string,
  opts?: { beforeSeq?: number; afterSeq?: number; limit?: number },
): Promise<AgentConversation> {
  const qs = new URLSearchParams()
  // 仅在 >0 时传窗口参数：seq=0 是合法最小值，但后端把 before_seq<=0 当"取尾部"，
  // 传 0 会退化成尾部窗口（上翻时把同一批消息重复前插）。
  if (opts?.beforeSeq && opts.beforeSeq > 0) qs.set('before_seq', String(opts.beforeSeq))
  if (opts?.afterSeq && opts.afterSeq >= 0) qs.set('after_seq', String(opts.afterSeq))
  const limit = opts?.limit ?? 100
  qs.set('limit', String(limit))
  const res = await fetchJson<{
    session_id: string
    agent_id: string
    messages?: AgentMessageItem[]
    mails?: WireMailItem[]
  }>(`/sessions/${id}/agents/${encodeURIComponent(aid)}/messages?${qs.toString()}`)
  const messages = res.messages ?? []
  const mails = (res.mails ?? []).map(toMailItem)
  // 后端不返 total/has_more：取满 limit 视为"头部可能还有更早消息"（上翻按钮据此显示）。
  return {
    agent_id: res.agent_id ?? aid,
    total: messages.length,
    messages,
    mails,
    has_more: !opts?.afterSeq && messages.length >= limit,
  }
}

/**
 * 用户直连发送（编排页对话面板发送框）。
 * 后端状态机：等子返回→注入唤醒；终态→复活重跑；执行中/不可直连→409（抛 APIError）。
 */
export function sendAgentMessage(id: string, aid: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/agents/${encodeURIComponent(aid)}/message`, {
    method: 'POST',
    body: JSON.stringify({ content }),
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

/** 批量澄清统一提交（任务 140）：answers 与待澄清 questions 按下标对齐，全部作答后一次性提交。 */
export function clarifySessionBatch(id: string, answers: string[]): Promise<void> {
  return fetchJson(`/sessions/${id}/clarify`, {
    method: 'POST',
    body: JSON.stringify({ answers }),
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
