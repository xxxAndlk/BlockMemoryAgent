<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getProjectPreferences, saveProjectPreferences } from '@/api/preferences'
import { useWorkDir } from '@/composables/useWorkDir'

const content = ref('')
const path = ref('')
const loading = ref(false)
const saving = ref(false)
const dirty = ref(false)

// 与 chat 页同源的"记住的 workDir"（localStorage bma:last-workdir）：有则按该目录解析项目偏好。
const { workDir } = useWorkDir()

async function load() {
  loading.value = true
  try {
    const res = await getProjectPreferences(workDir.value || undefined)
    content.value = res.content || ''
    path.value = res.path || ''
    dirty.value = false
  } catch (e) {
    ElMessage.error('项目偏好加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    await saveProjectPreferences(content.value, workDir.value || undefined)
    dirty.value = false
    ElMessage.success('项目偏好已保存')
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

function onInput() {
  dirty.value = true
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-gray-300">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-gray-200">项目偏好</h2>
        <p class="text-xs text-gray-500 mt-1">
          当前 workDir 的 .bma/project_preferences.md：「项目约定」人工维护，「项目经验」会话结束自动沉淀，注入每个 Agent 上下文。
        </p>
      </div>
      <div class="flex items-center gap-2">
        <el-tag v-if="dirty" size="small" type="warning" effect="plain">未保存</el-tag>
        <el-button plain class="!bg-transparent !border-[#2a2d35] !text-gray-300" :loading="loading" @click="load">
          <el-icon class="mr-1"><Refresh /></el-icon> 刷新
        </el-button>
        <el-button type="primary" :loading="saving" :disabled="!dirty" @click="save">
          <el-icon class="mr-1"><Check /></el-icon> 保存
        </el-button>
      </div>
    </div>

    <el-card class="!border-[#2a2d35] !bg-[#1a1d24]" shadow="never">
      <p v-if="path" class="text-[11px] text-gray-600 mb-2 font-mono break-all">{{ path }}</p>
      <el-input
        v-model="content"
        type="textarea"
        :rows="24"
        spellcheck="false"
        class="prefs-editor"
        @input="onInput"
      />
      <div class="mt-3 text-[11px] text-gray-600">
        提示：改完即时生效（下次派发即注入）；自动沉淀的行带时间戳，人工行程序永不改写。
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.prefs-editor :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.7;
  background: #0f1115;
  color: #d1d5db;
}
</style>
