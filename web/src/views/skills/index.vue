<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listLearnedSkills,
  getLearnedSkill,
  saveLearnedSkill,
  enableLearnedSkill,
  disableLearnedSkill,
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
/** 编辑弹窗「配套脚本」只读清单（从详情 content 的 frontmatter 解析；后端 PUT 不接收 tools 变更）。 */
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
        <div v-loading="loading" class="grid gap-3 md:grid-cols-2">
          <el-card v-for="s in skills" :key="s.name" class="!border-line !bg-card" shadow="never">
            <div class="flex items-start justify-between gap-3">
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
                  <span>更新于 {{ fmtTime(s.updated_at) }}</span>
                </div>
              </div>
              <div class="flex flex-col items-end gap-2 shrink-0">
                <el-switch :model-value="s.enabled" :loading="mutating === s.name"
                           :disabled="mutating === s.name" @change="() => toggle(s)" />
                <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="openEdit(s)">
                  <el-icon class="mr-1"><EditPen /></el-icon> 编辑
                </el-button>
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
