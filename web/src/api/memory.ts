import { fetchJson } from './client'

export interface AgentSnapshot {
  agent_id: string
  topic_id: string
  last_step_id: string
  key_summaries: { step_id: string; content: string; timestamp: string }[]
  open_issues: { id: string; description: string; created_at: string }[]
  local_vars: Record<string, unknown>
  published_ver: number
  updated_at: string
}

export interface SnapshotResponse {
  agent_id: string
  snapshot: AgentSnapshot | null
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

export interface MemoryLevelsResponse {
  agent_id: string
  topic_id: string
  total: number
  levels: Record<string, number>
}

export function getSnapshot(agentID: string, topicID: string): Promise<SnapshotResponse> {
  return fetchJson('/snapshot', {
    method: 'POST',
    body: JSON.stringify({ agent_id: agentID, topic_id: topicID }),
  })
}

export function searchMemory(
  agentID: string,
  topicID: string,
  query: string,
  limit = 10
): Promise<MemorySearchResponse> {
  return fetchJson('/memory/search', {
    method: 'POST',
    body: JSON.stringify({ agent_id: agentID, topic_id: topicID, query, limit }),
  })
}

export function getMemoryLevels(agentID: string, topicID: string): Promise<MemoryLevelsResponse> {
  return fetchJson(
    `/memory/levels?agent_id=${encodeURIComponent(agentID)}&topic_id=${encodeURIComponent(topicID)}`
  )
}
