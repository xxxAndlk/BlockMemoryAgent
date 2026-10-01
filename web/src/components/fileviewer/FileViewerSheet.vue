<script setup lang="ts">
// FileViewer 表格视图（TODO #26 阶段 B）：
// - CSV/TSV：前端引号感知解析（utils/fileKind.parseDelimited），万行内直渲；
// - xlsx/xls：SheetJS 按需动态加载，多 sheet 页签切换，只读渲染。
// 超出行数上限截断渲染并提示（避免大表卡死渲染线程）。
import { ref, computed, watch, onBeforeUnmount } from 'vue'
import { Loading } from '@element-plus/icons-vue'
import { parseDelimited } from '@/utils/fileKind'

const ROW_CAP = 20000
const CELL_CAP = 400000

const props = defineProps<{
  /** 文件名（判定 csv/tsv 还是 xlsx） */
  name: string
  /** CSV/TSV：已拉取的文本内容 */
  text?: string
  /** xlsx：raw 端点 URL（前端取 arraybuffer 解析） */
  url?: string
}>()

const loading = ref(false)
const error = ref('')
/** sheet 页签列表（CSV/TSV 只有一页） */
const sheets = ref<Array<{ name: string; rows: string[][] }>>([])
const activeSheet = ref(0)

const truncated = ref(false)

function capRows(rows: string[][]): string[][] {
  let cellCount = 0
  let cut = rows.length
  for (let i = 0; i < rows.length; i++) {
    cellCount += rows[i].length
    if (i + 1 > ROW_CAP || cellCount > CELL_CAP) {
      cut = i
      break
    }
  }
  truncated.value = cut < rows.length
  return rows.slice(0, cut)
}

function loadDelimited() {
  const ext = props.name.split('.').pop()?.toLowerCase() || ''
  const delim = ext === 'tsv' ? '\t' : ','
  const rows = parseDelimited(props.text ?? '', delim)
  sheets.value = [{ name: props.name, rows: capRows(rows) }]
  activeSheet.value = 0
}

async function loadXlsx() {
  const XLSX = await import('xlsx')
  if (!props.url) return
  const resp = await fetch(props.url)
  if (!resp.ok) throw new Error(`下载失败（HTTP ${resp.status}）`)
  const buf = await resp.arrayBuffer()
  const wb = XLSX.read(buf, { type: 'array' })
  const out: Array<{ name: string; rows: string[][] }> = []
  for (const name of wb.SheetNames) {
    const ws = wb.Sheets[name]
    const rows = XLSX.utils.sheet_to_json<string[]>(ws, { header: 1, raw: false, defval: '' })
    out.push({ name, rows: capRows(rows) })
  }
  sheets.value = out
  activeSheet.value = 0
}

async function load() {
  loading.value = true
  error.value = ''
  sheets.value = []
  try {
    const ext = props.name.split('.').pop()?.toLowerCase() || ''
    if (ext === 'xlsx' || ext === 'xls') await loadXlsx()
    else loadDelimited()
  } catch (e) {
    error.value = `表格解析失败：${e instanceof Error ? e.message : String(e)}`
  } finally {
    loading.value = false
  }
}

watch(() => [props.name, props.text, props.url], () => { void load() }, { immediate: true })

const curRows = computed(() => sheets.value[activeSheet.value]?.rows || [])

onBeforeUnmount(() => {
  sheets.value = [] // 关闭释放表格内存
})
</script>

<template>
  <div class="sheet-view">
    <!-- sheet 页签 -->
    <div v-if="sheets.length > 1" class="sheet-tabs">
      <button v-for="(s, i) in sheets" :key="s.name" type="button"
              class="sheet-tab" :class="{ active: i === activeSheet }"
              :title="s.name" @click="activeSheet = i">
        {{ s.name }}
      </button>
    </div>

    <div class="sheet-body">
      <div v-if="loading" class="sheet-state">
        <el-icon class="is-loading"><Loading /></el-icon> 解析中…
      </div>
      <div v-else-if="error" class="sheet-state error">{{ error }}</div>
      <div v-else-if="!curRows.length" class="sheet-state">（空表）</div>
      <template v-else>
        <div v-if="truncated" class="sheet-truncated">
          表格过大，仅渲染前 {{ curRows.length }} 行；完整内容请下载后用表格软件打开。
        </div>
        <div class="sheet-scroll">
          <table class="sheet-table">
            <tbody>
              <tr v-for="(row, ri) in curRows" :key="ri">
                <td v-for="(cell, ci) in row" :key="ci" class="sheet-cell"
                    :class="{ head: ri === 0 }">{{ cell }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.sheet-view {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}
.sheet-tabs {
  display: flex;
  align-items: center;
  gap: 2px;
  padding: 6px 12px;
  background: #12151c;
  border-bottom: 1px solid #23262e;
  overflow-x: auto;
  flex-shrink: 0;
}
.sheet-tab {
  padding: 4px 12px;
  border: 1px solid transparent;
  border-radius: 6px;
  background: none;
  color: #a8afba;
  font-size: 12.5px;
  cursor: pointer;
  white-space: nowrap;
}
.sheet-tab:hover {
  background: #23262e;
}
.sheet-tab.active {
  color: #8a94ff;
  background: #232947;
  border-color: #4f5dff;
}
.sheet-body {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.sheet-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  flex: 1;
  color: #a8afba;
  font-size: 13px;
}
.sheet-state.error {
  color: #f87171;
}
.sheet-truncated {
  padding: 6px 12px;
  font-size: 12px;
  color: #fbbf24;
  background: rgba(245, 158, 11, 0.1);
  border-bottom: 1px solid #23262e;
  flex-shrink: 0;
}
.sheet-scroll {
  flex: 1;
  min-height: 0;
  overflow: auto;
}
.sheet-table {
  border-collapse: collapse;
  font-size: 12.5px;
  min-width: 100%;
}
.sheet-cell {
  padding: 4px 10px;
  border-bottom: 1px solid #1d2027;
  border-right: 1px solid #1d2027;
  color: #cfd4dc;
  white-space: nowrap;
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sheet-cell.head {
  position: sticky;
  top: 0;
  background: #1a1d24;
  color: #e5e7eb;
  font-weight: 600;
  z-index: 1;
}
</style>
