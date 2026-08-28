<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listPlugins, enablePlugin, disablePlugin, reloadPlugins, type PluginInfo } from '@/api/plugins'

const plugins = ref<PluginInfo[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)

async function load() {
  loading.value = true
  try {
    const res = await listPlugins()
    plugins.value = res.plugins || []
  } catch (e) {
    ElMessage.error('插件列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// 热启用/停用：ReAct 每轮重读工具 Schema，下一轮即生效（无需重启）。
async function toggle(p: PluginInfo) {
  mutating.value = p.id
  try {
    const updated = p.enabled ? await disablePlugin(p.id) : await enablePlugin(p.id)
    const idx = plugins.value.findIndex(x => x.id === p.id)
    if (idx >= 0) plugins.value[idx] = updated
    ElMessage.success(`插件 ${p.id} 已${updated.enabled ? '启用' : '停用'}，下一轮对话生效`)
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
    load()
  } finally {
    mutating.value = null
  }
}

async function handleReload() {
  loading.value = true
  try {
    const res = await reloadPlugins()
    plugins.value = res.plugins || []
    ElMessage.success('插件配置已重载')
  } catch (e) {
    ElMessage.error('重载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

function stateType(p: PluginInfo) {
  if (!p.enabled) return 'info'
  if (p.state === 'error' || p.last_error) return 'danger'
  if (p.state === 'running' || p.state === 'enabled') return 'success'
  return 'warning'
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-gray-300">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-gray-200">插件管理</h2>
        <p class="text-xs text-gray-500 mt-1">热插拔 MCP / bundle 插件；启用/停用下一轮对话生效，无需重启服务。</p>
      </div>
      <el-button plain class="!bg-transparent !border-[#2a2d35] !text-gray-300" :loading="loading" @click="handleReload">
        <el-icon class="mr-1"><Refresh /></el-icon> 重载配置
      </el-button>
    </div>

    <div class="grid gap-3 md:grid-cols-2">
      <el-card v-for="p in plugins" :key="p.id" class="!border-[#2a2d35] !bg-[#1a1d24]">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2 flex-wrap">
              <span class="font-bold text-gray-200">{{ p.name || p.id }}</span>
              <el-tag size="small" effect="plain" class="!bg-transparent !border-[#2a2d35]">{{ p.kind }}</el-tag>
              <el-tag size="small" :type="stateType(p)" effect="plain">{{ p.enabled ? (p.state || 'enabled') : 'disabled' }}</el-tag>
              <span v-if="p.version" class="text-[10px] text-gray-600">v{{ p.version }}</span>
            </div>
            <p v-if="p.description" class="text-xs text-gray-400 mt-2 line-clamp-2">{{ p.description }}</p>
            <div v-if="p.tools?.length" class="mt-2 flex flex-wrap gap-1">
              <el-tag v-for="t in p.tools" :key="t" size="small" effect="plain"
                      class="!bg-[#0f1115] !border-[#2a2d35] !text-gray-400 font-mono">{{ t }}</el-tag>
            </div>
            <div class="mt-2 space-y-1 text-[11px]">
              <div v-if="p.roles?.length" class="text-gray-500">
                可见角色：<span class="text-gray-400">{{ p.roles.join('、') }}</span>
              </div>
              <div v-if="p.url" class="text-gray-500 truncate">
                入口：<a :href="p.url" target="_blank" rel="noreferrer" class="text-blue-400 hover:underline">{{ p.url }}</a>
              </div>
              <div v-if="p.missing_env?.length" class="text-orange-400">
                缺失环境变量：{{ p.missing_env.join('、') }}
              </div>
              <div v-if="p.last_error" class="text-red-400 break-all">最近错误：{{ p.last_error }}</div>
            </div>
          </div>
          <el-switch :model-value="p.enabled" :loading="mutating === p.id"
                     :disabled="mutating === p.id"
                     @change="() => toggle(p)" />
        </div>
      </el-card>
    </div>

    <div v-if="!loading && !plugins.length" class="text-center text-sm text-gray-500 py-16">
      暂无已注册插件。外部插件包放入 config/plugins.d/ 后点击「重载配置」。
    </div>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
