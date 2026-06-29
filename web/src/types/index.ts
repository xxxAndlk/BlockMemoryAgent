export interface ChatMessage {
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
}

export interface Session {
  id: string
  goal: string
  status: 'running' | 'completed' | 'error' | 'awaiting_clarify'
  result?: string
  started_at: string
  ended_at?: string
  events: SessionEvent[]
  messages: ChatMessage[]
}

export interface SessionEvent {
  type: string
  agent: string
  message: string
  kind?: string
  tool?: string
  tool_path?: string
  tool_output?: string
  tool_error?: string
  tool_args?: string
  success?: boolean
  timestamp: string
  prompt?: string
  input_tokens?: number
  output_tokens?: number
  detail_json?: string
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
  tool_ref: string
  cost: number
  tags: string[]
}

export interface HealthStatus {
  name: string
  online: boolean
  detail: string
}
