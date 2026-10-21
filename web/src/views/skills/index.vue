<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listLearnedSkills,
  getLearnedSkill,
  saveLearnedSkill,
  enableLearnedSkill,
  disableLearnedSkill,
  deleteSkill,
  reforgeSkill,
  listEvolutionLog,
  consolidateSkills,
  type LearnedSkill,
  type SkillTool,
  type EvolutionLogEntry,
} from '@/api/learned'
import { listSkills } from '@/api/skills'
import type { Skill } from '@/types'

const activeTab = ref('skills')
const skills = ref<LearnedSkill[]>([])
const entries = ref<EvolutionLogEntry[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)

const builtinSkills = ref<Skill[]>([])
const builtinLoading = ref(false)

const consolidating = ref(false)

// 多选批量删除：selected 存技能 name（技能名唯一）；列表刷新后按现存项修剪。
const selected = ref<Set<string>>(new Set())
const batchDeleting = ref(false)
const selectedNames = computed(() => skills.value.filter(s => selected.value.has(s.name)).map(s => s.name))
const selectedCount = computed(() => selectedNames.value.length)
const allSelected = computed(() => skills.value.length > 0 && selectedCount.value === skills.value.length)
const someSelected = computed(() => selectedCount.value > 0 && !allSelected.value)

function toggleSelect(name: string, checked: boolean) {
  const next = new Set(selected.value)
  if (checked) next.add(name)
  else next.delete(name)
  selected.value = next
}

function toggleSelectAll(checked: boolean) {
  selected.value = checked ? new Set(skills.value.map(s => s.name)) : new Set()
}

function clearSelection() {
  selected.value = new Set()
}

// 批量删除：沿用单条 DELETE 端点并发提交（每批 4 个），逐条成功即从列表与选中集移除，
// 全部结束后重拉列表校准；失败项保留选中并提示，避免"看起来删了其实没删"。
async function removeSelected() {
  const names = selectedNames.value.slice()
  if (!names.length) return
  batchDeleting.value = true
  const failed: string[] = []
  let deleted = 0
  try {
    for (let i = 0; i < names.length; i += 4) {
      const chunk = names.slice(i, i + 4)
      const results = await Promise.allSettled(chunk.map(n => deleteSkill(n)))
      results.forEach((r, idx) => {
        if (r.status === 'fulfilled') deleted++
        else failed.push(chunk[idx])
      })
      const gone = new Set(chunk.filter((_, idx) => results[idx].status === 'fulfilled'))
      skills.value = skills.value.filter(s => !gone.has(s.name))
      const next = new Set(selected.value)
      gone.forEach(n => next.delete(n))
      selected.value = next
    }
    if (failed.length) {
      ElMessage.error(`已删除 ${deleted} 个，${failed.length} 个失败：${failed.slice(0, 3).join('、')}${failed.length > 3 ? ' 等' : ''}`)
    } else {
      ElMessage.success(`已删除 ${deleted} 个技能`)
    }
    loadSkills()
  } finally {
    batchDeleting.value = false
  }
}

// 手动触发技能库整理（合并语义重复技能 + 归档零使用技能）；轻量模型调用，可能耗时 1-2 分钟。
async function runConsolidate() {
  consolidating.value = true
  try {
    const res = await consolidateSkills()
    ElMessage.success(res.summary || '整理完成')
    await Promise.all([loadSkills(), loadLog()])
  } catch (e) {
    ElMessage.error('整理失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    consolidating.value = false
  }
}

async function loadBuiltin() {
  builtinLoading.value = true
  try {
    const res = await listSkills()
    builtinSkills.value = res.skills || []
  } catch (e) {
    ElMessage.error('内置技能加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    builtinLoading.value = false
  }
}

const editVisible = ref(false)
const editSaving = ref(false)
const editName = ref('')
const editTitle = ref('')
const editWhenToUse = ref('')
const editContent = ref('')
/** 编辑弹窗「配套脚本」清单（从详情 content 的 frontmatter 解析；可移除，保存时随 PUT 整体替换提交）。 */
const editTools = ref<SkillTool[]>([])

// 宽容解析 SKILL.md frontmatter 的 tools 清单（仅认后端 RenderSkillFrontmatter 产出的
// 固定 YAML 形状：`- path:` / `desc:` / `run:` 二级缩进；解析失败返回空清单，不影响编辑）。
function parseSkillTools(raw: string): SkillTool[] {
  if (!raw.startsWith('---')) return []
  const end = raw.indexOf('\n---', 4)
  if (end < 0) return []
  const tools: SkillTool[] = []
  let cur: SkillTool | null = null
  let inTools = false
  for (const line of raw.slice(4, end).split('\n')) {
    if (!inTools) {
      if (/^tools:\s*$/.test(line)) inTools = true
      continue
    }
    const item = line.match(/^\s+- path:\s*(.+?)\s*$/)
    if (item) {
      cur = { path: item[1], desc: '' }
      tools.push(cur)
      continue
    }
    if (!cur) {
      if (/^\S/.test(line)) inTools = false
      continue
    }
    const desc = line.match(/^\s+desc:\s*(.+?)\s*$/)
    if (desc) { cur.desc = desc[1]; continue }
    const run = line.match(/^\s+run:\s*(.+?)\s*$/)
    if (run) { cur.run = run[1]; continue }
    if (/^\S/.test(line)) { inTools = false; cur = null }
  }
  return tools.filter(t => t.path)
}

async function loadSkills() {
  loading.value = true
  try {
    const res = await listLearnedSkills()
    skills.value = res.skills || []
    // 修剪选中集：已被删除/消失的技能不再算选中。
    if (selected.value.size) {
      const alive = new Set(skills.value.map(s => s.name))
      const next = new Set([...selected.value].filter(n => alive.has(n)))
      if (next.size !== selected.value.size) selected.value = next
    }
  } catch (e) {
    ElMessage.error('技能库加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function loadLog() {
  loading.value = true
  try {
    const res = await listEvolutionLog()
    entries.value = res.entries || []
  } catch (e) {
    ElMessage.error('进化日志加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// 启用/禁用：即时生效于 list_skills 可见性与向量召回。
async function toggle(s: LearnedSkill) {
  mutating.value = s.name
  try {
    if (s.enabled) {
      await disableLearnedSkill(s.name)
      s.enabled = false
      ElMessage.success(`技能 ${s.name} 已禁用`)
    } else {
      await enableLearnedSkill(s.name)
      s.enabled = true
      ElMessage.success(`技能 ${s.name} 已启用`)
    }
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
    loadSkills()
  } finally {
    mutating.value = null
  }
}

// 手动删除（物理删）：PG 行 + 技能文件 + 退池。成功后立即从列表移除（不等重新拉取），再后台校准。
async function removeSkill(s: LearnedSkill) {
  mutating.value = s.name
  try {
    await deleteSkill(s.name)
    skills.value = skills.value.filter(x => x.name !== s.name)
    if (selected.value.has(s.name)) {
      const next = new Set(selected.value)
      next.delete(s.name)
      selected.value = next
    }
    ElMessage.success(`技能 ${s.name} 已删除`)
    loadSkills()
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    mutating.value = null
  }
}

// 重铸（decision 模型按当前产出契约重写正文）：结构门/评分不过时后端 422，错误即理由，旧内容保留。
async function reforge(s: LearnedSkill) {
  mutating.value = s.name
  try {
    const res = await reforgeSkill(s.name)
    ElMessage.success(res.summary || `技能 ${s.name} 已重铸`)
    loadSkills()
    loadLog()
  } catch (e) {
    ElMessage.error('重铸失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    mutating.value = null
  }
}

// 最近使用人话格式化：空=从未；1 个月内相对天数，之后短日期。
function fmtLastUsed(t?: string) {
  if (!t) return '从未'
  const then = new Date(t).getTime()
  if (Number.isNaN(then)) return ''
  const days = Math.floor((Date.now() - then) / 86400000)
  if (days <= 0) return '今天'
  if (days === 1) return '昨天'
  if (days < 30) return `${days} 天前`
  return new Date(t).toLocaleDateString('zh-CN')
}

async function openEdit(s: LearnedSkill) {
  editName.value = s.name
  editTitle.value = s.title
  editWhenToUse.value = s.when_to_use
  editContent.value = ''
  editVisible.value = true
  try {
    const res = await getLearnedSkill(s.name)
    // 配套脚本清单先于 frontmatter 剥离解析。
    editTools.value = parseSkillTools(res.content)
    // 内容含 frontmatter，编辑框只展示正文（frontmatter 由保存时重建）。
    const idx = res.content.indexOf('---\n\n')
    editContent.value = idx >= 0 ? res.content.slice(idx + 5) : res.content
  } catch (e) {
    editTools.value = []
    ElMessage.error('技能详情加载失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function saveEdit() {
  editSaving.value = true
  try {
    await saveLearnedSkill(editName.value, {
      title: editTitle.value,
      when_to_use: editWhenToUse.value,
      content: editContent.value,
      // C4：配套脚本清单随保存整体提交（后端校验 path 并同步删除已移除的 scripts/ 文件）。
      tools: editTools.value.map(t => ({ path: t.path, desc: t.desc, run: t.run })),
    })
    editVisible.value = false
    ElMessage.success('技能已更新')
    loadSkills()
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    editSaving.value = false
  }
}

function kindLabel(kind: string) {
  switch (kind) {
    case 'user_pref': return '用户偏好'
    case 'project_lesson': return '项目经验'
    case 'skill_create': return '技能新建'
    case 'skill_update': return '技能更新'
    case 'skill_dropped': return '技能未沉淀'  // 技能库满，建新被写入门拒绝
    case 'skill_merge': return '技能合并'
    case 'skill_archive': return '技能归档'
    case 'skill_evict': return '冷淘汰'    // 满库确定性淘汰：最冷的 enabled 技能归档腾位
    case 'skill_delete': return '技能删除'  // 手动删除（物理删）
    case 'skill_reforge': return '技能重铸'  // decision 模型按契约重写正文
    case 'consolidate_run': return '整理记录'
    default: return kind
  }
}

function kindType(kind: string) {
  switch (kind) {
    case 'skill_create': return 'success'
    case 'skill_update': return 'warning'
    case 'skill_merge': return 'warning'
    case 'skill_dropped': return 'danger'
    case 'skill_archive': return 'info'
    case 'skill_evict': return 'warning'
    case 'skill_delete': return 'danger'
    case 'skill_reforge': return 'warning'
    case 'consolidate_run': return 'success'
    case 'user_pref': return ''
    case 'project_lesson': return 'info'
    default: return 'info'
  }
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

onMounted(() => {
  loadSkills()
  loadLog()
  loadBuiltin()
})
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="mb-4 flex items-start justify-between gap-4">
      <div>
        <h2 class="text-lg font-bold text-ink">技能库</h2>
        <p class="text-xs text-ink-2 mt-1">
          会话结束自动沉淀的跨项目工艺技能包；任务派发时按语义召回提示（只注一行），load_skill 取全文。
          技能过多会稀释选择，启用数达阈值后每日自动整理（合并重复 / 归档零使用）。
        </p>
      </div>
      <el-button plain class="!bg-transparent !border-line !text-ink shrink-0"
                 :loading="consolidating" @click="runConsolidate">
        <el-icon class="mr-1"><MagicStick /></el-icon> 立即整理
      </el-button>
    </div>

    <el-tabs v-model="activeTab">
      <el-tab-pane label="经验技能" name="skills">
        <!-- 多选工具条：勾选卡片后批量删除（沿用单条 DELETE 端点并发提交）。 -->
        <div class="mb-3 flex items-center gap-3 flex-wrap">
          <el-checkbox :model-value="allSelected" :indeterminate="someSelected" :disabled="!skills.length"
                       @change="(v: string | number | boolean) => toggleSelectAll(!!v)">
            全选
          </el-checkbox>
          <span v-if="selectedCount" class="text-xs text-ink-2">已选 {{ selectedCount }} 项</span>
          <el-popconfirm :title="`确定删除选中的 ${selectedCount} 个技能？删除后不可恢复（PG 元数据 + 技能文件一并删除）`"
                         confirm-button-text="批量删除" cancel-button-text="取消" width="300"
                         @confirm="removeSelected">
            <template #reference>
              <el-button v-if="selectedCount" size="small" plain type="danger" class="!bg-transparent"
                         :loading="batchDeleting">
                <el-icon class="mr-1"><Delete /></el-icon> 批量删除
              </el-button>
            </template>
          </el-popconfirm>
          <el-button v-if="selectedCount" size="small" text :disabled="batchDeleting" @click="clearSelection">
            清空选择
          </el-button>
        </div>
        <div v-loading="loading" class="grid gap-3 md:grid-cols-2">
          <el-card v-for="s in skills" :key="s.name" class="!border-line !bg-card" shadow="never">
            <div class="flex items-start justify-between gap-3">
              <el-checkbox class="mt-1 shrink-0" :model-value="selected.has(s.name)" :disabled="batchDeleting"
                           @change="(v: string | number | boolean) => toggleSelect(s.name, !!v)" />
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2 flex-wrap">
                  <span class="font-bold text-ink">{{ s.title }}</span>
                  <el-tag size="small" effect="plain" class="!bg-transparent !border-line font-mono">{{ s.name }}</el-tag>
                  <el-tag size="small" :type="s.enabled ? 'success' : 'info'" effect="plain">
                    {{ s.enabled ? '启用' : '禁用' }}
                  </el-tag>
                  <el-tag v-if="s.outcome" size="small" :type="s.outcome === 'success' ? 'success' : 'danger'" effect="plain">
                    源自{{ s.outcome === 'success' ? '成功' : '失败' }}会话
                  </el-tag>
                  <el-tag v-if="s.has_tools" size="small" type="warning" effect="plain"
                          title="该技能携带可执行脚本" class="!cursor-help">
                    ⚙ 带工具
                  </el-tag>
                </div>
                <p class="text-xs text-ink-2 mt-2 line-clamp-2">{{ s.when_to_use }}</p>
                <div class="mt-2 text-[11px] text-ink-3 flex gap-3">
                  <span>使用 {{ s.use_count }} 次</span>
                  <span>最近使用 {{ fmtLastUsed(s.last_used_at) }}</span>
                  <span>更新于 {{ fmtTime(s.updated_at) }}</span>
                </div>
              </div>
              <div class="flex flex-col items-end gap-2 shrink-0">
                <el-switch :model-value="s.enabled" :loading="mutating === s.name"
                           :disabled="mutating === s.name" @change="() => toggle(s)" />
                <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="openEdit(s)">
                  <el-icon class="mr-1"><EditPen /></el-icon> 编辑
                </el-button>
                <el-popconfirm title="用 decision 模型按当前契约重写该技能正文？（重写不过质量门时保留旧内容；耗时约 1 分钟）"
                               confirm-button-text="重铸" cancel-button-text="取消"
                               width="280" @confirm="reforge(s)">
                  <template #reference>
                    <el-button size="small" plain class="!bg-transparent !border-line !text-ink"
                               :loading="mutating === s.name">
                      <el-icon class="mr-1"><MagicStick /></el-icon> 重铸
                    </el-button>
                  </template>
                </el-popconfirm>
                <el-popconfirm :title="s.enabled ? `确定删除技能 ${s.name}？` : `技能 ${s.name} 已禁用，删除后不可恢复，确定删除？`"
                               confirm-button-text="删除" cancel-button-text="取消"
                               width="260" @confirm="removeSkill(s)">
                  <template #reference>
                    <el-button size="small" plain type="danger" class="!bg-transparent" :loading="mutating === s.name">
                      <el-icon class="mr-1"><Delete /></el-icon> 删除
                    </el-button>
                  </template>
                </el-popconfirm>
              </div>
            </div>
          </el-card>
        </div>
        <div v-if="!loading && !skills.length" class="text-center text-sm text-ink-2 py-16">
          暂无经验技能。有价值的会话（成功或失败）结束后会自动沉淀技能包。
        </div>
      </el-tab-pane>

      <el-tab-pane label="内置技能" name="builtin">
        <div v-loading="builtinLoading" class="grid gap-3 md:grid-cols-2">
          <el-card v-for="s in builtinSkills" :key="s.skill_id" shadow="never">
            <div class="flex items-center gap-2 flex-wrap">
              <span class="font-bold text-ink">{{ s.name }}</span>
              <el-tag size="small" effect="plain">{{ s.domain || '通用' }}</el-tag>
              <el-tag v-if="s.cost" size="small" type="warning" effect="plain">cost {{ s.cost }}</el-tag>
            </div>
            <p class="text-xs text-ink-2 mt-2 line-clamp-2">{{ s.description }}</p>
            <div v-if="s.tool_ref" class="mt-2 text-[11px] text-ink-3 font-mono break-all">工具：{{ s.tool_ref }}</div>
            <div v-if="s.tags?.length" class="mt-2 flex flex-wrap gap-1">
              <el-tag v-for="t in s.tags" :key="t" size="small" effect="plain" class="!text-ink-2">{{ t }}</el-tag>
            </div>
          </el-card>
        </div>
        <div v-if="!builtinLoading && !builtinSkills.length" class="text-center text-sm text-ink-3 py-16">
          暂无内置技能（config/skills.yaml 与插件包注入）。
        </div>
      </el-tab-pane>

      <el-tab-pane label="进化日志" name="log">
        <el-card class="!border-line !bg-card" shadow="never">
          <el-timeline v-if="entries.length">
            <el-timeline-item v-for="e in entries" :key="e.id" :timestamp="fmtTime(e.created_at)" placement="top">
              <div class="flex items-center gap-2 flex-wrap">
                <el-tag size="small" :type="kindType(e.kind)" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
                <span class="text-sm text-ink font-bold">{{ e.target }}</span>
                <span v-if="e.source_session" class="text-[10px] text-ink-3 font-mono">{{ e.source_session }}</span>
              </div>
              <p class="text-xs text-ink-2 mt-1">{{ e.summary }}</p>
            </el-timeline-item>
          </el-timeline>
          <div v-else class="text-center text-sm text-ink-2 py-16">
            暂无进化记录。
          </div>
        </el-card>
      </el-tab-pane>
    </el-tabs>

    <el-dialog v-model="editVisible" :title="`编辑技能：${editName}`" width="640px" top="6vh">
      <div class="space-y-3">
        <div>
          <div class="text-xs text-ink-2 mb-1">标题</div>
          <el-input v-model="editTitle" spellcheck="false" />
        </div>
        <div>
          <div class="text-xs text-ink-2 mb-1">适用场景（when_to_use，参与语义召回）</div>
          <el-input v-model="editWhenToUse" type="textarea" :rows="2" spellcheck="false" />
        </div>
        <div>
          <div class="text-xs text-ink-2 mb-1">正文（步骤 / 坑点 / 验证）</div>
          <el-input v-model="editContent" type="textarea" :rows="14" spellcheck="false" class="skill-editor" />
        </div>
        <div v-if="editTools.length">
          <div class="text-xs text-ink-2 mb-1">配套脚本（{{ editTools.length }}）</div>
          <div class="rounded-md border border-line bg-page">
            <div v-for="t in editTools" :key="t.path"
                 class="flex items-center gap-2 px-3 py-2 text-xs border-b border-line last:border-b-0">
              <span class="font-mono text-primary shrink-0">{{ t.path }}</span>
              <span v-if="t.desc" class="text-ink-2 shrink-0 max-w-[40%] truncate" :title="t.desc">{{ t.desc }}</span>
              <span v-if="t.run" class="text-ink-3 truncate flex-1 text-right" :title="t.run">{{ t.run }}</span>
              <el-popconfirm title="从该技能中移除此脚本？（scripts/ 下对应文件将一并删除）"
                             confirm-button-text="移除" cancel-button-text="取消"
                             width="260" @confirm="editTools = editTools.filter(x => x.path !== t.path)">
                <template #reference>
                  <el-button size="small" text type="danger" class="!px-1 shrink-0">移除</el-button>
                </template>
              </el-popconfirm>
            </div>
          </div>
          <p class="text-[11px] text-ink-3 mt-1">
            脚本由会话沉淀自动生成；移除脚本在点击「保存」后生效。新增脚本请放入
            config/skills_learned/{{ editName }}/scripts/ 后重新编辑本技能登记。
          </p>
        </div>
      </div>
      <template #footer>
        <el-button @click="editVisible = false">取消</el-button>
        <el-button type="primary" :loading="editSaving" @click="saveEdit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.skill-editor :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.7;
  background: var(--bma-page);
  color: var(--bma-text);
}
</style>
