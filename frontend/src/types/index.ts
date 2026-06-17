export interface Session {
  id: string
  goal: string
  status: 'running' | 'completed' | 'error'
  result?: string
  started_at: string
  ended_at?: string
  events: SessionEvent[]
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
  success?: boolean
  timestamp: string
}

export interface AgentNode {
  name: string
  type: string
  domain: string
  status: string
}

export interface SubTask {
  id: string
  title: string
  assignee?: string
  status: string
  result?: string
}

export interface TaskBoardData {
  topic_id: string
  goal: string
  status: string
  tasks: SubTask[]
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
