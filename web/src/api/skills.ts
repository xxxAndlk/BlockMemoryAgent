import { fetchJson } from './client'
import type { Skill } from '@/types'

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

export function listSkills(): Promise<{ skills: Skill[] }> {
  return fetchJson('/skills')
}

export function getAgentSkills(agentID: string): Promise<AgentSkillsResponse> {
  return fetchJson(`/agents/${agentID}/skills`)
}
