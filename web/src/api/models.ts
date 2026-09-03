import { fetchJson } from './client'

/** 模型预设（与后端 types.ModelPreset 对齐）。 */
export interface ModelPreset {
  id: string
  name?: string
  provider: string
  model: string
  base_url?: string
}

/** 单个可切换角色的当前模型状态（与后端 agent.RoleModelStatus 对齐）。 */
export interface RoleModelStatus {
  role_id: string
  provider: string
  model: string
  preset_id?: string
  overridden: boolean
}

/** GET /api/models 响应（与后端 agent.ModelCatalog 对齐）。 */
export interface ModelCatalog {
  presets: ModelPreset[]
  roles: RoleModelStatus[]
}

export function listModels(): Promise<ModelCatalog> {
  return fetchJson('/models')
}

/** 切换角色模型；后端含最长 60s 连通性探测，超时需放宽到 70s。 */
export function switchModel(
  role: string,
  preset: string,
): Promise<{ status: string; role: string; preset: string; provider: string; model: string }> {
  return fetchJson('/models/switch', {
    method: 'POST',
    body: JSON.stringify({ role, preset }),
    timeoutMs: 70000,
  })
}
