import { fetchJson } from './client'
import type { HealthStatus } from '@/types'

export interface HealthResponse {
  postgres: HealthStatus
  redis: HealthStatus
  llm: HealthStatus
}

export interface StatusResponse {
  program: string
  mode: string
  soul: string
  llm_provider: string
  llm_model: string
  version: string
}

export function getHealth(): Promise<HealthResponse> {
  return fetchJson('/health')
}

export function getStatus(): Promise<StatusResponse> {
  return fetchJson('/status')
}

// ---- 装机能力自检（TODO #18-1 T28）----

export interface CapabilityItem {
  /** 能力名：llm / postgres / redis / embed / plugins / workdir */
  name: string
  ok: boolean
  detail?: string
  /** 缺失的环境变量或配置键 */
  missing?: string[]
  /** ok=false 时的可行动修复提示 */
  hint?: string
}

export interface CapabilitiesResponse {
  items: CapabilityItem[]
  all_ok: boolean
}

/** 六类能力逐项自检（LLM/PG/Redis/嵌入/插件/工作目录），settings 能力面板数据源。 */
export function getCapabilities(): Promise<CapabilitiesResponse> {
  return fetchJson('/capabilities')
}
