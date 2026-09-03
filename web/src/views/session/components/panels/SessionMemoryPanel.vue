<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  listEvolutionLog,
  listLearnedSkills,
  type EvolutionLogEntry,
  type LearnedSkill,
} from '@/api/learned'

const props = defineProps<{ sessionId: string }>()

const entries = ref<EvolutionLogEntry[]>([])
const skills = ref<LearnedSkill[]>([])
const loading = ref(false)

// 本会话沉淀：evolution_log 与 learned_skills 均带 source_session 字段，前端按之过滤。
async function load() {
  if (!props.sessionId) {
    entries.value = []
    skills.value = []
    return
  }
  loading.value = true
  try {
    const [logRes, skillRes] = await Promise.all([listEvolutionLog(200), listLearnedSkills()])
    entries.value = (logRes.entries || []).filter((e) => e.source_session === props.sessionId)
    skills.value = (skillRes.skills || []).filter((s) => s.source_session === props.sessionId)
  } catch {
    entries.value = []
    skills.value = []
  } finally {
    loading.value = false
  }
}

watch(() => props.sessionId, load, { immediate: true })

function kindLabel(kind: string) {
  switch (kind) {
    case 'user_pref': return '用户偏好'
    case 'project_lesson': return '项目经验'
    case 'skill_create': return '技能新建'
    case 'skill_update': return '技能更新'
    default: return kind
  }
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}
</script>

<template>
  <div v-loading="loading" class="space-y-4 text-xs">
    <div>
      <div class="font-bold text-sm text-ink mb-2">沉淀技能</div>
      <div v-if="!skills.length" class="text-ink-3 py-2">本会话未沉淀技能</div>
      <div v-for="s in skills" :key="s.name" class="p-2 bg-page rounded border border-line mb-1.5">
        <div class="flex items-center gap-2">
          <span class="font-bold text-ink">{{ s.title }}</span>
          <el-tag size="small" effect="plain" :type="s.outcome === 'success' ? 'success' : 'danger'">
            源自{{ s.outcome === 'success' ? '成功' : '失败' }}会话
          </el-tag>
        </div>
        <p class="text-ink-2 mt-1 line-clamp-2">{{ s.when_to_use }}</p>
      </div>
    </div>

    <div>
      <div class="font-bold text-sm text-ink mb-2">沉淀记录</div>
      <div v-if="!entries.length" class="text-ink-3 py-2">本会话暂无沉淀记录（会话结束后自动沉淀）</div>
      <div v-for="e in entries" :key="e.id" class="p-2 bg-page rounded border border-line mb-1.5">
        <div class="flex items-center gap-2">
          <el-tag size="small" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
          <span class="font-bold text-ink">{{ e.target }}</span>
          <span class="text-ink-3 ml-auto">{{ fmtTime(e.created_at) }}</span>
        </div>
        <p class="text-ink-2 mt-1">{{ e.summary }}</p>
      </div>
    </div>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
