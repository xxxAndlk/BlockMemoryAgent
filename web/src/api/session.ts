import type { Session, SessionEvent, AgentNode } from '@/types'

const API_BASE = '/api'

async function fetchJson<T>(url: string, options?: RequestInit): Promise<T> {
  const r = await fetch(`${API_BASE}${url}`, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  })
  if (!r.ok) throw new Error(`${r.status} ${r.statusText}`)
  return r.json() as Promise<T>
}

export function listSessions(): Promise<Session[]> {
  return fetchJson('/sessions')
}

export function createSession(goal: string): Promise<Session> {
  return fetchJson('/sessions', {
    method: 'POST',
    body: JSON.stringify({ goal }),
  })
}

export function getSession(id: string): Promise<Session> {
  return fetchJson(`/sessions/${id}`)
}

export function getSessionBoard(id: string): Promise<{ session_id: string; board: any }> {
  return fetchJson(`/sessions/${id}/board`)
}

export function getSessionAgents(id: string): Promise<{ session_id: string; agents: AgentNode[]; tree: AgentNode[] }> {
  return fetchJson(`/sessions/${id}/agents`)
}

export function sendMessage(id: string, content: string): Promise<void> {
  return fetchJson(`/sessions/${id}/message`, {
    method: 'POST',
    body: JSON.stringify({ content }),
  })
}

export function cancelSession(id: string): Promise<{ session_id: string; status: string }> {
  return fetchJson(`/sessions/${id}/cancel`, { method: 'POST' })
}

export function clarifySession(id: string, answer: string): Promise<void> {
  return fetchJson(`/sessions/${id}/clarify`, {
    method: 'POST',
    body: JSON.stringify({ answer }),
  })
}

export function streamSession(
  id: string,
  onEvent: (ev: SessionEvent) => void,
  onDone?: (finalStatus?: string) => void,
  onError?: (err: Error) => void
): () => void {
  const es = new EventSource(`${API_BASE}/sessions/${id}/stream`)
  es.onmessage = (e) => {
    try {
      const d = JSON.parse(e.data)
      if (d.type === 'done') {
        es.close()
        // 后端 done 帧携带真实 status (completed/error/awaiting_clarify)，转发给调用方
        onDone?.(d.status)
        return
      }
      onEvent(d as SessionEvent)
    } catch (err) {
      onError?.(err as Error)
    }
  }
  es.onerror = () => {
    onError?.(new Error('SSE error'))
  }
  return () => es.close()
}

export interface HealthStatus {
  name: string
  online: boolean
  latency_ms?: number
  detail: string
}

export interface HealthResponse {
  postgres: HealthStatus
  redis: HealthStatus
  llm: HealthStatus
}

export function getHealth(): Promise<HealthResponse> {
  return fetchJson('/health')
}

export interface StatusResponse {
  program: string
  mode: string
  soul: string
  llm_provider: string
  llm_model: string
  version: string
}

export function getStatus(): Promise<StatusResponse> {
  return fetchJson('/status')
}

export interface TimelinePoint {
  time: string
  calls: number
  tokens: number
}

export interface TimelineResponse {
  points: TimelinePoint[]
}

export function getTimeline(points?: number): Promise<TimelineResponse> {
  const qs = points ? `?points=${points}` : ''
  return fetchJson(`/metrics/timeline${qs}`)
}

export interface ActivityItem {
  session_id: string
  agent: string
  kind: string
  content: string
  time: string
}

export interface ActivityResponse {
  activities: ActivityItem[]
}

export function getActivity(limit?: number): Promise<ActivityResponse> {
  const qs = limit ? `?limit=${limit}` : ''
  return fetchJson(`/activity${qs}`)
}

export interface SessionMetrics {
  session_id: string
  calls: number
  timeouts: number
  avg_duration: string
  max_duration: string
  input_tokens: number
  output_tokens: number
  total_tokens: number
}

export function getSessionMetrics(id: string): Promise<SessionMetrics> {
  return fetchJson(`/sessions/${id}/metrics`)
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

export interface AgentSnapshot {
  agent_id: string
  topic_id: string
  last_step_id: string
  key_summaries: { step_id: string; content: string; timestamp: string }[]
  open_issues: { id: string; description: string; created_at: string }[]
  local_vars: Record<string, any>
  published_ver: number
  updated_at: string
}

export interface SnapshotResponse {
  agent_id: string
  snapshot: AgentSnapshot | null
}

export function getSnapshot(agentID: string, topicID: string): Promise<SnapshotResponse> {
  return fetchJson('/snapshot', {
    method: 'POST',
    body: JSON.stringify({ agent_id: agentID, topic_id: topicID }),
  })
}

export interface MemorySearchResult {
  step_id: string
  summary: string
  action: string
  importance: number
  score: number
  time: string
}

export interface MemorySearchResponse {
  agent_id: string
  query: string
  results: MemorySearchResult[]
}

export function searchMemory(agentID: string, topicID: string, query: string, limit = 10): Promise<MemorySearchResponse> {
  return fetchJson('/memory/search', {
    method: 'POST',
    body: JSON.stringify({ agent_id: agentID, topic_id: topicID, query, limit }),
  })
}

export interface MemoryLevelsResponse {
  agent_id: string
  topic_id: string
  total: number
  levels: Record<string, number>
}

export function getMemoryLevels(agentID: string, topicID: string): Promise<MemoryLevelsResponse> {
  return fetchJson(`/memory/levels?agent_id=${encodeURIComponent(agentID)}&topic_id=${encodeURIComponent(topicID)}`)
}

export interface Skill {
  skill_id: string
  name: string
  description: string
  domain: string
  tool_ref: string
  cost: number
  tags: string[]
}

export interface SkillsResponse {
  skills: Skill[]
}

export function listSkills(): Promise<SkillsResponse> {
  return fetchJson('/skills')
}

export interface SkillSet {
  owner_agent: string
  domain: string
  skills: Skill[]
  created_at: string
}

export interface AgentSkillsResponse {
  agent_id: string
  skillset: SkillSet | null
}

export function getAgentSkills(agentID: string): Promise<AgentSkillsResponse> {
  return fetchJson(`/agents/${agentID}/skills`)
}

export interface FileItem {
  path: string
  size: number
  name: string
}

export interface FilesResponse {
  session_id: string
  files: FileItem[]
}

export function listFiles(sessionID: string): Promise<FilesResponse> {
  return fetchJson(`/files?session=${encodeURIComponent(sessionID)}`)
}

export interface FileContentResponse {
  path: string
  content: string
}

export function getFileContent(path: string): Promise<FileContentResponse> {
  return fetchJson(`/files/content?path=${encodeURIComponent(path)}`)
}
