import { fetchJson } from './client'

/** DAG 任务节点（与后端 pkg/types.Task 对齐，JSON 为 snake_case）。 */
export interface DagTask {
  id: string
  goal: string
  depends_on: string[]
  status: string // pending / running / completed / failed
  session_id: string
  started_at?: string
  finished_at?: string
}

/** DAG 定时任务（与后端 pkg/types.DAG 对齐）。 */
export interface DagJob {
  id: string
  name: string
  cron: string
  enabled: boolean
  tasks: DagTask[]
  created_at: string
  updated_at: string
}

/** GET /api/dag — 列出全部 DAG（后端直接返回数组）。 */
export function listDags(): Promise<DagJob[]> {
  return fetchJson('/dag')
}

/** POST /api/dag — 按 id upsert（新建/更新同一个端点），返回保存后的 DAG。 */
export function saveDag(dag: DagJob): Promise<DagJob> {
  return fetchJson('/dag', { method: 'POST', body: JSON.stringify(dag) })
}

/** DELETE /api/dag/:id — 删除 DAG。 */
export function deleteDag(id: string): Promise<{ id: string; deleted: boolean }> {
  return fetchJson(`/dag/${id}`, { method: 'DELETE' })
}

/** POST /api/dag/:id/trigger — 跳过 cron 检查立即派发一次。 */
export function triggerDag(id: string): Promise<{ id: string; triggered: boolean }> {
  return fetchJson(`/dag/${id}/trigger`, { method: 'POST' })
}

/** GET /api/dag/running — 运行中 DAG 快照（map: id -> DAG）。 */
export function getRunning(): Promise<Record<string, DagJob>> {
  return fetchJson('/dag/running')
}
