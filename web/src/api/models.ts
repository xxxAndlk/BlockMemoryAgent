import { fetchJson } from './client'

/** 模型注册表条目视图（与后端 agent.ModelEntryView 对齐，不含 api_key）。 */
export interface ModelEntryView {
  id: string
  name?: string
  provider: string
  model: string
  base_url?: string
  max_output_tokens?: number
  description?: string
}

/** 单个可切换角色的当前模型状态（与后端 agent.RoleModelStatus 对齐）。 */
export interface RoleModelStatus {
  role_id: string
  provider: string
  model: string
  model_id?: string
  thinking?: string
  bound: boolean
}

/** GET /api/models 响应（与后端 agent.ModelCatalog 对齐）。 */
export interface ModelCatalog {
  models: ModelEntryView[]
  roles: RoleModelStatus[]
}

/** 新增模型条目请求体（POST /api/models；id 缺省由后端从 model 名生成）。 */
export interface AddModelRequest {
  name?: string
  provider: string
  model: string
  api_key?: string
  base_url?: string
  max_output_tokens?: number
  description?: string
}

export function listModels(): Promise<ModelCatalog> {
  return fetchJson('/models')
}

/** 切换角色绑定模型；后端含最长 60s 连通性探测，超时需放宽到 70s。
 *  thinking 空 = 跟随角色 roles.yaml 配置。 */
export function switchModel(
  role: string,
  modelId: string,
  thinking = '',
): Promise<{ status: string; role: string; model_id: string; provider: string; model: string }> {
  return fetchJson('/models/switch', {
    method: 'POST',
    body: JSON.stringify({ role, model_id: modelId, thinking }),
    timeoutMs: 70000,
  })
}

/** 新增模型条目（落 config/models.json，免重启热更新）。 */
export function addModel(req: AddModelRequest): Promise<{ status: string; id: string }> {
  return fetchJson('/models', {
    method: 'POST',
    body: JSON.stringify(req),
    timeoutMs: 15000,
  })
}
