import { fetchJson } from './client'

/** 经验技能（learned_skills 表元数据，与后端 store.LearnedSkill 对齐）。 */
export interface LearnedSkill {
  name: string
  title: string
  when_to_use: string
  content_path: string
  enabled: boolean
  use_count: number
  source_session?: string
  outcome?: string
  created_at: string
  updated_at: string
}

export interface LearnedSkillDetail {
  skill: LearnedSkill
  content: string
}

/** 进化审计流水（evolution_log 表）。 */
export interface EvolutionLogEntry {
  id: number
  kind: string // user_pref|project_lesson|skill_create|skill_update
  target: string
  summary: string
  source_session?: string
  created_at: string
}

export function listLearnedSkills(): Promise<{ skills: LearnedSkill[] }> {
  return fetchJson('/skills/learned')
}

export function getLearnedSkill(name: string): Promise<LearnedSkillDetail> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}`)
}

export function saveLearnedSkill(
  name: string,
  body: { title: string; when_to_use: string; content: string }
): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}`, {
    method: 'PUT',
    body: JSON.stringify(body),
  })
}

export function enableLearnedSkill(name: string): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}/enable`, { method: 'POST' })
}

export function disableLearnedSkill(name: string): Promise<{ ok: boolean }> {
  return fetchJson(`/skills/learned/${encodeURIComponent(name)}/disable`, { method: 'POST' })
}

export function listEvolutionLog(limit = 200): Promise<{ entries: EvolutionLogEntry[] }> {
  return fetchJson(`/evolution/log?limit=${limit}`)
}
