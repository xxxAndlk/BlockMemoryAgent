import type { Session, SessionEvent, SessionSummary, AgentNode, TaskBoardData, WireImage, TrustMode, SessionGear, SessionThinking } from '@/types'
import { fetchJson } from './client'
import { APP_CONFIG } from '@/config/app'
import { TERMINAL_STATUSES } from '@/utils/notifications'

/** 非终态 done（旧后端把挂起态当会话结束关连接）的续连间隔：只作兜底轮询，新后端不会走到。 */
const SUSPENDED_DONE_RETRY_MS = 1500

/**
 * 会话列表（摘要线型，不含 events/messages）。
 * `limit` 省略时后端按 200 条截断——首页要对全量会话做计数/分页，
 * 必须显式传大 limit（上限 1000），否则删掉的会话会被更旧的行悄悄顶上来，数字纹丝不动。
 */
export function listSessions(limit?: number): Promise<SessionSummary[]> {
  return fetchJson(`/sessions${limit ? `?limit=${limit}` : ''}`)
}

export function createSession(goal: string, images?: WireImage[], workDir?: string, gear?: SessionGear, thinking?: SessionThinking): Promise<Session> {
  const body: Record<string, unknown> = { goal }
  if (images?.length) body.images = images
  if (workDir) body.work_dir = workDir
  // 创建时选档（TODO #14）：仅显式选择时携带；省略由后端取 config 默认档（daily）。
  if (gear) body.gear = gear
  // 创建时选思考强度（2026-09-16）：空串 = 跟随角色默认，省略字段。
  if (thinking) body.thinking = thinking
  return fetchJson('/sessions', { method: 'POST', body: JSON.stringify(body) })
}

export function getSession(id: string): Promise<Session> {
  return fetchJson(`/sessions/${id}`)
}

/**
 * 工作区文件 URL（对话栏媒体卡片 / HTML 预览 iframe 的 src）。
 *
 * 走 path 型路由而不是查询参数：HTML 产物里的**相对引用**（pages/index.html 里的
 * assets/x.png）会以该 URL 为基准解析，天然可用。逐段 encodeURIComponent 保留斜杠分隔。
 */
export function workspaceUrl(sessionId: string, path: string): string {
  const encoded = path
    .replace(/\\/g, '/')
    .split('/')
    .filter((s) => s.length > 0)
    .map((s) => encodeURIComponent(s))
    .join('/')
  return `${APP_CONFIG.apiBase}/sessions/${encodeURIComponent(sessionId)}/workspace/${encoded}`
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
  /** 热层最大 seq（-1=热层为空）：兜底的重置判据，小于前端游标即需整表重载。 */
  hotMaxSeq: number
  /** 对话序号（复活重跑递增）：权威的重置判据——变了就整表重载，与 seq 竞态无关。 */
  runId: number
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
 * 取会话级邮件留痕（全部 Agent 的 mailbox 收发，一行一封、收发双方重复行已去重）。
 * 主对话栏用：把"上级 ↔ 下级"的往来显示出来（外发此前在界面上完全不可见）。
 */
export async function getSessionMailboxTrace(id: string, limit = 200): Promise<AgentMailItem[]> {
  const res = await fetchJson<{ mails?: WireMailItem[] }>(
    `/sessions/${id}/mailbox-trace?limit=${limit}`,
  )
  return (res.mails ?? []).map(toMailItem)
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
    hot_max_seq?: number
    run_id?: number
  }>(`/sessions/${id}/agents/${encodeURIComponent(aid)}/messages?${qs.toString()}`)
  const messages = res.messages ?? []
  const mails = (res.mails ?? []).map(toMailItem)
  // 后端不返 total/has_more：取满 limit 视为"头部可能还有更早消息"（上翻按钮据此显示）。
  return {
    agent_id: res.agent_id ?? aid,
    total: messages.length,
    messages,
    mails,
    // 重置判据：runId 权威（复活重跑递增，与 seq 竞态无关），hotMaxSeq 兜底。
    hotMaxSeq: res.hot_max_seq ?? -1,
    runId: res.run_id ?? 0,
    has_more: !opts?.afterSeq && messages.length >= limit,
  }
}

/**
 * 用户直连发送（编排页对话面板发送框）。
 * 后端状态机：等子返回→注入唤醒；执行中→邮箱排队（queued=true，P0-2 steering）；
 * 终态→复活重跑；不可直连（paused/idle/meta）→409（抛 APIError）。
 */
export function sendAgentMessage(id: string, aid: string, content: string): Promise<{ ok: boolean; queued: boolean }> {
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

/** 切换会话执行档位（TODO #14 三档全手动）：POST /sessions/{id}/gear，原子即时生效；
 *  手动切档允许任意向（升档只升不降仅约束 escalate 发起侧）；在飞子 Agent 不强杀，下轮按新档选角色。 */
export function setSessionGear(id: string, gear: SessionGear): Promise<{ session_id: string; gear: string }> {
  return fetchJson(`/sessions/${id}/gear`, {
    method: 'POST',
    body: JSON.stringify({ gear }),
  })
}

/** 切换会话级思考强度（2026-09-16）：POST /sessions/{id}/thinking，原子即时生效——
 *  provider 每次 LLM 调用实时读取，下一次调用即用新档（热，免重启）；空串 = 跟随角色默认。
 *  只影响本会话顶层 Agent，在飞子 Agent 不受影响。 */
export function setSessionThinking(id: string, thinking: SessionThinking): Promise<{ session_id: string; thinking: string }> {
  return fetchJson(`/sessions/${id}/thinking`, {
    method: 'POST',
    body: JSON.stringify({ thinking }),
  })
}

/** 运行中会话任务入队：不打断当前执行，当前轮结束后依次消费。 */
export function enqueueSession(id: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/enqueue`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

/** 澄清答复提交超时：用户主动动作不能按默认 10s 静默失败（后端重启/卡顿时需要更宽的
 *  窗口，配合 chat/utils/clarifySubmit 的重试一起用）。 */
const CLARIFY_TIMEOUT_MS = 30000

export function clarifySession(id: string, answer: string): Promise<void> {
  return fetchJson(`/sessions/${id}/clarify`, {
    method: 'POST',
    body: JSON.stringify({ answer }),
    timeoutMs: CLARIFY_TIMEOUT_MS,
  })
}

/** 批量澄清统一提交（任务 140）：answers 与待澄清 questions 按下标对齐，全部作答后一次性提交。 */
export function clarifySessionBatch(id: string, answers: string[]): Promise<void> {
  return fetchJson(`/sessions/${id}/clarify`, {
    method: 'POST',
    body: JSON.stringify({ answers }),
    timeoutMs: CLARIFY_TIMEOUT_MS,
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
  let online = false // 上次 onopen 是否成功（区分首次连接与断线重连）

  const connect = () => {
    if (closed) return
    es?.close() // 防重复流：重连触发点可能撞上仍存活的旧连接（先关再开）
    es = new EventSource(`${APP_CONFIG.apiBase}/sessions/${id}/stream`)
    es.onopen = () => {
      if (online) return
      online = true
      attempt = 0
    }
    es.onmessage = (e) => {
      try {
        const d = JSON.parse(e.data)
        if (d.type === 'done') {
          // 非终态 done = 旧后端把「挂起等子/暂停于子」误当会话结束（推完 done 就关连接，
          // 2026-09-25 实证：对话栏从挂起那刻起永久冻在「挂起等待子」、最终答复不渲染）。
          // 会话还在跑，不能收口（不停面板定时器、不跑对账），改为续连补全量快照。
          if (d.status && !TERMINAL_STATUSES.has(d.status)) {
            es?.close()
            es = null
            online = false
            if (reconnectTimer) clearTimeout(reconnectTimer)
            reconnectTimer = setTimeout(connect, SUSPENDED_DONE_RETRY_MS)
            return
          }
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
      scheduleReconnect()
    }
  }

  // 断线重连调度（TODO #16-5 T20 补播兜底）：页面可见期间持续重试（指数退避，
  // 封顶 sseRetryMaxDelayMs）；页面不可见时暂停——浏览器本就会掐后台页的 SSE，
  // 恢复可见/网络恢复时立即续连。重连成功后服务端先推全量快照，断档事件自动补齐，
  // 因此无需额外补播端点。
  const scheduleReconnect = () => {
    online = false
    if (document.visibilityState !== 'visible') {
      document.addEventListener('visibilitychange', onVisible, { once: true })
      return
    }
    const delay = Math.min(1000 * Math.pow(2, attempt), APP_CONFIG.sseRetryMaxDelayMs)
    attempt++
    reconnectTimer = setTimeout(connect, delay)
  }

  const onVisible = () => {
    document.removeEventListener('visibilitychange', onVisible)
    if (closed) return
    if (document.visibilityState === 'visible') {
      if (online && es) return // 连接还活着（后台页未必掐 SSE），无需重连
      attempt = 0
      connect()
    } else {
      scheduleReconnect()
    }
  }

  const onOnline = () => {
    if (closed) return
    if (online && es) return // 连接活着就不折腾
    if (reconnectTimer) clearTimeout(reconnectTimer)
    attempt = 0
    connect()
  }
  window.addEventListener('online', onOnline)

  connect()

  return () => {
    closed = true
    document.removeEventListener('visibilitychange', onVisible)
    window.removeEventListener('online', onOnline)
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
  // 后端当前直接返回 []*SessionLogRecord 且无 json tag（大写键的裸数组，无信封）；
  // 这里做兼容归一化：同时容忍裸数组/信封、大写/小写键。
  return fetchJson<unknown>(`/sessions/${id}/logs${q}`).then((raw) => {
    const list: unknown[] = Array.isArray(raw)
      ? raw
      : ((raw as SessionLogsResponse | null)?.logs ?? [])
    const logs = list.map((r): SessionLog => {
      const o = (r || {}) as Record<string, unknown>
      const pick = <T>(snake: string, pascal: string, dflt: T): T =>
        (o[snake] as T | undefined) ?? (o[pascal] as T | undefined) ?? dflt
      return {
        id: pick('id', 'ID', 0),
        session_id: pick('session_id', 'SessionID', ''),
        agent: pick('agent', 'Agent', ''),
        level: pick('level', 'Level', ''),
        phase: pick('phase', 'Phase', ''),
        message: pick('message', 'Message', ''),
        prompt: pick('prompt', 'Prompt', undefined),
        response: pick('response', 'Response', undefined),
        input_tokens: pick('input_tokens', 'InputTokens', 0),
        output_tokens: pick('output_tokens', 'OutputTokens', 0),
        model: pick('model', 'Model', ''),
        latency_ms: pick('latency_ms', 'LatencyMs', 0),
        created_at: pick('created_at', 'CreatedAt', ''),
        meta: pick('meta', 'Meta', undefined),
      }
    })
    return { session_id: id, logs, count: logs.length }
  })
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
