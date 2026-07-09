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
