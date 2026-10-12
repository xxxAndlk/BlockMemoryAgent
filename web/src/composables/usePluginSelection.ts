import { computed, ref } from 'vue'
import { listPlugins } from '@/api/plugins'
import type { PluginInfo } from '@/api/plugins'

// 插件目录共享状态（模块级单例，同 useModelSelection 模式）：
// ChatInput 插件弹层懒加载目录；只读展示，启停管理仍在插件页。
const catalog = ref<PluginInfo[] | null>(null)
const loading = ref(false)
const loadError = ref('')

export function usePluginSelection() {
  /** 懒加载插件目录（已加载且非强制刷新时跳过）。 */
  async function ensureLoaded(force = false) {
    if (loading.value) return
    if (catalog.value && !force) return
    loading.value = true
    loadError.value = ''
    try {
      const resp = await listPlugins()
      catalog.value = resp.plugins ?? []
    } catch (e) {
      loadError.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 可指定的插件：仅已启用且运行中的可选择（其余选了后端也会判无效）。 */
  const selectable = computed(() =>
    (catalog.value ?? []).filter((p) => p.enabled && p.state === 'running' && (p.tools?.length ?? 0) > 0),
  )

  return { catalog, selectable, loading, loadError, ensureLoaded }
}
