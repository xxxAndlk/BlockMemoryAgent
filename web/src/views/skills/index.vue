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
  type LearnedSkill,
  type EvolutionLogEntry,
} from '@/api/learned'

const activeTab = ref('skills')
const skills = ref<LearnedSkill[]>([])
const entries = ref<EvolutionLogEntry[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)

const editVisible = ref(false)
const editSaving = ref(false)
const editName = ref('')
const editTitle = ref('')
const editWhenToUse = ref('')
const editContent = ref('')

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
    // 内容含 frontmatter，编辑框只展示正文（frontmatter 由保存时重建）。
    const idx = res.content.indexOf('---\n\n')
    editContent.value = idx >= 0 ? res.content.slice(idx + 5) : res.content
  } catch (e) {
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
    default: return kind
  }
}

function kindType(kind: string) {
  switch (kind) {
    case 'skill_create': return 'success'
    case 'skill_update': return 'warning'
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
})
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-gray-300">
    <div class="mb-4">
      <h2 class="text-lg font-bold text-gray-200">技能库</h2>
      <p class="text-xs text-gray-500 mt-1">
        会话结束自动沉淀的跨项目工艺技能包；任务派发时按语义召回提示（只注一行），load_skill 取全文。
      </p>
    </div>

    <el-tabs v-model="activeTab">
      <el-tab-pane label="经验技能" name="skills">
        <div v-loading="loading" class="grid gap-3 md:grid-cols-2">
          <el-card v-for="s in skills" :key="s.name" class="!border-[#2a2d35] !bg-[#1a1d24]" shadow="never">
            <div class="flex items-start justify-between gap-3">
              <div class="min-w-0 flex-1">
                <div class="flex items-center gap-2 flex-wrap">
                  <span class="font-bold text-gray-200">{{ s.title }}</span>
                  <el-tag size="small" effect="plain" class="!bg-transparent !border-[#2a2d35] font-mono">{{ s.name }}</el-tag>
                  <el-tag size="small" :type="s.enabled ? 'success' : 'info'" effect="plain">
                    {{ s.enabled ? '启用' : '禁用' }}
                  </el-tag>
                  <el-tag v-if="s.outcome" size="small" :type="s.outcome === 'success' ? 'success' : 'danger'" effect="plain">
                    源自{{ s.outcome === 'success' ? '成功' : '失败' }}会话
                  </el-tag>
                </div>
                <p class="text-xs text-gray-400 mt-2 line-clamp-2">{{ s.when_to_use }}</p>
                <div class="mt-2 text-[11px] text-gray-600 flex gap-3">
                  <span>使用 {{ s.use_count }} 次</span>
                  <span>更新于 {{ fmtTime(s.updated_at) }}</span>
                </div>
              </div>
              <div class="flex flex-col items-end gap-2 shrink-0">
                <el-switch :model-value="s.enabled" :loading="mutating === s.name"
                           :disabled="mutating === s.name" @change="() => toggle(s)" />
                <el-button size="small" plain class="!bg-transparent !border-[#2a2d35] !text-gray-300" @click="openEdit(s)">
                  <el-icon class="mr-1"><EditPen /></el-icon> 编辑
                </el-button>
              </div>
            </div>
          </el-card>
        </div>
        <div v-if="!loading && !skills.length" class="text-center text-sm text-gray-500 py-16">
          暂无经验技能。有价值的会话（成功或失败）结束后会自动沉淀技能包。
        </div>
      </el-tab-pane>

      <el-tab-pane label="进化日志" name="log">
        <el-card class="!border-[#2a2d35] !bg-[#1a1d24]" shadow="never">
          <el-timeline v-if="entries.length">
            <el-timeline-item v-for="e in entries" :key="e.id" :timestamp="fmtTime(e.created_at)" placement="top">
              <div class="flex items-center gap-2 flex-wrap">
                <el-tag size="small" :type="kindType(e.kind)" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
                <span class="text-sm text-gray-300 font-bold">{{ e.target }}</span>
                <span v-if="e.source_session" class="text-[10px] text-gray-600 font-mono">{{ e.source_session }}</span>
              </div>
              <p class="text-xs text-gray-400 mt-1">{{ e.summary }}</p>
            </el-timeline-item>
          </el-timeline>
          <div v-else class="text-center text-sm text-gray-500 py-16">
            暂无进化记录。
          </div>
        </el-card>
      </el-tab-pane>
    </el-tabs>

    <el-dialog v-model="editVisible" :title="`编辑技能：${editName}`" width="640px" top="6vh">
      <div class="space-y-3">
        <div>
          <div class="text-xs text-gray-500 mb-1">标题</div>
          <el-input v-model="editTitle" spellcheck="false" />
        </div>
        <div>
          <div class="text-xs text-gray-500 mb-1">适用场景（when_to_use，参与语义召回）</div>
          <el-input v-model="editWhenToUse" type="textarea" :rows="2" spellcheck="false" />
        </div>
        <div>
          <div class="text-xs text-gray-500 mb-1">正文（步骤 / 坑点 / 验证）</div>
          <el-input v-model="editContent" type="textarea" :rows="14" spellcheck="false" class="skill-editor" />
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
  background: #0f1115;
  color: #d1d5db;
}
</style>
