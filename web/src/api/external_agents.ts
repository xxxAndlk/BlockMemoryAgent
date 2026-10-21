import { fetchJson } from './client'

/** 外部 Agent 信息（与后端 external_agents_http.go 契约对齐）。 */
export interface ExternalAgentInfo {
  id: string
  command: string
  args: string[]
  timeout_sec: number
  enabled: boolean
  description: string
  /** exec.LookPath(command) 是否在 PATH 中命中。 */
  installed: boolean
}

export function listExternalAgents(): Promise<{ agents: ExternalAgentInfo[] }> {
  return fetchJson('/external-agents')
}

export function reloadExternalAgents(): Promise<{ status: string; agents: ExternalAgentInfo[] }> {
  return fetchJson('/external-agents/reload', { method: 'POST' })
}
