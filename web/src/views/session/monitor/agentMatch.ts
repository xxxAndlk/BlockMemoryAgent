// 监控面「当前 Agent」归属匹配。
// 背景：后端事件/结构化日志的 agent 字段写的是 role.Name 展示名（"<实例名>领域Agent"，
// 长名会被截断成 "<前缀>…领域Agent"），与节点 name 不是精确相等；事件的 detail_json
// 里带有 {"agent_id": "<inst_id>"}（工具/进度类事件），是更可靠的归属口径。
// 因此匹配规则：优先 detail_json.agent_id === node.inst_id，否则用归一化名字前缀互判兜底。
import type { AgentNode, SessionEvent } from '@/types'
import type { SessionLog } from '@/api/session'

/** 把 "<名>领域Agent" / "<截断前缀>…领域Agent" 归一化为可比较的名字前缀。 */
export function normalizeAgentName(raw: string): string {
  let s = (raw || '').trim()
  const idx = s.indexOf('领域Agent')
  if (idx >= 0) s = s.slice(0, idx)
  if (s.endsWith('…') || s.endsWith('...')) s = s.replace(/…$|\.{3}$/, '')
  return s.trim()
}

/** 名字口径匹配：全等或前缀互含（覆盖截断名）。 */
export function matchAgentName(rawAgent: string, node: AgentNode): boolean {
  const base = normalizeAgentName(rawAgent)
  if (!base || !node.name) return false
  return base === node.name || node.name.startsWith(base) || base.startsWith(node.name)
}

/** 从 detail_json 提取 agent_id（后端工具/进度事件写入的实例归属）。 */
function eventAgentId(detailJson?: string): string {
  if (!detailJson) return ''
  try {
    const v = JSON.parse(detailJson)
    return typeof v?.agent_id === 'string' ? v.agent_id : ''
  } catch {
    return ''
  }
}

/** 事件是否属于指定 Agent 节点。 */
export function matchAgentEvent(ev: SessionEvent, node: AgentNode): boolean {
  if (eventAgentId(ev.detail_json) === node.inst_id) return true
  return matchAgentName(ev.agent, node)
}

/** 结构化日志（日志分析）是否属于指定 Agent 节点（无 detail_json，仅名字口径）。 */
export function matchAgentLog(log: SessionLog, node: AgentNode): boolean {
  return matchAgentName(log.agent, node)
}
