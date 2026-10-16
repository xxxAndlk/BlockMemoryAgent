<template>
  <div>
    <div v-if="!workdir" class="text-sm text-ink-2 py-10 text-center">
      先在右上角填工作目录（配方存放于该目录的 <code>.bma/workflows/</code>）。
    </div>
    <template v-else>
      <el-table :data="recipes" v-loading="loading" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
        <el-table-column label="配方 ID" min-width="140">
          <template #default="{ row }">
            <span class="font-mono text-xs">{{ row.id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="标题" min-width="180">
          <template #default="{ row }">
            <span class="text-sm">{{ row.title || '—' }}</span>
            <el-tag v-if="row.error" size="small" type="danger" effect="plain" class="ml-2">损坏</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="版本" width="70">
          <template #default="{ row }">v{{ row.version }}</template>
        </el-table-column>
        <el-table-column label="生成工具" min-width="200">
          <template #default="{ row }">
            <span class="font-mono text-xs text-ink-2">{{ row.tool }}</span>
          </template>
        </el-table-column>
        <el-table-column label="交付契约" min-width="180">
          <template #default="{ row }">
            <span class="text-xs text-ink-2">
              {{ row.deliverable_type }}{{ row.frames ? ` × ${row.frames}帧` : '' }}{{ row.cell ? ` · ${row.cell}` : '' }}
            </span>
          </template>
        </el-table-column>
        <el-table-column label="阶段/锚点" width="110">
          <template #default="{ row }">
            <span class="text-xs text-ink-2">{{ row.stage_count }} / {{ row.anchor_count }}</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="180" fixed="right">
          <template #default="{ row }">
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="openRecipe(row)">
              查看 YAML
            </el-button>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="copyPath(row)">
              复制路径
            </el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="text-sm text-ink-2 py-10">
            该目录还没有配方——在对话里打样确认后，让 Agent「把这套标准存成工作流」（workflow_freeze）。
          </div>
        </template>
      </el-table>

      <!-- 配方 YAML：查看可编辑（保存=落盘新版本） -->
      <el-dialog v-model="dialogOpen" :title="`配方 ${editing?.id || ''}`" width="760px" top="6vh">
        <div v-if="editing?.error" class="text-xs text-red-400 mb-2">解析错误：{{ editing.error }}（可修正后保存）</div>
        <el-input v-model="yamlText" type="textarea" :autosize="{ minRows: 16, maxRows: 28 }" class="font-mono"
                  spellcheck="false" />
        <template #footer>
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="dialogOpen = false">关闭</el-button>
          <el-button type="primary" :loading="saving" @click="save">保存（版本 +1）</el-button>
        </template>
      </el-dialog>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, onMounted } from 'vue'
import { ElMessage } from 'element-plus'
import { getRecipe, listRecipes, saveRecipe, type RecipeSummary } from '@/api/workflow'

const props = defineProps<{ workdir: string; refreshKey: number }>()

const recipes = ref<RecipeSummary[]>([])
const loading = ref(false)
const dialogOpen = ref(false)
const editing = ref<{ id: string; error?: string } | null>(null)
const yamlText = ref('')
const saving = ref(false)

async function load() {
  if (!props.workdir) {
    recipes.value = []
    return
  }
  loading.value = true
  try {
    const res = await listRecipes(props.workdir)
    recipes.value = res.recipes || []
  } catch (e) {
    ElMessage.error('读取配方库失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function openRecipe(row: RecipeSummary) {
  try {
    const detail = await getRecipe(props.workdir, row.id)
    editing.value = { id: detail.id, error: detail.error }
    yamlText.value = detail.yaml
    dialogOpen.value = true
  } catch (e) {
    ElMessage.error('读取配方失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function save() {
  if (!editing.value) return
  saving.value = true
  try {
    const res = await saveRecipe(props.workdir, editing.value.id, yamlText.value)
    ElMessage.success(`已保存 ${res.id}（v${res.version}）`)
    dialogOpen.value = false
    await load()
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

function copyPath(row: RecipeSummary) {
  const p = `${props.workdir.replace(/[\\/]+$/, '')}/.bma/workflows/${row.id}.yaml`
  navigator.clipboard?.writeText(p).then(
    () => ElMessage.success('已复制：' + p),
    () => ElMessage.info(p),
  )
}

watch(() => [props.workdir, props.refreshKey], load)
onMounted(load)
</script>
