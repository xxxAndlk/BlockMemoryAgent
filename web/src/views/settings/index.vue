<template>
  <div class="p-6 h-full overflow-y-auto text-gray-300">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-gray-200">模型切换</h2>
        <p class="text-xs text-gray-500 mt-1">
          按角色切换 LLM 模型（预设来自 roles.yaml model_presets）。切换含连通性探测（最长 60 秒），
          domain/叶子角色下一次派发生效、轻量/审计类下一次调用生效、meta 角色新会话生效；进行中任务不中断。
        </p>
      </div>
      <el-button plain class="!bg-transparent !border-[#2a2d35] !text-gray-300" :loading="loading" @click="load">
        <el-icon class="mr-1"><Refresh /></el-icon> 刷新
      </el-button>
    </div>

    <el-table
      :data="roles"
      v-loading="loading"
      class="!bg-[#1a1d24]"
      header-cell-class="!bg-[#14161c] !text-gray-400"
      cell-class="!bg-[#1a1d24]"
      style="width: 100%"
    >
      <el-table-column prop="role_id" label="角色" width="180" class-name="font-mono" />
      <el-table-column label="当前模型" min-width="260">
        <template #default="{ row }">
          <div class="flex items-center gap-2">
            <span class="font-mono text-gray-300">{{ row.provider }}/{{ row.model }}</span>
            <el-tag v-if="row.overridden" size="small" type="warning" effect="plain">
              覆写: {{ row.preset_id }}
            </el-tag>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="切换到预设" min-width="220">
        <template #default="{ row }">
          <el-select
            v-model="selections[row.role_id]"
            placeholder="选择预设"
            size="default"
            class="w-full"
            :disabled="switching === row.role_id"
          >
            <el-option
              v-for="p in presets"
              :key="p.id"
              :value="p.id"
              :label="`${p.name || p.id} (${p.provider}/${p.model})`"
            />
          </el-select>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="140">
        <template #default="{ row }">
          <el-button
            type="primary"
            plain
            size="small"
            :loading="switching === row.role_id"
            :disabled="!selections[row.role_id] || switching === row.role_id"
            @click="apply(row)"
          >
            切换
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <p v-if="!loading && !roles.length" class="text-center text-sm text-gray-500 py-16">
      暂无可切换角色。请在 roles.yaml 配置角色与 model_presets 后重启服务。
    </p>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listModels, switchModel, type ModelPreset, type RoleModelStatus } from '@/api/models'

const roles = ref<RoleModelStatus[]>([])
const presets = ref<ModelPreset[]>([])
const loading = ref(false)
const switching = ref<string | null>(null)
const selections = reactive<Record<string, string>>({})

async function load() {
  loading.value = true
  try {
    const res = await listModels()
    roles.value = res.roles || []
    presets.value = res.presets || []
    for (const r of roles.value) {
      selections[r.role_id] = r.overridden && r.preset_id ? r.preset_id : ''
    }
  } catch (e) {
    ElMessage.error('模型目录加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function apply(row: RoleModelStatus) {
  const preset = selections[row.role_id]
  if (!preset) return
  switching.value = row.role_id
  try {
    const res = await switchModel(row.role_id, preset)
    ElMessage.success(`${row.role_id} 已切换到 ${res.model}（探测通过）`)
    await load()
  } catch (e) {
    ElMessage.error('切换失败（保持原模型）：' + (e instanceof Error ? e.message : String(e)))
    await load()
  } finally {
    switching.value = null
  }
}

onMounted(load)
</script>
