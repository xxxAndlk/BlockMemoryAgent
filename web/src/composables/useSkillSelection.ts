import { computed, ref } from 'vue'
import { listSkills } from '@/api/skills'

/** 技能选择器条目：只保留展示所需字段（接口返回的 content 正文可能很大，不进选择器状态）。 */
export interface SkillOption {
  /** 技能规范化名称（池内 Name，缺省回退 SkillID）——提交给后端的就是它。 */
  id: string
  name: string
  description: string
  /** 来源：yaml | dir | bundle | builtin | learned。 */
  source: string
}

// 技能目录共享状态（模块级单例，同 usePluginSelection/useModelSelection 模式）：
// ChatInput 技能弹层懒加载目录；只读展示，管理仍在技能页。
const catalog = ref<SkillOption[] | null>(null)
const loading = ref(false)
const loadError = ref('')

export function useSkillSelection() {
  /** 懒加载技能目录（已加载且非强制刷新时跳过）。 */
  async function ensureLoaded(force = false) {
    if (loading.value) return
    if (catalog.value && !force) return
    loading.value = true
    loadError.value = ''
    try {
      const resp = await listSkills()
      const seen = new Set<string>()
      const out: SkillOption[] = []
      for (const s of resp.skills ?? []) {
        const id = (s.name || s.skill_id || '').trim()
        if (!id || seen.has(id)) continue
        seen.add(id)
        out.push({
          id,
          name: s.name || s.skill_id,
          description: s.description || '',
          source: s.source || '',
        })
      }
      out.sort((a, b) => a.name.localeCompare(b.name))
      catalog.value = out
    } catch (e) {
      loadError.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  /** 可指定的技能：全池（learned 技能已由后端注册进池）。 */
  const selectable = computed(() => catalog.value ?? [])

  return { catalog, selectable, loading, loadError, ensureLoaded }
}
