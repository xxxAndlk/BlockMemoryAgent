import { fetchJson } from './client'

export interface TimelinePoint {
  time: string
  calls: number
  tokens: number
}

export interface TimelineResponse {
  points: TimelinePoint[]
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

export interface AgentModelTokenStats {
  agent: string
  model: string
  input_tokens: number
  output_tokens: number
  calls: number
}

export interface SessionTokenMetricsResponse {
  session_id: string
  total_input_tokens: number
  total_output_tokens: number
  total_calls: number
  stats: AgentModelTokenStats[]
}

export function getTimeline(points?: number): Promise<TimelineResponse> {
  const qs = points ? `?points=${points}` : ''
  return fetchJson(`/metrics/timeline${qs}`)
}

export function getActivity(limit?: number): Promise<ActivityResponse> {
  const qs = limit ? `?limit=${limit}` : ''
  return fetchJson(`/activity${qs}`)
}

export function getSessionMetrics(id: string): Promise<SessionMetrics> {
  return fetchJson(`/sessions/${id}/metrics`)
}

export function getSessionTokenMetrics(id: string): Promise<SessionTokenMetricsResponse> {
  return fetchJson(`/sessions/${id}/token-metrics`)
}

// ===== 效率一等指标 + 子 Agent 审计面（TODO 第9⑥ / 第10③）=====

export interface EfficiencyBranch {
  node_id: string
  role: string
  domain: string
  task: string
  status: string
  wall_clock_sec: number
  tool_calls: number
  rounds: number
  files_written: string[]
}

export interface EfficiencyRoleStat {
  role: string
  calls: number
  input_tokens: number
  output_tokens: number
  avg_latency_ms: number
  p50_input_tokens: number
  p95_input_tokens: number
}

export interface SessionEfficiency {
  session_id: string
  tokens_per_file: number
  files_delivered: number
  avg_rounds_per_dispatch: number
  verify_token_share: number
  meta_domain_ratio: number
  input_p50: number
  input_p95: number
  total_input_tokens: number
  total_output_tokens: number
  branches: EfficiencyBranch[]
  role_stats: EfficiencyRoleStat[]
}

export interface AgentEventRow {
  type: string
  role: string
  content: string
  tool_name: string
  input: string
  output: string
  occurred: string
}

export function getSessionEfficiency(id: string): Promise<SessionEfficiency> {
  return fetchJson(`/sessions/${id}/efficiency`)
}

export function getAgentEvents(id: string, agentId: string, limit = 200, offset = 0): Promise<{ session_id: string; agent_id: string; events: AgentEventRow[] }> {
  return fetchJson(`/sessions/${id}/agents/${encodeURIComponent(agentId)}/events?limit=${limit}&offset=${offset}`)
}

export function pauseSessionAgent(id: string, agentId: string): Promise<{ session_id: string; agent_id: string; status: string }> {
  return fetchJson(`/sessions/${id}/agents/${encodeURIComponent(agentId)}/pause`, { method: 'POST' })
}

/** 终止指定子 Agent 实例（硬取消，不可恢复；编排页节点卡/面板的 ⏹ 动作）。 */
export function cancelSessionAgent(id: string, agentId: string): Promise<{ session_id: string; agent_id: string; status: string }> {
  return fetchJson(`/sessions/${id}/agents/${encodeURIComponent(agentId)}/cancel`, { method: 'POST' })
}
