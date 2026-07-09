export type SessionStatus = 'running' | 'completed' | 'error' | 'awaiting_clarify'

export interface ChatMessage {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
}

export interface Session {
  id: string
  goal: string
  status: SessionStatus
  result?: string
  started_at: string
  ended_at?: string
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
