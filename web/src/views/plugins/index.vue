<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { listPlugins, enablePlugin, disablePlugin, reloadPlugins, type PluginInfo } from '@/api/plugins'

const plugins = ref<PluginInfo[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)

// 工具折叠：每卡独立展开状态（默认收起，最多展示 TOOL_COLLAPSE_LIMIT 个 chip）。
const TOOL_COLLAPSE_LIMIT = 12
const expanded = reactive<Record<string, boolean>>({})

function isExpanded(p: PluginInfo) {
  return !!expanded[p.id]
}
function toggleTools(p: PluginInfo) {
  expanded[p.id] = !expanded[p.id]
}
function visibleTools(p: PluginInfo) {
  const tools = p.tools || []
  if (isExpanded(p) || tools.length <= TOOL_COLLAPSE_LIMIT) return tools
  return tools.slice(0, TOOL_COLLAPSE_LIMIT)
}

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

// 形态徽标约定：PluginInfo 无 transport 字段，按 id/kind 判定（spec §06）。
function transportBadge(p: PluginInfo): { label: string; type: 'warning' | 'info' | '' } | null {
  if (p.id === 'host_computer_use') return { label: '宿主直控', type: 'warning' }
  if (p.id === 'computer_use') return { label: 'Docker 沙箱', type: 'info' }
  if (p.kind === 'service') return { label: 'HTTP 服务', type: '' }
  return null
}

// kind 徽标色系：mcp / service / bundle 三种形态一眼区分。
function kindBadge(p: PluginInfo) {
  const map: Record<string, { label: string; cls: string }> = {
    mcp: { label: 'MCP', cls: 'kind-mcp' },
    service: { label: 'Service', cls: 'kind-service' },
    bundle: { label: 'Bundle', cls: 'kind-bundle' },
  }
  const hit = map[p.kind]
  return hit || { label: p.kind, cls: 'kind-other' }
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-ink">插件管理</h2>
        <p class="text-xs text-ink-2 mt-1">热插拔 MCP / bundle 插件；启用/停用下一轮对话生效，无需重启服务。</p>
      </div>
      <el-button plain class="!bg-transparent !border-line !text-ink" :loading="loading" @click="handleReload">
        <el-icon class="mr-1"><Refresh /></el-icon> 重载配置
      </el-button>
    </div>

    <div class="grid gap-4 md:grid-cols-2 items-stretch">
      <el-card v-for="p in plugins" :key="p.id" class="!border-line !bg-card plugin-card" shadow="never">
        <!-- 标题行：名称 + 徽标组 + 右侧开关 -->
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1 flex items-center gap-2 flex-wrap">
            <span class="font-bold text-ink truncate">{{ p.name || p.id }}</span>
            <span class="kind-badge" :class="kindBadge(p).cls">{{ kindBadge(p).label }}</span>
            <el-tag v-if="transportBadge(p)" size="small" :type="transportBadge(p)!.type" effect="plain">
              {{ transportBadge(p)!.label }}
            </el-tag>
            <el-tag size="small" :type="stateType(p)" effect="plain">{{ p.enabled ? (p.state || 'enabled') : 'disabled' }}</el-tag>
            <span v-if="p.version" class="text-[10px] text-ink-3 font-mono">v{{ p.version }}</span>
          </div>
          <el-switch :model-value="p.enabled" :loading="mutating === p.id"
                     :disabled="mutating === p.id"
                     @change="() => toggle(p)" />
        </div>

        <!-- 描述 -->
        <p v-if="p.description" class="text-xs text-ink-2 mt-2 line-clamp-2 min-h-[2em]">{{ p.description }}</p>

        <!-- host_computer_use 警示条 -->
        <div v-if="p.id === 'host_computer_use'"
             class="mt-2 rounded border border-amber-500/30 bg-amber-500/10 px-2 py-1.5 text-[11px] text-amber-600 dark:text-amber-400">
          该插件直接操作宿主机 GUI（鼠标/键盘/截屏），与沙箱版 computer_use 不要同时启用。
        </div>

        <!-- 工具区：最多 12 个 chip，超出可独立展开 -->
        <div v-if="p.tools?.length" class="mt-3">
          <div class="text-[11px] text-ink-3 mb-1.5">工具 ({{ p.tools.length }})</div>
          <div class="flex flex-wrap gap-1">
            <el-tag v-for="t in visibleTools(p)" :key="t" size="small" effect="plain"
                    class="!bg-page !border-line !text-ink-2 font-mono text-[11px]">{{ t }}</el-tag>
          </div>
          <button v-if="p.tools.length > TOOL_COLLAPSE_LIMIT"
                  class="mt-1.5 text-[11px] text-primary hover:underline"
                  @click="toggleTools(p)">
            {{ isExpanded(p) ? `收起 ▴` : `展开全部 ${p.tools.length} 个工具 ▾` }}
          </button>
        </div>
        <div v-else class="mt-3 text-[11px] text-ink-3 italic">无工具</div>

        <!-- 元信息区 -->
        <div class="mt-3 pt-2 border-t border-line space-y-1 text-[11px]">
          <div v-if="p.roles?.length" class="text-ink-2">
            可见角色：<span class="text-ink-2">{{ p.roles.join('、') }}</span>
          </div>
          <div v-if="p.url" class="text-ink-2 truncate">
            入口：<a :href="p.url" target="_blank" rel="noreferrer" class="text-blue-400 hover:underline">{{ p.url }}</a>
          </div>
          <div v-if="p.missing_env?.length" class="text-orange-400">
            缺失环境变量：{{ p.missing_env.join('、') }}
          </div>
          <div v-if="p.last_error" class="text-red-400 break-all">最近错误：{{ p.last_error }}</div>
        </div>
      </el-card>
    </div>

    <div v-if="!loading && !plugins.length" class="text-center text-sm text-ink-2 py-16">
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

/* 卡片 hover：轻微阴影 + 边框高亮 */
.plugin-card {
  transition: box-shadow 0.15s ease, border-color 0.15s ease;
}
.plugin-card:hover {
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.08);
  border-color: var(--bma-primary, #409eff) !important;
}

/* kind 徽标：自定义色系（el-tag 配色受主题变量影响，这里钉死三种形态） */
.kind-badge {
  display: inline-flex;
  align-items: center;
  padding: 0 6px;
  height: 18px;
  border-radius: 4px;
  font-size: 10px;
  line-height: 18px;
  border: 1px solid transparent;
  white-space: nowrap;
}
.kind-mcp {
  color: #7c3aed;
  background: rgba(124, 58, 237, 0.08);
  border-color: rgba(124, 58, 237, 0.35);
}
.kind-service {
  color: #0891b2;
  background: rgba(8, 145, 178, 0.08);
  border-color: rgba(8, 145, 178, 0.35);
}
.kind-bundle {
  color: #059669;
  background: rgba(5, 150, 105, 0.08);
  border-color: rgba(5, 150, 105, 0.35);
}
.kind-other {
  color: var(--bma-text-2, #666);
  background: var(--bma-page, #f5f5f5);
  border-color: var(--bma-line, #ddd);
}
</style>
