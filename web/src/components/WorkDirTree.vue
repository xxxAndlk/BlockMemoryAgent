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
const OPEN_KEY = 'bma:openWorkdirs'
const TREE_OPEN_KEY = 'bma:treeOpen'

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
const showAllDirs = ref<string[]>([]) // 展开「显示更多」的目录（normDir 键）
// 树默认全收起（用户定调：点「会话」列出目录，点目录才展开其中的会话），展开态 localStorage 记忆。
const openDirs = ref<string[]>(JSON.parse(localStorage.getItem(OPEN_KEY) || '[]'))
const treeOpen = ref(localStorage.getItem(TREE_OPEN_KEY) === '1')
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
  return showAllDirs.value.includes(normDir(g.dir)) ? g.sessions : g.sessions.slice(0, SESSIONS_PER_DIR)
}

function isShowAll(g: DirGroup) {
  return showAllDirs.value.includes(normDir(g.dir))
}

function toggleMore(g: DirGroup) {
  const key = normDir(g.dir)
  showAllDirs.value = isShowAll(g)
    ? showAllDirs.value.filter((k) => k !== key)
    : [...showAllDirs.value, key]
}

function isOpen(g: DirGroup) {
  return openDirs.value.includes(normDir(g.dir))
}

/** 点目录行：整组会话收起/展开（状态随 localStorage 记忆）。 */
function toggleCollapse(g: DirGroup) {
  const key = normDir(g.dir)
  openDirs.value = isOpen(g)
    ? openDirs.value.filter((k) => k !== key)
    : [...openDirs.value, key]
  localStorage.setItem(OPEN_KEY, JSON.stringify(openDirs.value))
}

/** 点「会话」标题行：整棵树（目录清单）收起/展开。 */
function toggleTree() {
  treeOpen.value = !treeOpen.value
  localStorage.setItem(TREE_OPEN_KEY, treeOpen.value ? '1' : '0')
}

function openSession(s: SessionSummary) {
  router.push({ path: '/session', query: { id: s.id } })
}

/**
 * 起新会话：`?new=1` 是会话页的「清空重来」信令——不带这个标记时，会话页对无 id 的
 * 入口一律回落到"恢复上次会话"，用户在会话页点「新建会话」看起来毫无反应（同路由
 * 仅 query 变化不会重新挂载组件）。
 */
function startSession(dir: string) {
  if (dir) setWorkDir(dir)
  router.push({ path: '/session', query: dir ? { work_dir: dir, new: '1' } : { new: '1' } })
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

/** 该目录下是否有当前打开的会话（目录行给底色用）。 */
function containsActive(g: DirGroup) {
  return !!activeSessionId.value && g.sessions.some((s) => s.id === activeSessionId.value)
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
  <!-- 高度随内容（不再 flex-1 撑满）：目录少时菜单紧跟其后，不做出一大片空白；
       内容超一屏时收缩（flex-shrink 默认）→ 列表内滚动。 -->
  <div class="flex flex-col min-h-0" :class="treeOpen ? 'pt-1' : 'shrink-0'">
    <!-- 左内边距与目录行/会话行对齐（三者箭头同一条竖线），右侧留给图标按钮 -->
    <div class="flex items-center justify-between mx-2 pl-3 pr-1 pt-1 pb-1 shrink-0">
      <!-- 标题行即总开关：整行左段可点，hover 有底色（此前只有小字本身可点，看不出能点） -->
      <div class="flex items-center gap-1.5 min-w-0 flex-1 self-stretch py-1.5 rounded-card cursor-pointer select-none hover:bg-page transition-colors"
           title="展开/收起会话目录" @click="toggleTree">
        <el-icon class="text-ink-3 shrink-0 text-xs transition-transform" :class="treeOpen ? 'rotate-90' : ''"><ArrowRight /></el-icon>
        <span class="text-xs font-medium" :class="treeOpen ? 'text-ink-2' : 'text-ink-3'">会话</span>
        <span v-if="!treeOpen && groups.length" class="text-[10px] text-ink-3 shrink-0">{{ groups.length }}</span>
      </div>
      <div class="flex items-center gap-1 shrink-0">
        <el-button v-if="loading" link size="small" disabled><el-icon class="animate-spin"><Loading /></el-icon></el-button>
        <el-button link size="small" title="刷新会话列表" @click="load"><el-icon><Refresh /></el-icon></el-button>
        <el-button link size="small" title="新建会话" @click="startSession('')"><el-icon><Plus /></el-icon></el-button>
        <el-button link size="small" title="添加工作目录" @click="addOpen = true"><el-icon><FolderAdd /></el-icon></el-button>
      </div>
    </div>

    <div v-show="treeOpen" class="overflow-y-auto min-h-0 pb-2">
      <div v-if="!groups.length" class="px-3 py-2 text-xs text-ink-3">暂无会话</div>

      <div v-for="g in groups" :key="g.dir">
        <!-- 目录行：单击整组收起/展开（箭头指示）；目录配置走右侧 ⋯。
             含当前打开会话的目录给一层底色，收起状态下也能一眼定位。 -->
        <div class="group/dir flex items-center gap-1.5 px-3 py-1.5 rounded-card mx-2 transition-colors cursor-pointer select-none"
             :class="containsActive(g) ? 'bg-page' : 'hover:bg-page'"
             @click="toggleCollapse(g)">
          <el-icon class="text-ink-3 shrink-0 text-xs transition-transform"
                   :class="isOpen(g) ? 'rotate-90' : ''"><ArrowRight /></el-icon>
          <el-icon class="text-ink-2 shrink-0 text-sm">
            <Folder v-if="!isOpen(g)" /><FolderOpened v-else />
          </el-icon>
          <span class="text-xs font-bold truncate min-w-0 flex-1" :title="g.dir || '默认目录'">{{ g.label }}</span>
          <el-tag v-if="g.running" size="small" type="warning" effect="plain" class="shrink-0 !px-1 !h-4 text-[10px]">{{ g.running }}</el-tag>
          <span v-else-if="g.total" class="text-[10px] text-ink-3 shrink-0 tabular-nums">{{ g.total }}</span>

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

        <!-- 会话行：最多 5 条 + 显示更多（整组收起时全隐）。
             左侧竖线把子项挂到父目录上（缩进之外的层级线索），线随缩进对齐目录箭头。 -->
        <template v-if="isOpen(g)">
        <div class="ml-[26px] mr-2 border-l border-line">
          <div
            v-for="s in visibleSessions(g)"
            :key="s.id"
            class="flex items-center gap-2 pl-3 pr-2 py-1 rounded-card cursor-pointer transition-colors"
            :class="s.id === activeSessionId ? 'bg-[var(--bma-primary-soft)] text-[var(--bma-primary)]' : 'hover:bg-page'"
            :title="s.goal"
            @click="openSession(s)"
          >
            <span class="w-1.5 h-1.5 rounded-full shrink-0" :class="statusDotClass(s.status)"></span>
            <span class="text-xs truncate min-w-0 flex-1">{{ s.goal || '(无目标)' }}</span>
            <span class="text-[10px] text-ink-3 shrink-0 tabular-nums" :title="statusText(s.status)">{{ relTime(s.started_at) }}</span>
          </div>

          <div
            v-if="g.sessions.length > SESSIONS_PER_DIR"
            class="pl-3 pr-2 py-1 text-[11px] text-ink-3 cursor-pointer hover:text-primary"
            @click="toggleMore(g)"
          >
            {{ isShowAll(g) ? '收起' : `显示更多 (${g.sessions.length - SESSIONS_PER_DIR})` }}
          </div>
        </div>
        </template>
      </div>
    </div>

    <!-- 添加工作目录 -->
    <el-dialog v-model="addOpen" title="添加工作目录" width="480px" append-to-body>
      <div class="text-xs text-ink-2 mb-3">新会话将以该目录作为 Agent 的工作根目录；目录会常驻侧栏。</div>
      <WorkDirPicker v-model="newDir" placeholder="选择目录（已有目录 / 浏览 / 手输）"
                     :existing="groups.map((g) => g.dir).filter(Boolean)" />
      <template #footer>
        <el-button @click="addOpen = false">取消</el-button>
        <el-button type="primary" :disabled="!newDir.trim()" @click="addDir">添加并新建会话</el-button>
      </template>
    </el-dialog>

    <WorkDirDrawer v-model:dir="drawerDir" />
  </div>
</template>
