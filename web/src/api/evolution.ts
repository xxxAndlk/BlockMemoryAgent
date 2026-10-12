import { fetchJson } from './client'

/** 角色自进化准则（每角色一条 ≤300 字抽象行为准则，AI 会话中自维护，每轮注入系统提示词）。 */
export interface RoleEvolutionPrompt {
  role_id: string
  role_name: string
  content: string
  source_session?: string
  updated_at: string
}

export function listEvolutionPrompts(): Promise<{ prompts: RoleEvolutionPrompt[] }> {
  return fetchJson('/roles/evolution-prompts')
}

export function setEvolutionPrompt(roleId: string, content: string): Promise<RoleEvolutionPrompt> {
  return fetchJson(`/roles/${encodeURIComponent(roleId)}/evolution-prompt`, {
    method: 'PUT',
    body: JSON.stringify({ content }),
  })
}
