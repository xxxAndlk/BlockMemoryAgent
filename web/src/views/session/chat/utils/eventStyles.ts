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
  if (k === 'assistant_text') return 'primary'
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
  if (k === 'assistant_text') return 'ChatDotRound'
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
    clarify_detail: '澄清详情',
    stats: '统计',
    message: '消息',
    memory_recall: '记忆召回',
    topic_switch: '话题切换',
    sub_agent_dispatch: '派发子Agent',
    sub_agent_done: '子Agent完成',
    assistant_text: '中间正文',
    interrupt: '中断',
    enqueue: '入队',
  }
  return map[k] || k
}

/** 高频 kind 的"说人话"状态短语（TODO #15 翻译层①，T10）：面向普通用户的进度描述，
 *  不暴露内部术语；长尾 kind 回落 kindLabel。新增 kind 记得同时补这两处映射。 */
const kindPhrases: Record<string, string> = {
  think: '正在思考…',
  intend: '确定了下一步动作',
  tool_call: '正在执行工具操作',
  tool_result: '工具执行完成',
  tool_exec: '正在执行工具操作',
  wait: '等待子 Agent 回传',
  sub_agent_dispatch: '派发子 Agent 执行分工任务',
  sub_agent_done: '子 Agent 完成并回传结果',
  memory_recall: '召回相关记忆作参考',
  graph_step: '推进任务流程',
  clarify: '等待补充说明',
  assistant_text: '汇报进展',
  llm_result: '汇总结果',
  llm_response: '汇总结果',
  message: '发送消息',
  error: '遇到问题，正在处理',
  agent_done: '本轮任务完成',
  agent_created: '组建执行团队',
  topic_switch: '切换话题上下文',
  interrupt: '已注入新指令',
  enqueue: '任务已排队',
}

/** kind/type 对应的用户可读状态短语；无映射时回落 kindLabel。 */
export function kindPhrase(kind?: string, type?: string): string {
  const k = kind || type || ''
  if (kindPhrases[k]) return kindPhrases[k]
  return kindLabel(kind, type)
}

/** 从事件流尾部找最近一条"有进度含义"的事件，返回其人话短语（T10 常驻状态行）。
 *  调试类（prompt/token_usage）跳过；找不到返回空串（调用方自行隐藏/回落）。 */
export function latestEventPhrase(events: SessionEvent[]): string {
  for (let i = events.length - 1; i >= 0; i--) {
    const ev = events[i]
    const k = ev.kind || ev.type || ''
    if (k === 'user_message' || k === 'prompt' || k === 'token_usage' || k === 'system') continue
    const p = kindPhrase(ev.kind, ev.type)
    if (p) return p
  }
  return ''
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
