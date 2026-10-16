import { fetchJson } from './client'

/** 配方摘要（GET /workflow/recipes 一行）。 */
export interface RecipeSummary {
  id: string
  title: string
  version: number
  tool: string
  deliverable_type: string
  frames: number
  cell: string
  stage_count: number
  anchor_count: number
  error?: string
}

/** 配方全文（GET /workflow/recipes/:id）。 */
export interface RecipeDetail {
  id: string
  version?: number
  title?: string
  yaml: string
  error?: string
}

/** 名册件（roster JSON 平铺对象，任意槽位键随件带出）。 */
export interface RosterItemInfo {
  id: string
  name?: string
  stages?: string[]
  [slot: string]: unknown
}

/** 运行行（workflow_runs）。 */
export interface WorkflowRun {
  id: string
  owner: string
  session_id: string
  workdir: string
  recipe_id: string
  recipe_version: number
  recipe_yaml?: string
  roster: RosterItemInfo[]
  status: string // pending|running|paused|done|failed|aborted
  pause_reason?: string // budget_exceeded|user|circuit_breaker
  budget_cap: number
  spent_estimate: number
  concurrency: number
  fail_fast: boolean
  created_at: string
  updated_at: string
  finished_at?: string
  /** 运行列表附带的最新 attempt 状态计数（cell 口径）。 */
  counts?: Record<string, number>
}

/** 单元行（workflow_units 的一次 attempt；重摇生成新行，旧行保留）。 */
export interface WorkflowUnit {
  id: string
  run_id: string
  item_id: string
  item_name?: string
  stage: string
  attempt_no: number
  status: string // pending|running|done|needs_review|failed|skipped
  artifact_path?: string
  check_result?: string
  reject_reason?: string
  idem_key: string
  created_at: string
  updated_at: string
}

/** unit 的 check_result JSON（程序/评委/漂移/升级 四段）。 */
export interface UnitCheckResult {
  program?: { pass: boolean; items?: { key: string; pass: boolean; reason?: string }[] }
  judge?: { pass: boolean; items?: { item: string; pass: boolean; reason?: string }[]; err?: string }
  drift?: { flag: boolean; distance?: number; samples?: number; note?: string }
  upgrade?: { agent_id?: string; role?: string; domain?: string; error?: string }
  error?: string
}

function qs(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

/** 扫工作区配方目录。 */
export function listRecipes(workdir: string): Promise<{ recipes: RecipeSummary[] }> {
  return fetchJson(`/workflow/recipes${qs({ workdir })}`)
}

/** 配方 YAML 全文。 */
export function getRecipe(workdir: string, id: string): Promise<RecipeDetail> {
  return fetchJson(`/workflow/recipes/${encodeURIComponent(id)}${qs({ workdir })}`)
}

/** 保存配方（版本自动递增；回灌草案确认落盘也走这里）。 */
export function saveRecipe(workdir: string, id: string, yaml: string): Promise<{ id: string; version: number }> {
  return fetchJson(`/workflow/recipes/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify({ workdir, yaml }),
  })
}

/** 运行列表（默认全量倒序）。 */
export function listRuns(opts?: { session_id?: string; status?: string; limit?: number }): Promise<{ runs: WorkflowRun[] }> {
  return fetchJson(`/workflow/runs${qs(opts || {})}`)
}

/** 运行详情：run 行 + 全部 units（含全部 attempt 行）。 */
export function getRun(id: string): Promise<{ run: WorkflowRun; units: WorkflowUnit[] }> {
  return fetchJson(`/workflow/runs/${encodeURIComponent(id)}`)
}

export function pauseRun(id: string): Promise<{ paused: boolean }> {
  return fetchJson(`/workflow/runs/${encodeURIComponent(id)}/pause`, { method: 'POST' })
}

export function resumeRun(id: string, extraBudget = 0): Promise<{ resumed: boolean }> {
  return fetchJson(`/workflow/runs/${encodeURIComponent(id)}/resume`, {
    method: 'POST',
    body: JSON.stringify({ extra_budget: extraBudget }),
  })
}

export function abortRun(id: string): Promise<{ aborted: boolean }> {
  return fetchJson(`/workflow/runs/${encodeURIComponent(id)}/abort`, { method: 'POST' })
}

/** 人审操作：accept / reject（必带 reason）/ reroll / skip。 */
export function unitAction(unitId: string, action: 'accept' | 'reject' | 'reroll' | 'skip', reason = ''): Promise<{ unit_id: string }> {
  return fetchJson(`/workflow/units/${encodeURIComponent(unitId)}/${action}`, {
    method: 'POST',
    body: JSON.stringify({ reason }),
  })
}

/** 回灌配方：聚合本 run 驳回理由 → 配方 v+1 草案（未落盘，确认后走 saveRecipe）。 */
export function runFeedback(id: string, timeoutMs = 90000): Promise<{ run_id: string; draft: string; current: string }> {
  return fetchJson(`/workflow/runs/${encodeURIComponent(id)}/feedback`, { method: 'POST', timeoutMs })
}
