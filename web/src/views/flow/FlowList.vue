<template>
  <div>
    <div class="flex items-center justify-between mb-3">
      <div class="text-xs text-ink-2">
        流程存放于工作目录的 <code>.bma/flows/*.json</code>。
      </div>
      <el-button size="small" type="primary" plain :disabled="!workdir" @click="createFlow">
        <el-icon class="mr-1"><Plus /></el-icon>新建流程
      </el-button>
    </div>

    <div v-if="!workdir" class="text-sm text-ink-2 py-10 text-center">
      先在右上角填工作目录（流程存放于该目录的 <code>.bma/flows/</code>）。
    </div>
    <el-table v-else :data="flows" v-loading="loading" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
      <el-table-column label="流程 ID" min-width="140">
        <template #default="{ row }">
          <span class="font-mono text-xs">{{ row.id }}</span>
        </template>
      </el-table-column>
      <el-table-column label="名称" min-width="160">
        <template #default="{ row }">
          <span class="text-sm">{{ row.name || '—' }}</span>
          <el-tag v-if="row.error" size="small" type="danger" effect="plain" class="ml-2">损坏</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="版本" width="70">
        <template #default="{ row }">v{{ row.version }}</template>
      </el-table-column>
      <el-table-column label="节点数" width="80">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ row.node_count }}</span>
        </template>
      </el-table-column>
      <el-table-column label="描述" min-width="220">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ row.description || '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="错误" min-width="200">
        <template #default="{ row }">
          <span v-if="row.error" class="text-xs text-red-400">{{ row.error }}</span>
          <span v-else class="text-xs text-ink-3">—</span>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="100" fixed="right">
        <template #default="{ row }">
          <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="editFlow(row)">
            编辑
          </el-button>
        </template>
      </el-table-column>
      <template #empty>
        <div class="text-sm text-ink-2 py-10">
          该目录还没有流程——点右上角「新建流程」在可视化编辑器里搭一个。
        </div>
      </template>
    </el-table>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listFlows, type FlowSummary } from '@/api/flow'

const props = defineProps<{ workdir: string; refreshKey: number }>()
const router = useRouter()

const flows = ref<FlowSummary[]>([])
const loading = ref(false)

async function load() {
  if (!props.workdir) {
    flows.value = []
    return
  }
  loading.value = true
  try {
    const res = await listFlows(props.workdir)
    flows.value = res.flows || []
  } catch (e) {
    ElMessage.error('读取流程库失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

/** 编辑器页统一走 /flow/edit，id 缺省视为新建。 */
function editFlow(row: FlowSummary) {
  router.push({ path: '/flow/edit', query: { workdir: props.workdir, id: row.id } })
}

function createFlow() {
  router.push({ path: '/flow/edit', query: { workdir: props.workdir, new: '1' } })
}

watch(() => [props.workdir, props.refreshKey], load)
onMounted(load)
</script>
