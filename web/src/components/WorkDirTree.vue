<script setup lang="ts">
// 侧栏目录树（Codex 式）：以工作目录为分组展示会话，默认每目录只展开最近 5 条。
// 「工作目录」与「会话」此前分散在两个导航页（/projects 管目录、/history 翻历史），
// 合并进常驻侧栏后，一屏内完成"选目录 → 开旧会话 / 起新会话 / 改目录配置"。
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import type { SessionSummary } from '@/types'
import { listSessions } from '@/api/session'
import { useWorkDir } from '@/composables/useWorkDir'
import { normDir } from '@/utils/dir'
import { statusDotClass, statusText } from '@/utils/sessionStatus'
import WorkDirPicker from '@/components/WorkDirPicker.vue'
import WorkDirDrawer from '@/components/WorkDirDrawer.vue'

const SESSIONS_PER_DIR = 5
// 会话总条数与后端上限（listSessionsMaxLimit=1000）对齐：目录树要覆盖全部会话，
// 不传 limit 会被后端默认截断 200 条，目录分组会凭空少一截。
const SESSION_LIST_LIMIT = 1000

const DEFAULT_DIR = '' // 空 work_dir = 后端默认目录
const DIRS_KEY = 'bma:workdirs'

type DirGroup = {
  dir: string
  label: string
  total: number
  running: number
  lastActive: string
  sessions: SessionSummary[]
}

const route = useRoute()
const router = useRouter()
const { setWorkDir } = useWorkDir()

const sessions = ref<SessionSummary[]>([])
const loading = ref(false)
const expandedDirs = ref<string[]>([]) // 展开「显示更多」的目录（normDir 键）
const customDirs = ref<string[]>(JSON.parse(localStorage.getItem(DIRS_KEY) || '[]'))

// 添加目录弹窗
const addOpen = ref(false)
const newDir = ref('')

// 目录设置抽屉（项目偏好 / 测试助手）
const drawerDir = ref<string | null>(null)

const activeSessionId = computed(() => (route.query.id as string) || '')

/** 目录名：取路径末段（Codex 侧栏同样只显示项目名），完整路径放 title。 */
function dirLabel(dir: string) {
  if (!dir) return '默认目录'
  const parts = dir.split(/[\\/]+/).filter(Boolean)
  return parts[parts.length - 1] || dir
}

const groups = computed<DirGroup[]>(() => {
  const map = new Map<string, DirGroup>()
  for (const s of sessions.value) {
    const dir = s.work_dir || DEFAULT_DIR
    // 归一化键：同址不同写法（尾斜杠/盘符大小写）必须落同一组。
    const key = normDir(dir)
    let g = map.get(key)
    if (!g) {
      g = { dir, label: dirLabel(dir), total: 0, running: 0, lastActive: '', sessions: [] }
      map.set(key, g)
    }
    g.total++
    if (s.status === 'running') g.running++
    if (s.started_at && s.started_at > g.lastActive) g.lastActive = s.started_at
    g.sessions.push(s)
  }
  // 手工添加的目录即使暂无会话也要出现在树里（否则"加了目录却看不见"）。
  for (const d of customDirs.value) {
    const key = normDir(d)
    if (!map.has(key)) {
      map.set(key, { dir: d, label: dirLabel(d), total: 0, running: 0, lastActive: '', sessions: [] })
    }
  }
  for (const g of map.values()) {
    g.sessions.sort((a, b) => (b.started_at || '').localeCompare(a.started_at || ''))
  }
  return [...map.values()].sort((a, b) => b.lastActive.localeCompare(a.lastActive))
})

async function load() {
  loading.value = true
  try {
    sessions.value = (await listSessions(SESSION_LIST_LIMIT)) || []
  } catch {
    // 侧栏加载失败不弹错（后端重启期间每次路由切换都会响）；保留上次结果，由用户手动刷新。
  } finally {
    loading.value = false
  }
}

onMounted(load)
// 路由变化即刷新：新建/删除会话、改了会话目录后切页，树的归属跟着变。
watch(() => route.fullPath, load)

function visibleSessions(g: DirGroup) {
  return expandedDirs.value.includes(normDir(g.dir)) ? g.sessions : g.sessions.slice(0, SESSIONS_PER_DIR)
}

function isExpanded(g: DirGroup) {
  return expandedDirs.value.includes(normDir(g.dir))
}

function toggleMore(g: DirGroup) {
  const key = normDir(g.dir)
  expandedDirs.value = isExpanded(g)
    ? expandedDirs.value.filter((k) => k !== key)
    : [...expandedDirs.value, key]
}

function openSession(s: SessionSummary) {
  router.push({ path: '/session', query: { id: s.id } })
}

/** 在该目录下发新会话：固化目录并进会话页（`?work_dir=` 由会话页预填，不带 id）。 */
function startSession(dir: string) {
  setWorkDir(dir)
  router.push({ path: '/session', query: dir ? { work_dir: dir } : {} })
}

function viewAll(dir: string) {
  router.push({ path: '/history', query: dir ? { work_dir: dir } : {} })
}

function removeDir(dir: string) {
  customDirs.value = customDirs.value.filter((d) => normDir(d) !== normDir(dir))
  localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
}

function addDir() {
  const d = newDir.value.trim()
  if (!d) return
  if (!customDirs.value.some((x) => normDir(x) === normDir(d))) {
    customDirs.value = [...customDirs.value, d]
    localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
  }
  newDir.value = ''
  addOpen.value = false
  startSession(d)
}

/** ⋯ 菜单：value 为动作名，el-dropdown 只回传字符串，目录从当前行闭包带入。 */
function onDirCommand(cmd: string, g: DirGroup) {
  switch (cmd) {
    case 'new': return startSession(g.dir)
    case 'all': return viewAll(g.dir)
    case 'prefs': drawerDir.value = g.dir; return
    case 'remove': return removeDir(g.dir)
  }
}

function isCustom(g: DirGroup) {
  return customDirs.value.some((d) => normDir(d) === normDir(g.dir))
}

function relTime(iso: string) {
  if (!iso) return ''
  const diff = Date.now() - new Date(iso).getTime()
  if (!Number.isFinite(diff) || diff < 0) return ''
  const min = Math.floor(diff / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min}m`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}
</script>

<template>
  <div class="flex flex-col min-h-0">
    <div class="flex items-center justify-between px-3 pt-1 pb-2 shrink-0">
      <span class="text-xs text-ink-3">会话</span>
      <div class="flex items-center gap-1">
        <el-button v-if="loading" link size="small" disabled><el-icon class="animate-spin"><Loading /></el-icon></el-button>
        <el-button link size="small" title="刷新会话列表" @click="load"><el-icon><Refresh /></el-icon></el-button>
        <el-button link size="small" title="新建会话" @click="startSession('')"><el-icon><Plus /></el-icon></el-button>
        <el-button link size="small" title="添加工作目录" @click="addOpen = true"><el-icon><FolderAdd /></el-icon></el-button>
      </div>
    </div>

    <div class="overflow-y-auto min-h-0 pb-2">
      <div v-if="!groups.length" class="px-3 py-2 text-xs text-ink-3">暂无会话</div>

      <div v-for="g in groups" :key="g.dir" class="mb-1">
        <!-- 目录行：单击展开/收起目录配置由此进 ⋯；双击无特殊语义 -->
        <div class="group/dir flex items-center gap-1.5 px-3 py-1.5 rounded-card mx-2 hover:bg-page transition-colors">
          <el-icon class="text-ink-2 shrink-0 text-sm"><FolderOpened /></el-icon>
          <span class="text-xs font-bold truncate min-w-0 flex-1" :title="g.dir || '默认目录'">{{ g.label }}</span>
          <el-tag v-if="g.running" size="small" type="warning" effect="plain" class="shrink-0 !px-1 !h-4 text-[10px]">{{ g.running }}</el-tag>
          <span v-else-if="g.total" class="text-[10px] text-ink-3 shrink-0">{{ g.total }}</span>

          <el-dropdown trigger="click" @command="(c: string) => onDirCommand(c, g)">
            <el-icon class="shrink-0 text-ink-3 cursor-pointer opacity-0 group-hover/dir:opacity-100 hover:text-primary transition-opacity" @click.stop>
              <MoreFilled />
            </el-icon>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="new"><el-icon><Promotion /></el-icon>在此目录新建会话</el-dropdown-item>
                <el-dropdown-item command="all"><el-icon><Clock /></el-icon>查看全部会话</el-dropdown-item>
                <el-dropdown-item command="prefs" divided><el-icon><Setting /></el-icon>项目偏好与测试助手</el-dropdown-item>
                <el-dropdown-item v-if="isCustom(g)" command="remove"><el-icon><Delete /></el-icon>移除目录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>

        <!-- 会话行：最多 5 条 + 显示更多 -->
        <div
          v-for="s in visibleSessions(g)"
          :key="s.id"
          class="flex items-center gap-2 pl-7 pr-3 py-1 mx-2 rounded-card cursor-pointer transition-colors"
          :class="s.id === activeSessionId ? 'bg-[var(--bma-primary-soft)] text-[var(--bma-primary)]' : 'hover:bg-page'"
          :title="s.goal"
          @click="openSession(s)"
        >
          <span class="w-1.5 h-1.5 rounded-full shrink-0" :class="statusDotClass(s.status)"></span>
          <span class="text-xs truncate min-w-0 flex-1">{{ s.goal || '(无目标)' }}</span>
          <span class="text-[10px] text-ink-3 shrink-0" :title="statusText(s.status)">{{ relTime(s.started_at) }}</span>
        </div>

        <div
          v-if="g.sessions.length > SESSIONS_PER_DIR"
          class="pl-7 pr-3 py-1 mx-2 text-[11px] text-ink-3 cursor-pointer hover:text-primary"
          @click="toggleMore(g)"
        >
          {{ isExpanded(g) ? '收起' : `显示更多 (${g.sessions.length - SESSIONS_PER_DIR})` }}
        </div>
      </div>
    </div>

    <!-- 添加工作目录 -->
    <el-dialog v-model="addOpen" title="添加工作目录" width="480px" append-to-body>
      <div class="text-xs text-ink-2 mb-3">新会话将以该目录作为 Agent 的工作根目录；目录会常驻侧栏。</div>
      <WorkDirPicker v-model="newDir" placeholder="选择目录（浏览或手输路径）" />
      <template #footer>
        <el-button @click="addOpen = false">取消</el-button>
        <el-button type="primary" :disabled="!newDir.trim()" @click="addDir">添加并新建会话</el-button>
      </template>
    </el-dialog>

    <WorkDirDrawer v-model:dir="drawerDir" />
  </div>
</template>
