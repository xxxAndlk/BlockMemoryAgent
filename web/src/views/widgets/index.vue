<script setup lang="ts">
// 界面部件管理页（2026-10-13 Widget 机制）：widgets/ 目录下部件的启停与手动重扫。
// 部件本体由 Agent 经 widget_save 工具创建/更新，本页只做状态管理，不提供编辑。
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listWidgets,
  enableWidget,
  disableWidget,
  reloadWidgets,
  deleteWidget,
  listTrash,
  restoreWidget,
} from '@/api/widgets'
import type { WidgetInfo, WidgetTrashEntry } from '@/types'

const widgets = ref<WidgetInfo[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)
const trashOpen = ref(false)
const trash = ref<WidgetTrashEntry[]>([])
const trashLoading = ref(false)

async function load() {
  loading.value = true
  try {
    const res = await listWidgets()
    widgets.value = res.widgets || []
  } catch (e) {
    ElMessage.error('部件列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// 热启用/停用：原子改写 widget.json 的 enabled 字段，已打开的卡片下次加载即生效。
async function toggle(w: WidgetInfo) {
  mutating.value = w.id
  try {
    const updated = w.enabled ? await disableWidget(w.id) : await enableWidget(w.id)
    const idx = widgets.value.findIndex(x => x.id === w.id)
    if (idx >= 0) widgets.value[idx] = updated
    ElMessage.success(`部件 ${w.id} 已${updated.enabled ? '启用' : '停用'}`)
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
    load()
  } finally {
    mutating.value = null
  }
}

/** 删除部件：软删进后端回收站（widgets/.trash/），可从回收站抽屉恢复。 */
async function handleDelete(w: WidgetInfo) {
  mutating.value = w.id
  try {
    await deleteWidget(w.id)
    widgets.value = widgets.value.filter((x) => x.id !== w.id)
    ElMessage.success(`部件 ${w.id} 已移入回收站`)
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    mutating.value = null
  }
}

async function handleReload() {
  loading.value = true
  try {
    const res = await reloadWidgets()
    widgets.value = res.widgets || []
    ElMessage.success('部件已重新扫描')
  } catch (e) {
    ElMessage.error('重载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

/** 回收站：打开抽屉即拉取列表。 */
async function openTrash() {
  trashOpen.value = true
  trashLoading.value = true
  try {
    trash.value = (await listTrash()).trash || []
  } catch (e) {
    ElMessage.error('回收站加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    trashLoading.value = false
  }
}

/** 恢复后同时刷新部件列表与回收站。 */
async function handleRestore(entry: WidgetTrashEntry) {
  trashLoading.value = true
  try {
    await restoreWidget(entry.name)
    ElMessage.success(`部件 ${entry.id || entry.name} 已恢复`)
    load()
    trash.value = (await listTrash()).trash || []
  } catch (e) {
    ElMessage.error('恢复失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    trashLoading.value = false
  }
}

/** updated_at 为后端 RFC3339；解析失败给原文。 */
function formatTime(t?: string) {
  if (!t) return ''
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString()
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-ink">界面部件</h2>
        <p class="text-xs text-ink-2 mt-1">Agent 自写的渲染部件（二维码画布、美化展示卡等），在对话栏以沙箱卡片展示。</p>
      </div>
      <div class="flex items-center gap-2">
        <el-button plain class="!bg-transparent !border-line !text-ink" @click="openTrash">
          <el-icon class="mr-1"><Delete /></el-icon> 回收站
        </el-button>
        <el-button plain class="!bg-transparent !border-line !text-ink" :loading="loading" @click="handleReload">
          <el-icon class="mr-1"><Refresh /></el-icon> 重载部件
        </el-button>
      </div>
    </div>

    <div class="grid gap-4 md:grid-cols-2 items-stretch">
      <el-card v-for="w in widgets" :key="w.id" class="!border-line !bg-card widget-card" shadow="never">
        <!-- 标题行：名称 + 徽标 + 右侧开关 -->
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1 flex items-center gap-2 flex-wrap">
            <span class="font-bold text-ink truncate">{{ w.name || w.id }}</span>
            <el-tag size="small" :type="w.enabled ? 'success' : 'info'" effect="plain">
              {{ w.enabled ? 'enabled' : 'disabled' }}
            </el-tag>
            <span v-if="w.version" class="text-[10px] text-ink-3 font-mono">v{{ w.version }}</span>
          </div>
          <el-popconfirm title="删除该部件？可稍后在回收站恢复" confirm-button-text="删除" cancel-button-text="取消"
                         width="220" @confirm="() => handleDelete(w)">
            <template #reference>
              <el-button size="small" text type="danger" class="mr-1" :loading="mutating === w.id"
                         :disabled="mutating === w.id">删除</el-button>
            </template>
          </el-popconfirm>
          <el-switch :model-value="w.enabled" :loading="mutating === w.id"
                     :disabled="mutating === w.id"
                     @change="() => toggle(w)" />
        </div>

        <!-- 描述 -->
        <p v-if="w.description" class="text-xs text-ink-2 mt-2 line-clamp-2 min-h-[2em]">{{ w.description }}</p>

        <!-- 元信息区 -->
        <div class="mt-3 pt-2 border-t border-line space-y-1 text-[11px]">
          <div class="text-ink-2">
            ID：<span class="font-mono text-ink-2">{{ w.id }}</span>
          </div>
          <div class="text-ink-2">
            入口：<span class="font-mono text-ink-2">{{ w.entry }}</span>
            <span v-if="w.files?.length"> · {{ w.files.length }} 个文件</span>
          </div>
          <div class="text-ink-2">
            Owner：<span class="font-mono text-ink-2">{{ w.owner || '-' }}</span>
          </div>
          <div v-if="w.updated_at" class="text-ink-3">更新于：{{ formatTime(w.updated_at) }}</div>
        </div>
      </el-card>
    </div>

    <div v-if="!loading && !widgets.length" class="text-center text-sm text-ink-2 py-16">
      暂无界面部件。部件由 Agent 经 widget_save 创建，目录为 config.yaml 同级的 widgets/。
    </div>

    <!-- 回收站抽屉：软删部件列表与恢复 -->
    <el-drawer v-model="trashOpen" title="部件回收站" size="360px">
      <div v-loading="trashLoading" class="space-y-2">
        <div v-for="t in trash" :key="t.name"
             class="flex items-center justify-between gap-2 border border-line rounded px-3 py-2">
          <div class="min-w-0">
            <div class="text-xs text-ink font-mono truncate">{{ t.id || t.name }}</div>
            <div v-if="t.deleted_at" class="text-[10px] text-ink-3">删除于 {{ formatTime(t.deleted_at) }}</div>
          </div>
          <el-button size="small" plain type="primary" :disabled="trashLoading" @click="handleRestore(t)">
            恢复
          </el-button>
        </div>
        <div v-if="!trashLoading && !trash.length" class="text-center text-xs text-ink-3 py-8">回收站为空</div>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

/* 卡片 hover：轻微阴影 + 边框高亮（与插件管理页一致） */
.widget-card {
  transition: box-shadow 0.15s ease, border-color 0.15s ease;
}
.widget-card:hover {
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.08);
  border-color: var(--bma-primary, #409eff) !important;
}
</style>
