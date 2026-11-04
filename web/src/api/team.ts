import { fetchJson } from './client'

/** C-03 自动派发 attempt 行（GET /team/attempts）。 */
export interface DispatchAttempt {
  session_id: string
  plan_version: number
  task_id: string
  attempt_no: number
  attempt_id: string
  dispatch_key: string
  status: string
  parent_agent_id?: string
  agent_id?: string
  spec_version?: string
  context_manifest_hash?: string
  reserved_at: string
  started_at?: string
  finished_at?: string
  result_id?: string
  last_error?: string
}

export async function listTeamAttempts(sessionId: string): Promise<DispatchAttempt[]> {
  const q = encodeURIComponent(sessionId)
  const res = await fetchJson<{ attempts: DispatchAttempt[] }>(`/team/attempts?session_id=${q}`)
  return res.attempts || []
}

export async function reconcileTeam(sessionId: string): Promise<string[]> {
  const res = await fetchJson<{ reconciled: string[] }>('/team/reconcile', {
    method: 'POST',
    body: JSON.stringify({ session_id: sessionId }),
  })
  return res.reconciled || []
}

/** 人工批准当前计划版本；未批准的计划不会被自动调度。 */
export async function approveTeamPlan(sessionId: string): Promise<{ plan_version: number }> {
  return fetchJson<{ plan_version: number }>('/team/approve', {
    method: 'POST',
    body: JSON.stringify({ session_id: sessionId }),
  })
}
