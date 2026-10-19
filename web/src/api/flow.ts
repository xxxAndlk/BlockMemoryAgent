import { fetchJson } from './client'

/** 流程节点类型（后端权威：start|llm|tool|branch|approval|answer）。 */
export type FlowNodeType = 'start' | 'llm' | 'tool' | 'branch' | 'approval' | 'answer'

/** start.inputs / llm.outputs 的条目。 */
export interface FlowIOField {
  key: string
  type: string
  required?: boolean
  description?: string
}

/** branch when 条件的单个比较。 */
export interface FlowBranchCondition {
  left: string
  op: string
  right?: string
}

/** branch cases 的一条分支。 */
export interface FlowBranchCase {
  label: string
  when: FlowBranchCondition[]
  next: string
}

/** 节点 config 按类型取子集用（任意键随配置带出）。 */
export interface FlowNodeConfig {
  inputs?: FlowIOField[]
  outputs?: FlowIOField[] | Record<string, unknown>
  model?: string
  system?: string
  prompt?: string
  temperature?: number
  tool?: string
  args?: Record<string, unknown>
  cases?: FlowBranchCase[]
  default?: string
  question?: string
  [key: string]: unknown
}

/** 流程图节点（flow JSON 里的 nodes 元素）。 */
export interface FlowNodeDef {
  id: string
  type: FlowNodeType
  name?: string
  position: { x: number; y: number }
  depends_on: string[]
  config: FlowNodeConfig
}

/** flow JSON 全文（.bma/flows/<id>.json 的解析结果，前端组装的保存对象）。 */
export interface FlowDef {
  id: string
  name: string
  version: number
  description?: string
  nodes: FlowNodeDef[]
}

/** 流程摘要（GET /flow/flows 一行）。 */
export interface FlowSummary {
  id: string
  name: string
  version: number
  description?: string
  node_count: number
  error?: string
}

/** 流程全文（GET /flow/flows/:id；json 为文件原文，损坏时 error 非空）。 */
export interface FlowDetail {
  id: string
  version: number
  name: string
  json: string
  error?: string
}

/** 运行行（flow_runs）。 */
export interface FlowRun {
  id: string
  flow_id: string
  flow_name: string
  flow_version: number
  status: string // pending|running|waiting_approval|done|failed|aborted
  error?: string
  outputs?: Record<string, unknown>
  inputs?: Record<string, unknown>
  workdir: string
  session_id?: string
  created_at: string
  updated_at: string
  finished_at?: string
}

/** 节点运行行（flow_node_runs）。 */
export interface FlowNodeRun {
  id: string
  run_id: string
  node_id: string
  node_type: string
  status: string // pending|running|success|failed|waiting|skipped
  inputs?: Record<string, unknown>
  outputs?: Record<string, unknown>
  error?: string
  started_at?: string
  finished_at?: string
}

function qs(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') q.set(k, String(v))
  }
  const s = q.toString()
  return s ? `?${s}` : ''
}

/** 扫工作区流程目录。 */
export function listFlows(workdir: string): Promise<{ flows: FlowSummary[] }> {
  return fetchJson(`/flow/flows${qs({ workdir })}`)
}

/** 流程 JSON 全文。 */
export function getFlow(workdir: string, id: string): Promise<FlowDetail> {
  return fetchJson(`/flow/flows/${encodeURIComponent(id)}${qs({ workdir })}`)
}

/** 保存流程（后端解析+校验+版本自动递增；校验失败 400 纯文本中文错误）。 */
export function saveFlow(workdir: string, id: string, json: string): Promise<{ id: string; version: number; name: string }> {
  return fetchJson(`/flow/flows/${encodeURIComponent(id)}`, {
    method: 'PUT',
    body: JSON.stringify({ workdir, json }),
  })
}

/** 发起一次流程运行。 */
export function startFlowRun(workdir: string, flowId: string, inputs: Record<string, unknown>, sessionId?: string): Promise<{ run_id: string; status: string }> {
  return fetchJson('/flow/runs', {
    method: 'POST',
    body: JSON.stringify({ workdir, flow_id: flowId, inputs, session_id: sessionId || undefined }),
  })
}

/** 运行列表（时间倒序）。 */
export function listFlowRuns(opts?: { flow_id?: string; status?: string; limit?: number }): Promise<{ runs: FlowRun[] }> {
  return fetchJson(`/flow/runs${qs(opts || {})}`)
}

/** 运行详情：run 行 + 全部节点运行。 */
export function getFlowRun(id: string): Promise<{ run: FlowRun; node_runs: FlowNodeRun[] }> {
  return fetchJson(`/flow/runs/${encodeURIComponent(id)}`)
}

/** 中止运行。 */
export function abortFlowRun(id: string): Promise<{ aborted: boolean }> {
  return fetchJson(`/flow/runs/${encodeURIComponent(id)}/abort`, { method: 'POST' })
}

/** approval 节点裁决：通过继续走，拒绝 run 失败。 */
export function decideFlowNode(runId: string, nodeId: string, approved: boolean, comment?: string): Promise<{ decided: boolean }> {
  return fetchJson(`/flow/runs/${encodeURIComponent(runId)}/nodes/${encodeURIComponent(nodeId)}/decide`, {
    method: 'POST',
    body: JSON.stringify({ approved, comment }),
  })
}
