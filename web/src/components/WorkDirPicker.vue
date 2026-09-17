<script setup lang="ts">
// WorkDirPicker.vue 工作目录选择器：行内只读展示 + 「选择目录」下拉。
// 首屏就是"已有工作目录"清单（会话用过的目录，由调用方聚合去重）——多数场景是复用旧目录，
// 浏览文件系统是备选；弹窗保留面包屑/手输路径/已有目录三种定位方式。
// 契约仍是 v-model（modelValue/update:modelValue），emit 只在"选定"时发生一次——
// 此前行内是实时输入框，逐字符 emit，调用方若是"改即保存"会把半截路径写进会话。
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { browseFS, pickSystemDir, type BrowseResult } from '@/api/fs'
import { dirBaseName, sameDir } from '@/utils/dir'

const props = defineProps<{
  modelValue: string
  /** 已有工作目录（会话用过的目录，多会话共用同目录的快捷入口，由调用方聚合去重） */
  existing?: string[]
  /** 空值时行内展示的占位文案（如「进程默认目录」） */
  placeholder?: string
}>()
const emit = defineEmits<{ 'update:modelValue': [string] }>()

const open = ref(false)
const loading = ref(false)
const current = ref('') // 当前浏览到的目录（绝对路径）
const parent = ref('') // 上级目录（根目录为空 → 禁用「上级」）
const dirs = ref<BrowseResult['dirs']>([])
const pick = ref('') // 单击选中的子目录（优先于"当前目录"）
const manual = ref('') // 手输/粘贴的路径
const error = ref('')

/** 行内展示值：绑定值优先，否则占位文案（不写死「默认」二字，由调用方定语义）。 */
const display = computed(() => props.modelValue || props.placeholder || '默认目录')
const hasValue = computed(() => !!props.modelValue)

/** 将被选定的目录：单击的子目录优先，否则当前所在目录。 */
const effective = computed(() => pick.value || current.value)

/**
 * 面包屑分段：把当前路径拆成可点击的祖先链，兼容 Windows 盘符（C:\a\b）与 POSIX（/a/b）。
 * 每段携带其绝对路径，点击即 load() 该层。
 */
const crumbs = computed<{ label: string; path: string }[]>(() => {
  const p = current.value
  if (!p) return []
  const isWin = /^[a-zA-Z]:/.test(p)
  const parts = p.split(/[\\/]+/).filter(Boolean)
  const out: { label: string; path: string }[] = []
  let acc = ''
  parts.forEach((seg, i) => {
    if (i === 0) {
      acc = isWin ? seg + '\\' : '/' + seg
    } else {
      acc = acc.endsWith('\\') || acc.endsWith('/') ? acc + seg : acc + (isWin ? '\\' : '/') + seg
    }
    out.push({ label: seg, path: acc })
  })
  return out
})

async function load(p: string) {
  loading.value = true
  error.value = ''
  try {
    const r = await browseFS(p)
    current.value = r.path
    parent.value = r.parent
    dirs.value = r.dirs
    pick.value = ''
    manual.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    ElMessage.error('打开目录失败：' + error.value)
  } finally {
    loading.value = false
  }
}

const picking = ref(false)

/**
 * 「更改」：优先调**系统原生目录选择框**（在服务主机上弹出，即用户眼前的资源管理器式选择器），
 * 拿不到（平台不支持/超时/已有窗口打开）时才退回网页版选择器——原生是主路径，网页版是兜底。
 */
async function chooseDir() {
  if (picking.value) return
  picking.value = true
  try {
    const r = await pickSystemDir()
    if (r.path) {
      emit('update:modelValue', r.path)
      return
    }
    // path 为空 = 用户在原生的框里点了取消：不改变现值，也不再弹第二个框。
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e)
    // 501/504/409：原生不可用 → 退回网页版选择器（并说明原因，用户知道发生了什么）。
    error.value = ''
    open.value = true
    ElMessage.warning('系统选择器不可用，已切换为网页版选择（' + msg + '）')
  } finally {
    picking.value = false
  }
}

function openDialog() {
  error.value = ''
  open.value = true
}

// 打开时定位到：绑定值 → 上次浏览位置（都不行则交给后端回默认根，如盘符列表）。
watch(open, (v) => {
  if (v) load(props.modelValue || current.value || '')
})

/** 手输路径：直接尝试打开该目录（非法路径就地红字提示，不再是一行 console 里的报错）。 */
function goManual() {
  const t = manual.value.trim()
  if (!t) return
  void load(t)
}

function confirm() {
  const target = effective.value
  if (!target) return
  emit('update:modelValue', target)
  open.value = false
}

/** 清除本会话目录 → 回落进程默认目录（后端空串语义）。 */
function useDefault() {
  emit('update:modelValue', '')
  open.value = false
}

/** 快捷选中已有目录：直接选定，不再要求逐级进入。 */
function pickExisting(d: string) {
  emit('update:modelValue', d)
  open.value = false
}

/** 下拉命令：已有目录直接选定；其余三种入口复用旧路径（原生选择器 / 网页版弹窗 / 清空）。 */
function onCommand(cmd: { kind: string; dir?: string }) {
  if (cmd.kind === 'dir' && cmd.dir) return pickExisting(cmd.dir)
  if (cmd.kind === 'browse') return void chooseDir()
  if (cmd.kind === 'manual') return openDialog()
  if (cmd.kind === 'default') return useDefault()
}
</script>

<template>
  <div class="workdir-picker flex items-center gap-2 min-w-0">
    <span class="font-mono text-xs truncate min-w-0 flex-1 text-ink-2"
          :class="hasValue ? '' : 'text-ink-3'"
          :title="modelValue || display">{{ display }}</span>
    <!-- 主入口：已有目录下拉（复用旧目录是常态），浏览/手输收在菜单里 -->
    <el-dropdown trigger="click" class="shrink-0" @command="onCommand">
      <el-button size="small" type="primary" plain :loading="picking" class="shrink-0">
        {{ picking ? '选择中…' : '选择目录' }}<el-icon class="ml-1"><ArrowDown /></el-icon>
      </el-button>
      <template #dropdown>
        <el-dropdown-menu class="workdir-menu">
          <el-dropdown-item v-for="d in existing || []" :key="d"
                            :command="{ kind: 'dir', dir: d }" :title="d">
            <span class="dd-item">
              <el-icon v-if="sameDir(d, modelValue)" class="text-primary shrink-0"><Check /></el-icon>
              <span class="dd-name" :class="sameDir(d, modelValue) ? 'text-primary font-bold' : ''">{{ dirBaseName(d) }}</span>
              <span class="dd-path">{{ d }}</span>
            </span>
          </el-dropdown-item>
          <el-dropdown-item :command="{ kind: 'browse' }" :divided="!!existing?.length">
            <el-icon><FolderOpened /></el-icon>浏览其它目录…
          </el-dropdown-item>
          <el-dropdown-item :command="{ kind: 'manual' }">
            <el-icon><EditPen /></el-icon>手动输入路径…
          </el-dropdown-item>
          <el-dropdown-item v-if="hasValue" :command="{ kind: 'default' }">
            <el-icon><Refresh /></el-icon>使用默认目录
          </el-dropdown-item>
        </el-dropdown-menu>
      </template>
    </el-dropdown>

    <el-dialog v-model="open" title="选择工作目录（网页版）" width="560px" append-to-body>
      <!-- 面包屑：任意祖先层可点击直达 -->
      <div class="crumbs" :title="current">
        <template v-for="(c, i) in crumbs" :key="c.path">
          <span v-if="i > 0" class="sep">›</span>
          <button class="crumb" @click="load(c.path)">{{ c.label }}</button>
        </template>
        <span v-if="!crumbs.length" class="text-ink-3 text-xs">选择盘符 / 根目录</span>
        <el-icon v-if="loading" class="ml-2 animate-spin text-ink-3"><Loading /></el-icon>
      </div>

      <!-- 手输 / 粘贴路径 -->
      <div class="manual">
        <el-input v-model="manual" size="small" placeholder="或直接输入/粘贴完整路径"
                  @keyup.enter="goManual">
          <template #append><el-button size="small" @click="goManual">转到</el-button></template>
        </el-input>
      </div>

      <!-- 已有工作目录：多会话共用同目录的主路径 -->
      <div v-if="existing?.length" class="recent">
        <div class="recent-title">已有工作目录</div>
        <div class="recent-list">
          <button v-for="d in existing" :key="d" class="recent-item font-mono" :title="d"
                  @click="pickExisting(d)">{{ d }}</button>
        </div>
      </div>

      <!-- 子目录列表：单击选中、双击进入 -->
      <el-scrollbar max-height="300px">
        <div v-for="d in dirs" :key="d.path" class="dir-item" :class="{ active: pick === d.path }"
             :title="d.path" @dblclick="load(d.path)" @click="pick = d.path">{{ d.name }}</div>
        <el-empty v-if="!dirs.length && !loading" description="无子目录" :image-size="40" />
      </el-scrollbar>

      <template #footer>
        <div class="footer">
          <el-button v-if="hasValue" size="small" text class="!text-ink-3" @click="useDefault">
            使用默认目录
          </el-button>
          <span class="flex-1"></span>
          <el-button @click="open = false">取消</el-button>
          <el-button type="primary" :disabled="!effective" :title="effective" @click="confirm">
            选定{{ effective ? '：' + effective : '' }}
          </el-button>
        </div>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.crumbs {
  display: flex;
  align-items: center;
  gap: 2px;
  overflow-x: auto;
  white-space: nowrap;
  margin-bottom: 8px;
  padding-bottom: 2px;
  font-size: 12px;
}
.crumb {
  color: var(--el-color-primary);
  background: none;
  border: none;
  cursor: pointer;
  padding: 1px 3px;
  border-radius: 3px;
}
.crumb:hover { background: var(--el-fill-color-light); }
.sep { color: var(--el-text-color-placeholder); }
.manual { margin-bottom: 8px; }
.recent { margin-bottom: 8px; }
.recent-title { font-size: 11px; color: var(--el-text-color-secondary); margin-bottom: 4px; }
.recent-list { display: flex; flex-wrap: wrap; gap: 4px; }
.recent-item {
  font-size: 11px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  padding: 2px 6px;
  border: 1px solid var(--el-border-color);
  border-radius: 4px;
  background: none;
  cursor: pointer;
}
.recent-item:hover { border-color: var(--el-color-primary); color: var(--el-color-primary); }
/* 下拉项：目录名 + 灰色全路径（同一目录名的不同盘符要能分辨） */
.workdir-menu :deep(.el-dropdown-menu__item) {
  padding: 0;
}
.dd-item {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  max-width: 380px;
  padding: 5px 12px;
  min-width: 0;
}
.dd-name { flex-shrink: 0; }
.dd-path {
  color: var(--el-text-color-placeholder);
  font-size: 11px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.dir-item { padding: 4px 8px; cursor: pointer; border-radius: 4px; }
.dir-item:hover { background: var(--el-fill-color-light); }
.dir-item.active { background: var(--el-color-primary-light-8); }
.footer { display: flex; align-items: center; }
</style>
