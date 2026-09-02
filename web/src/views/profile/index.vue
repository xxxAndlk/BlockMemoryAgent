<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { getProfile, saveProfile } from '@/api/preferences'

const content = ref('')
const path = ref('')
const loading = ref(false)
const saving = ref(false)
const dirty = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await getProfile()
    content.value = res.content || ''
    path.value = res.path || ''
    dirty.value = false
  } catch (e) {
    ElMessage.error('用户画像加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    await saveProfile(content.value)
    dirty.value = false
    ElMessage.success('用户画像已保存')
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
        <h2 class="text-lg font-bold text-gray-200">用户画像</h2>
        <p class="text-xs text-gray-500 mt-1">
          跨项目的稳定偏好档案，会话结束自动沉淀（自动行带时间戳），人工行（无时间戳）程序永不改写。
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
        class="profile-editor"
        @input="onInput"
      />
      <div class="mt-3 text-[11px] text-gray-600">
        提示：自动沉淀的行带「（YYYY-MM-DD HH:MM）」时间戳；手动编辑请写无时间戳的行，合并整理时会被保护。
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.profile-editor :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.7;
  background: #0f1115;
  color: #d1d5db;
}
</style>
