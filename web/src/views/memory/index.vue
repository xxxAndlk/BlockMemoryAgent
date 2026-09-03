<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'
import { getProfile, getProjectPreferences } from '@/api/preferences'
import {
  listLearnedSkills,
  listEvolutionLog,
  type LearnedSkill,
  type EvolutionLogEntry,
} from '@/api/learned'

const sessions = ref<Session[]>([])
const skills = ref<LearnedSkill[]>([])
const entries = ref<EvolutionLogEntry[]>([])
const profile = ref('')
const projectPref = ref('')
const loading = ref(false)

// 项目筛选：'' = 全部项目；否则为某 work_dir
const project = ref('')
// 类型筛选：'' = 全部
const kind = ref('')

const kindOptions = [
  { value: '', label: '全部' },
  { value: 'user_pref', label: '用户偏好' },
  { value: 'project_lesson', label: '项目经验' },
  { value: 'skill_create', label: '技能新建' },
  { value: 'skill_update', label: '技能更新' },
]

// 会话 id → work_dir 映射：记忆条目的项目归属经 source_session 反查
const sessionDir = computed(() => {
  const m = new Map<string, string>()
  for (const s of sessions.value) m.set(s.id, s.work_dir || '')
  return m
})

const projectOptions = computed(() => {
  const dirs = Array.from(new Set(sessions.value.map((s) => s.work_dir || ''))).sort()
  return [{ value: '', label: '全部项目' }, ...dirs.map((d) => ({ value: d, label: d || '默认目录' }))]
})

// 无 source_session / 反查不到会话 = 全局条目，仅「全部项目」可见
function inProject(sourceSession?: string) {
  if (!project.value) return true
  if (!sourceSession) return false
  return sessionDir.value.get(sourceSession) === project.value
}

const filteredEntries = computed(() =>
  entries.value.filter((e) => inProject(e.source_session) && (!kind.value || e.kind === kind.value))
)

const filteredSkills = computed(() => skills.value.filter((s) => inProject(s.source_session)))

const stats = computed(() => ({
  skillTotal: filteredSkills.value.length,
  skillEnabled: filteredSkills.value.filter((s) => s.enabled).length,
  entryTotal: filteredEntries.value.length,
  sessionTotal: new Set(
    filteredEntries.value.map((e) => e.source_session).filter(Boolean)
  ).size,
}))

async function load() {
  loading.value = true
  try {
    const [sRes, skRes, logRes, pfRes] = await Promise.allSettled([
      listSessions(),
      listLearnedSkills(),
      listEvolutionLog(200),
      getProfile(),
    ])
    sessions.value = sRes.status === 'fulfilled' ? sRes.value || [] : []
    skills.value = skRes.status === 'fulfilled' ? skRes.value.skills || [] : []
    entries.value = logRes.status === 'fulfilled' ? logRes.value.entries || [] : []
    profile.value = pfRes.status === 'fulfilled' ? pfRes.value.content || '' : ''
  } catch (e) {
    ElMessage.error('记忆数据加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function loadProjectPref() {
  if (!project.value) {
    projectPref.value = ''
    return
  }
  try {
    const res = await getProjectPreferences(project.value)
    projectPref.value = res.content || ''
  } catch {
    projectPref.value = ''
  }
}

watch(project, loadProjectPref)
onMounted(load)

function kindLabel(k: string) {
  return kindOptions.find((o) => o.value === k)?.label || k
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// 摘要：取前 8 行非空行
function summarize(text: string) {
  return text.split('\n').filter((l) => l.trim()).slice(0, 8).join('\n')
}
</script>

<template>
  <div class="h-full flex flex-col gap-4 overflow-y-auto text-ink" v-loading="loading">
    <!-- 顶部：说明 + 筛选 -->
    <div class="bg-card border border-line rounded-card p-4 shrink-0">
      <div class="flex items-center justify-between gap-4 flex-wrap">
        <div>
          <div class="font-bold text-sm">记忆中心</div>
          <p class="text-xs text-ink-2 mt-1">
            全局记忆聚合：会话结束后自动沉淀的用户偏好 / 项目经验 / 经验技能，在此统一浏览与按项目筛选。
          </p>
        </div>
        <div class="flex items-center gap-3 flex-wrap">
          <el-select v-model="project" class="w-64" size="small">
            <el-option v-for="o in projectOptions" :key="o.value" :value="o.value" :label="o.label" />
          </el-select>
          <div class="flex gap-1">
            <el-button
              v-for="o in kindOptions"
              :key="o.value"
              size="small"
              :type="kind === o.value ? 'primary' : ''"
              :plain="kind !== o.value"
              @click="kind = o.value"
            >{{ o.label }}</el-button>
          </div>
        </div>
      </div>
    </div>

    <!-- 统计卡 -->
    <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 shrink-0">
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">经验技能</div>
        <div class="text-2xl font-bold">{{ stats.skillTotal }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">启用中</div>
        <div class="text-2xl font-bold text-green-600">{{ stats.skillEnabled }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">沉淀条目</div>
        <div class="text-2xl font-bold text-primary">{{ stats.entryTotal }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">涉及会话</div>
        <div class="text-2xl font-bold">{{ stats.sessionTotal }}</div>
      </div>
    </div>

    <!-- 主体：左时间线，右技能 + 画像/偏好摘要 -->
    <div class="flex gap-4 flex-1 min-h-0">
      <div class="flex-1 min-w-0 bg-card border border-line rounded-card p-4 overflow-y-auto">
        <div class="font-bold text-sm mb-3">沉淀时间线</div>
        <el-timeline v-if="filteredEntries.length">
          <el-timeline-item v-for="e in filteredEntries" :key="e.id" :timestamp="fmtTime(e.created_at)" placement="top">
            <div class="flex items-center gap-2 flex-wrap">
              <el-tag size="small" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
              <span class="text-sm font-bold">{{ e.target }}</span>
              <span v-if="e.source_session" class="text-[10px] text-ink-3 font-mono">{{ e.source_session }}</span>
            </div>
            <p class="text-xs text-ink-2 mt-1">{{ e.summary }}</p>
          </el-timeline-item>
        </el-timeline>
        <div v-else class="text-center text-sm text-ink-3 py-16">当前筛选下暂无沉淀条目。</div>
      </div>

      <div class="w-[380px] shrink-0 flex flex-col gap-4 overflow-y-auto">
        <div class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">经验技能</span>
            <router-link to="/skills" class="text-xs text-primary hover:underline">管理 →</router-link>
          </div>
          <div v-if="!filteredSkills.length" class="text-xs text-ink-3 py-2">暂无经验技能</div>
          <div v-for="s in filteredSkills" :key="s.name" class="p-2 bg-page rounded border border-line mb-1.5 text-xs">
            <div class="flex items-center gap-2">
              <span class="font-bold">{{ s.title }}</span>
              <el-tag size="small" effect="plain" :type="s.enabled ? 'success' : 'info'">
                {{ s.enabled ? '启用' : '禁用' }}
              </el-tag>
            </div>
            <p class="text-ink-2 mt-1 line-clamp-2">{{ s.when_to_use }}</p>
          </div>
        </div>

        <div class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">用户画像摘要</span>
            <router-link to="/profile" class="text-xs text-primary hover:underline">编辑 →</router-link>
          </div>
          <pre v-if="profile" class="text-xs text-ink-2 whitespace-pre-wrap break-all font-mono bg-page border border-line rounded p-2">{{ summarize(profile) }}</pre>
          <div v-else class="text-xs text-ink-3 py-2">暂无画像内容</div>
        </div>

        <div v-if="project" class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">项目偏好摘要</span>
            <router-link to="/projects" class="text-xs text-primary hover:underline">编辑 →</router-link>
          </div>
          <pre v-if="projectPref" class="text-xs text-ink-2 whitespace-pre-wrap break-all font-mono bg-page border border-line rounded p-2">{{ summarize(projectPref) }}</pre>
          <div v-else class="text-xs text-ink-3 py-2">该项目暂无偏好内容</div>
        </div>
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
