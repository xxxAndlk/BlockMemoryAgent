import type { SessionEvent } from '@/types'

export type TagType = 'success' | 'warning' | 'primary' | 'danger' | 'info'

/** Kind/type 到 Element Plus Tag type 的映射 */
export function kindTagType(kind?: string, type?: string): TagType {
  const k = kind || type || ''
  if (k === 'think') return 'warning'
  if (k === 'intend') return 'primary'
  if (k === 'llm' || k === 'llm_result' || k === 'llm_response') return 'success'
  if (k === 'tool_call') return 'primary'
  if (k === 'tool_result' || k === 'tool_exec') return 'success'
  if (k === 'error') return 'danger'
  if (k === 'prompt') return 'info'
  if (k === 'token_usage') return 'info'
  if (k === 'agent_created') return 'primary'
  if (k === 'wait') return 'warning'
  if (k === 'graph_step') return 'info'
  if (k === 'agent_done') return 'success'
  if (k === 'clarify') return 'warning'
  return 'info'
}

/** Kind 对应的 Element Plus icon 名称 */
export function kindIcon(kind?: string, type?: string): string {
  const k = kind || type || ''
  if (k === 'think') return 'ChatLineRound'
  if (k === 'intend') return 'Aim'
  if (k === 'llm' || k === 'llm_result' || k === 'llm_response') return 'MagicStick'
  if (k === 'tool_call') return 'Tools'
  if (k === 'tool_result' || k === 'tool_exec') return 'Check'
  if (k === 'error') return 'CircleClose'
  if (k === 'prompt') return 'EditPen'
  if (k === 'token_usage') return 'DataLine'
  if (k === 'agent_created') return 'UserFilled'
  if (k === 'wait') return 'Clock'
  if (k === 'graph_step') return 'Connection'
  if (k === 'agent_done') return 'CircleCheck'
  if (k === 'clarify') return 'QuestionFilled'
  return 'InfoFilled'
}

/** Kind 对应的中文标签 */
export function kindLabel(kind?: string, type?: string): string {
  const k = kind || type || ''
  const map: Record<string, string> = {
    think: '思考',
    intend: '意图',
    llm: 'LLM',
    llm_result: 'LLM 输出',
    llm_response: 'LLM 响应',
    tool_call: '工具调用',
    tool_result: '工具结果',
    tool_exec: '工具执行',
    error: '错误',
    prompt: 'Prompt',
    token_usage: 'Token',
    agent_created: '新建 Agent',
    wait: '等待',
    graph_step: '图步骤',
    agent_done: 'Agent 完成',
    user_message: '用户消息',
    system: '系统',
    progress: '进度',
    clarify: '需要澄清',
    stats: '统计',
  }
  return map[k] || k
}

/** Agent 名字到文本颜色（左侧标签） */
export function agentTextColor(agent: string): string {
  if (!agent) return 'text-ink'
  if (agent === 'MetaAgent') return 'text-blue-400'
  if (agent.startsWith('System') || agent === 'System') return 'text-ink-2'
  if (agent.includes('Domain')) return 'text-purple-400'
  if (agent.includes('SubDomain')) return 'text-fuchsia-400'
  if (agent.includes('Assistant')) return 'text-emerald-400'
  if (agent === 'User') return 'text-sky-300'
  return 'text-ink'
}

/** 时间戳 → HH:MM:SS */
export function fmtTime(iso?: string): string {
  if (!iso) return ''
  try {
    return new Date(iso).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  } catch {
    return iso
  }
}

/** 详情可展开判断 */
export function hasDetail(ev: SessionEvent): boolean {
  return !!(ev.tool_output || ev.tool_error || ev.detail_json || ev.prompt)
}
