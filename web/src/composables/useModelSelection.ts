import { ref } from 'vue'
import { addModel, listModels, switchModel } from '@/api/models'
import type { AddModelRequest, ModelCatalog } from '@/api/models'

// 模型选择共享状态（模块级单例，同 useWorkDir 模式）：
// ChatInput 弹层懒加载目录；切换/新增动作在此收敛，供多组件复用。
const catalog = ref<ModelCatalog | null>(null)
const loading = ref(false)
const loadError = ref('')

// 当前操作的目标角色（跨打开保持，默认 meta——web 对话直接驱动 MetaAgent）。
const selectedRole = ref('meta')
// 思考强度覆盖：空串 = 跟随角色 roles.yaml 配置。
const selectedThinking = ref('')

export function useModelSelection() {
  /** 懒加载模型目录（已加载且非强制刷新时跳过）。 */
  async function ensureLoaded(force = false) {
    if (loading.value) return
    if (catalog.value && !force) return
    loading.value = true
    loadError.value = ''
    try {
      catalog.value = await listModels()
      // 目录加载后把思考档同步为角色当前生效值（空=端点默认展示为跟随角色默认）。
      const role = catalog.value.roles.find((r) => r.role_id === selectedRole.value)
      if (role) selectedThinking.value = role.bound ? role.thinking || '' : ''
    } catch (e) {
      loadError.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 切换目标角色的绑定模型（含 60s 连通性探测，失败抛错由调用方提示）。 */
  async function doSwitch(modelId: string, thinking = selectedThinking.value) {
    const resp = await switchModel(selectedRole.value, modelId, thinking)
    await ensureLoaded(true)
    return resp
  }

  /** 新增模型条目（落 config/models.json）。 */
  async function doAdd(req: AddModelRequest) {
    const resp = await addModel(req)
    await ensureLoaded(true)
    return resp
  }

  return {
    catalog,
    loading,
    loadError,
    selectedRole,
    selectedThinking,
    ensureLoaded,
    doSwitch,
    doAdd,
  }
}
