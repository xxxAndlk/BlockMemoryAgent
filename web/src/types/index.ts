export type SessionStatus =
  | 'running'
  | 'completed'
  | 'error'
  | 'awaiting_clarify'
  | 'paused_on_child'

/** 用户消息图片的 HTTP 线型（与后端 agent.WireImage 对齐）。
 *  data 为 base64 编码内容（不带 data: 前缀）；后端 Go []byte JSON 编解码即 base64。 */
export interface WireImage {
  mime_type: string
  data: string
}

/** 待澄清问题的选项（来自 SSE awaiting_clarify 帧） */
export interface ClarifyOption {
  id: string
  label: string
  description?: string
}

export interface ChatMessage {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
}

/** 信任模式（TODO 第10⑥ 三级信任，对标 Codex）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主。 */
export type TrustMode = 'suggest' | 'auto-edit' | 'full-auto'

export interface Session {
  id: string
  goal: string
  status: SessionStatus
  result?: string
  started_at: string
  ended_at?: string
  /** 软停止销毁倒计时截止时间（TODO #37）：软停止后非空，续跑/到期后清空。 */
  destroy_at?: string | null
  /** 每会话工作目录：空/缺省 = 后端默认目录。 */
  work_dir?: string
  /** 信任模式（TODO 第10⑥）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主；空 = 后端回退现网语义。 */
  trust_mode?: TrustMode | ''
  events: SessionEvent[]
  messages: ChatMessage[]
}

export interface BaseSessionEvent {
  type: string
  agent: string
  message: string
  timestamp: string
  kind?: string
  tool?: string
  tool_path?: string
  tool_output?: string
  tool_error?: string
  tool_args?: string
  success?: boolean
  prompt?: string
  input_tokens?: number
  output_tokens?: number
  detail_json?: string
}

export interface UserMessageEvent extends BaseSessionEvent {
  type: 'user_message'
}

export interface ToolCallEvent extends BaseSessionEvent {
  type: 'tool_call'
  kind: 'tool_call'
}

export interface ToolExecEvent extends BaseSessionEvent {
  type: 'tool_exec'
}

export interface LLMEvent extends BaseSessionEvent {
  kind: 'llm' | 'llm_result' | 'llm_response' | 'think' | 'intend' | 'prompt' | 'agent_done' | 'wait' | 'graph_step'
}

export interface ErrorEvent extends BaseSessionEvent {
  type: 'error'
  kind?: 'error'
}

export interface TokenUsageEvent extends BaseSessionEvent {
  kind: 'token_usage'
}

export type SessionEvent =
  | UserMessageEvent
  | ToolCallEvent
  | ToolExecEvent
  | LLMEvent
  | ErrorEvent
  | TokenUsageEvent
  | BaseSessionEvent

export function isUserMessageEvent(ev: SessionEvent): ev is UserMessageEvent {
  return ev.type === 'user_message'
}

export function isToolCallEvent(ev: SessionEvent): ev is ToolCallEvent {
  return ev.type === 'tool_call' || ev.kind === 'tool_call'
}

export function isToolExecEvent(ev: SessionEvent): ev is ToolExecEvent {
  return ev.type === 'tool_exec'
}

export function isLLMEvent(ev: SessionEvent): ev is LLMEvent {
  const kinds = new Set([
    'llm',
    'llm_result',
    'llm_response',
    'think',
    'intend',
    'prompt',
    'agent_done',
    'wait',
    'graph_step',
  ])
  return !!(ev.kind && kinds.has(ev.kind))
}

export function isErrorEvent(ev: SessionEvent): ev is ErrorEvent {
  return ev.type === 'error' || ev.kind === 'error' || ev.success === false
}

export function isTokenUsageEvent(ev: SessionEvent): ev is TokenUsageEvent {
  return ev.kind === 'token_usage'
}

export interface AgentNode {
  inst_id: string
  role_def_id: string
  name: string
  type: string
  domain: string
  status: string
  parent_id: string
  goal?: string
  block_id?: string
  /** 活动证据：最近活动种类（llm_start/tool:<名>/stream 等），空=无监控条目 */
  activity_kind?: string
  /** 活动证据：最近活动距今时长（"5s" 格式） */
  last_activity_ago?: string
}

export interface SubTask {
  id: string
  title: string
  assignee?: string
  status: string
  result?: string
  depends_on?: string[]
  created_at: string
  updated_at: string
}

export interface TaskBoardData {
  topic_id: string
  goal: string
  status: string
  constraints?: Record<string, string>
  tasks: SubTask[]
  updated_at: string
}

export interface Skill {
  skill_id: string
  name: string
  description: string
  domain: string
  tool_ref: string
  cost: number
  tags: string[]
}

export interface HealthStatus {
  name: string
  online: boolean
  latency_ms?: number
  detail: string
}
