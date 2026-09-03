<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'
import { getProjectPreferences, saveProjectPreferences } from '@/api/preferences'
import { useWorkDir } from '@/composables/useWorkDir'
import WorkDirPicker from '@/components/WorkDirPicker.vue'

const DEFAULT_DIR = '' // 空 work_dir = 后端默认目录
const DIRS_KEY = 'bma:workdirs'

const router = useRouter()
const { setWorkDir } = useWorkDir()

const sessions = ref<Session[]>([])
const loading = ref(false)

// 手工添加的目录（无会话也能出现在列表）
const customDirs = ref<string[]>(JSON.parse(localStorage.getItem(DIRS_KEY) || '[]'))
const newDir = ref('')

const selected = ref<string>(DEFAULT_DIR)

// 项目偏好编辑器状态
const prefContent = ref('')
const prefPath = ref('')
const prefLoading = ref(false)
const prefSaving = ref(false)
const prefDirty = ref(false)

interface DirGroup {
  dir: string
  total: number
  running: number
  lastActive: string
}

const groups = computed<DirGroup[]>(() => {
  const map = new Map<string, DirGroup>()
  for (const s of sessions.value) {
    const dir = s.work_dir || DEFAULT_DIR
    const g = map.get(dir) || { dir, total: 0, running: 0, lastActive: '' }
    g.total++
    if (s.status === 'running') g.running++
    if (s.started_at && s.started_at > g.lastActive) g.lastActive = s.started_at
    map.set(dir, g)
  }
  for (const d of customDirs.value) {
    if (!map.has(d)) map.set(d, { dir: d, total: 0, running: 0, lastActive: '' })
  }
  return [...map.values()].sort((a, b) => b.lastActive.localeCompare(a.lastActive))
})

function dirLabel(dir: string) {
  return dir || '默认目录'
}

async function load() {
  loading.value = true
  try {
    sessions.value = await listSessions()
  } catch (e) {
    ElMessage.error('会话列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

function addDir() {
  const d = newDir.value.trim()
  if (!d) return
  if (!customDirs.value.includes(d)) {
    customDirs.value = [...customDirs.value, d]
    localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
  }
  selected.value = d
  newDir.value = ''
}

function removeCustomDir(dir: string) {
  customDirs.value = customDirs.value.filter((d) => d !== dir)
  localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
  if (selected.value === dir) selected.value = DEFAULT_DIR
}

// 发起新会话：固化工作目录并跳入会话页（容器读取 ?work_dir= 预填）
function startSession(dir: string) {
  setWorkDir(dir)
  router.push({ path: '/session', query: dir ? { work_dir: dir } : {} })
}

function viewSessions(dir: string) {
  router.push({ path: '/history', query: dir ? { work_dir: dir } : {} })
}

async function loadPrefs() {
  prefLoading.value = true
  try {
    const res = await getProjectPreferences(selected.value || undefined)
    prefContent.value = res.content || ''
    prefPath.value = res.path || ''
    prefDirty.value = false
  } catch (e) {
    ElMessage.error('项目偏好加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    prefLoading.value = false
  }
}

async function savePrefs() {
  prefSaving.value = true
  try {
    await saveProjectPreferences(prefContent.value, selected.value || undefined)
    prefDirty.value = false
    ElMessage.success('项目偏好已保存')
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    prefSaving.value = false
  }
}

watch(selected, loadPrefs)
onMounted(async () => {
  await load()
  if (groups.value.length && !groups.value.some((g) => g.dir === selected.value)) {
    selected.value = groups.value[0].dir
  }
  await loadPrefs()
})

function fmtTime(iso: string) {
  return iso
    ? new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
    : '—'
}
</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-ink">
    <!-- 左：目录卡片列表 -->
    <div class="flex-1 flex flex-col min-w-0 gap-4 overflow-y-auto">
      <div class="bg-card border border-line rounded-card p-4 shrink-0">
        <div class="font-bold text-sm mb-1">工作目录</div>
        <p class="text-xs text-ink-2">按目录组织会话与项目偏好；新会话将以此目录作为 Agent 的工作根目录。</p>
        <div class="flex gap-2 mt-3">
          <div class="flex-1"><WorkDirPicker v-model="newDir" /></div>
          <el-button type="primary" :disabled="!newDir.trim()" @click="addDir">
            <el-icon class="mr-1"><FolderAdd /></el-icon>添加目录
          </el-button>
        </div>
      </div>

      <div v-loading="loading" class="grid gap-3 xl:grid-cols-2">
        <div
          v-for="g in groups"
          :key="g.dir"
          class="bg-card border rounded-card p-4 cursor-pointer transition-colors"
          :class="selected === g.dir ? 'border-primary shadow-card' : 'border-line hover:border-primary'"
          @click="selected = g.dir"
        >
          <div class="flex items-center gap-2 min-w-0">
            <el-icon class="text-primary shrink-0"><FolderOpened /></el-icon>
            <span class="font-mono text-sm font-bold truncate">{{ dirLabel(g.dir) }}</span>
            <el-tag v-if="g.running" size="small" type="warning" effect="plain" class="shrink-0">
              {{ g.running }} 运行中
            </el-tag>
          </div>
          <div class="flex gap-4 mt-2 text-xs text-ink-2">
            <span>会话 {{ g.total }}</span>
            <span>最近活跃 {{ fmtTime(g.lastActive) }}</span>
          </div>
          <div class="flex gap-2 mt-3">
            <el-button size="small" type="primary" plain @click.stop="startSession(g.dir)">
              <el-icon class="mr-1"><Promotion /></el-icon>发起新会话
            </el-button>
            <el-button size="small" plain @click.stop="viewSessions(g.dir)">查看会话</el-button>
            <el-button
              v-if="customDirs.includes(g.dir) && !g.total"
              size="small" plain type="danger"
              @click.stop="removeCustomDir(g.dir)"
            >移除</el-button>
          </div>
        </div>
        <div v-if="!loading && !groups.length" class="text-center text-sm text-ink-3 py-16 col-span-2">
          暂无工作目录，用上方选择器添加一个。
        </div>
      </div>
    </div>

    <!-- 右：选中目录的项目偏好（原 project-prefs 页逻辑） -->
    <div class="w-[420px] shrink-0 bg-card border border-line rounded-card flex flex-col overflow-hidden">
      <div class="px-4 py-3 border-b border-line flex items-center justify-between gap-2">
        <div class="min-w-0">
          <div class="font-bold text-sm">项目偏好</div>
          <div class="text-[11px] text-ink-3 font-mono truncate">{{ prefPath || dirLabel(selected) }}</div>
        </div>
        <div class="flex items-center gap-2 shrink-0">
          <el-tag v-if="prefDirty" size="small" type="warning" effect="plain">未保存</el-tag>
          <el-button size="small" plain :loading="prefLoading" @click="loadPrefs">
            <el-icon><Refresh /></el-icon>
          </el-button>
          <el-button size="small" type="primary" :loading="prefSaving" :disabled="!prefDirty" @click="savePrefs">
            保存
          </el-button>
        </div>
      </div>
      <div class="flex-1 min-h-0 p-3">
        <el-input
          v-model="prefContent"
          type="textarea"
          spellcheck="false"
          class="prefs-editor h-full"
          placeholder="「项目约定」人工维护；「项目经验」会话结束自动沉淀，注入每个 Agent 上下文。"
          @input="prefDirty = true"
        />
      </div>
      <div class="px-4 py-2 border-t border-line text-[11px] text-ink-3">
        改完即时生效（下次派发即注入）；自动沉淀的行带时间戳，人工行程序永不改写。
      </div>
    </div>
  </div>
</template>

<style scoped>
.prefs-editor :deep(.el-textarea__inner) {
  height: 100%;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.7;
  background: var(--bma-page);
  color: var(--bma-text);
}
</style>
