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
