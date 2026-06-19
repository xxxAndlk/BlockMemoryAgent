<template>
  <div class="h-full flex gap-4 overflow-hidden">
    <!-- Left Column: Current Agent & Equipped Skills -->
    <div class="w-80 flex flex-col gap-4 overflow-y-auto">
      <el-card class="!border-dark-border !bg-dark-panel">
        <div class="text-sm">
          <div class="text-gray-400 mb-1">当前 Agent</div>
          <el-select v-model="selectedAgent" size="small" class="w-full">
            <el-option v-for="opt in agentOptions" :key="opt.value" :label="opt.label" :value="opt.value" />
          </el-select>
        </div>
      </el-card>

      <el-card class="!border-dark-border !bg-dark-panel flex-1">
        <template #header>
          <div class="font-bold text-sm">当前装配的 Skills</div>
        </template>

        <div v-if="loading" class="text-xs text-gray-500 text-center py-4">加载中...</div>
        <div v-else class="space-y-3">
          <div v-for="skill in equippedSkills" :key="skill.skill_id" class="p-3 bg-dark-bg rounded border border-dark-border relative group">
            <div class="flex justify-between items-start">
              <div>
                <div class="font-bold text-sm text-gray-200">{{ skill.name }}</div>
                <div class="text-xs text-gray-500 mt-1">Tool: <span class="text-blue-400">{{ skill.tool_ref }}</span></div>
                <div class="text-xs text-gray-500 mt-1">Cost: {{ skill.cost }}</div>
              </div>
            </div>
            <div class="text-xs text-gray-400 mt-2 pt-2 border-t border-dark-border">
              描述: {{ skill.description }}
            </div>
          </div>
          <div v-if="!equippedSkills.length" class="text-xs text-gray-500">未装配技能</div>
        </div>
      </el-card>
    </div>

    <!-- Right Column: Available Skills & History -->
    <div class="flex-1 flex flex-col gap-4 overflow-hidden">
      <!-- Available Skills -->
      <el-card class="!border-dark-border !bg-dark-panel flex-1 overflow-y-auto">
        <template #header>
          <div class="flex justify-between items-center">
            <div class="font-bold text-sm">技能候选池</div>
            <div class="flex gap-2">
              <el-input v-model="searchSkill" size="small" placeholder="搜索技能..." class="w-48">
                <template #prefix><el-icon><Search /></el-icon></template>
              </el-input>
            </div>
          </div>
        </template>

        <div v-if="loading" class="text-xs text-gray-500 text-center py-4">加载中...</div>
        <div v-else class="grid grid-cols-3 gap-4">
          <div v-for="skill in availableSkills" :key="skill.skill_id" class="p-4 bg-dark-bg rounded border border-dark-border hover:border-primary transition-colors flex flex-col">
            <div class="font-bold text-sm text-gray-200 mb-1">{{ skill.name }}</div>
            <div class="text-xs text-gray-500 mb-1">Tool: <span class="text-blue-400">{{ skill.tool_ref }}</span></div>
            <div class="text-xs text-gray-500 mb-4">Cost: {{ skill.cost }}</div>

            <div class="mt-auto">
              <el-button type="primary" size="small" class="w-full !bg-primary/20 !text-primary !border-primary hover:!bg-primary hover:!text-white transition-colors">
                <el-icon class="mr-1"><Plus /></el-icon> 装配
              </el-button>
            </div>
          </div>
          <div v-if="!availableSkills.length" class="col-span-3 text-xs text-gray-500 text-center py-4">无可用技能</div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { listSkills, getAgentSkills, type Skill } from '@/api/session'
import type { AgentNode } from '@/types'

const props = defineProps<{
  agents: AgentNode[]
}>()

const searchSkill = ref('')
const allSkills = ref<Skill[]>([])
const equippedSkillIds = ref<Set<string>>(new Set())
const selectedAgent = ref('')
const loading = ref(false)

const agentOptions = computed(() => props.agents.map(a => ({ label: a.name, value: a.inst_id })))

watch(() => props.agents, (agents) => {
  if (agents.length && !selectedAgent.value) {
    selectedAgent.value = agents[0].inst_id
  }
}, { immediate: true })

watch(selectedAgent, () => loadAgentSkills())

async function load() {
  loading.value = true
  try {
    const res = await listSkills()
    allSkills.value = res.skills || []
  } catch {
    allSkills.value = []
  }
  await loadAgentSkills()
  loading.value = false
}

async function loadAgentSkills() {
  if (!selectedAgent.value) {
    equippedSkillIds.value = new Set()
    return
  }
  try {
    const res = await getAgentSkills(selectedAgent.value)
    const ids = new Set<string>()
    res.skillset?.skills?.forEach((s: Skill) => ids.add(s.skill_id))
    equippedSkillIds.value = ids
  } catch {
    equippedSkillIds.value = new Set()
  }
}

const equippedSkills = computed(() => allSkills.value.filter(s => equippedSkillIds.value.has(s.skill_id)))
const availableSkills = computed(() => {
  const q = searchSkill.value.trim().toLowerCase()
  return allSkills.value.filter(s => {
    if (equippedSkillIds.value.has(s.skill_id)) return false
    if (!q) return true
    return s.name.toLowerCase().includes(q) || s.description.toLowerCase().includes(q) || s.skill_id.toLowerCase().includes(q)
  })
})

load()
</script>

<style scoped>
.body-flex-1 {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
  padding: 16px;
}

:deep(.search-input .el-input__wrapper) {
  box-shadow: none !important;
  border: 1px solid #2a2d35;
}
:deep(.search-input .el-input__wrapper.is-focus) {
  border-color: var(--el-color-primary);
}
</style>
