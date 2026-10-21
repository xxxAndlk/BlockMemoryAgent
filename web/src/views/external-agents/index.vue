<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listExternalAgents, reloadExternalAgents, type ExternalAgentInfo } from '@/api/external_agents'

const agents = ref<ExternalAgentInfo[]>([])
const loading = ref(false)

const hasUninstalled = computed(() => agents.value.some(a => !a.installed))

function commandPreview(a: ExternalAgentInfo): string {
  const args = (a.args || []).join(' ')
  return args ? `${a.command} ${args}` : a.command
}

async function load() {
  loading.value = true
  try {
    const res = await listExternalAgents()
    agents.value = res.agents || []
  } catch (e) {
    ElMessage.error('外部 Agent 列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function handleReload() {
  loading.value = true
  try {
    const res = await reloadExternalAgents()
    agents.value = res.agents || []
    ElMessage.success('外部 Agent 配置已重载')
  } catch (e) {
    ElMessage.error('重载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-ink">外部 Agent</h2>
        <p class="text-xs text-ink-2 mt-1">以子进程方式调用宿主机已安装的外部 Agent CLI；MetaAgent 经 CallExternalAgent 工具投递任务。</p>
      </div>
      <el-button plain class="!bg-transparent !border-line !text-ink" :loading="loading" @click="handleReload">
        <el-icon class="mr-1"><Refresh /></el-icon> 重新加载
      </el-button>
    </div>

    <div v-if="hasUninstalled"
         class="mb-4 rounded border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-[11px] text-amber-600 dark:text-amber-400">
      存在未安装的命令：对应 Agent 在 PATH 中找不到可执行文件，调用会直接被拒绝。
    </div>

    <div class="grid gap-4 md:grid-cols-2 items-stretch">
      <el-card v-for="a in agents" :key="a.id" class="!border-line !bg-card ext-card" shadow="never">
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1 flex items-center gap-2 flex-wrap">
            <span class="font-bold text-ink truncate">{{ a.id }}</span>
            <el-tag size="small" :type="a.enabled ? 'success' : 'info'" effect="plain">
              {{ a.enabled ? '已启用' : '已停用' }}
            </el-tag>
            <el-tag size="small" :type="a.installed ? 'success' : 'danger'" effect="plain">
              {{ a.installed ? '已安装' : '未安装' }}
            </el-tag>
          </div>
        </div>

        <p v-if="a.description" class="text-xs text-ink-2 mt-2 line-clamp-2 min-h-[2em]">{{ a.description }}</p>

        <!-- 命令行全文预览：command + args -->
        <div class="mt-3 rounded bg-page border border-line px-2.5 py-2 font-mono text-[11px] text-ink-2 break-all">
          {{ commandPreview(a) }}
        </div>

        <div class="mt-3 pt-2 border-t border-line text-[11px] text-ink-2">
          超时：{{ a.timeout_sec }}s
        </div>
      </el-card>
    </div>

    <div v-if="!loading && !agents.length" class="text-center text-sm text-ink-2 py-16">
      <p>尚未配置任何外部 Agent。</p>
      <p class="mt-2">编辑 <code class="font-mono text-ink">config/external_agents.yaml</code> 添加 agents 条目后，点击右上角「重新加载」即可生效。</p>
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

/* 卡片 hover：与插件页同款 */
.ext-card {
  transition: box-shadow 0.15s ease, border-color 0.15s ease;
}
.ext-card:hover {
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.08);
  border-color: var(--bma-primary, #409eff) !important;
}
</style>
