<script setup lang="ts">
// FileViewer Notebook 视图（TODO #26 阶段 B）：ipynb JSON 渲染 cell 序列——
// markdown cell 走 MarkdownRenderer，code cell 等宽源码 + 输出区（stream/error/
// execute_result，含 image/png base64 内联），输出默认折叠可展开。
import { computed, ref, watch } from 'vue'
import { ArrowRight } from '@element-plus/icons-vue'
import MarkdownRenderer from '@/components/MarkdownRenderer.vue'

const props = defineProps<{
  content: string
}>()

interface NbOutput {
  output_type: string
  name?: string
  text?: string | string[]
  traceback?: string[]
  data?: Record<string, unknown>
  ename?: string
  evalue?: string
}
interface NbCell {
  cell_type: 'markdown' | 'code' | 'raw'
  source: string | string[]
  outputs?: NbOutput[]
  execution_count?: number | null
}

interface ParsedCell {
  type: 'markdown' | 'code' | 'raw'
  source: string
  outputs: Array<{ kind: 'text'; text: string } | { kind: 'image'; src: string }>
  errorText: string
  execCount: number | null
}

const parseError = ref('')
const rawCells = ref<NbCell[]>([])
/** 输出区折叠状态（cell 下标 → 是否折叠），默认折叠。 */
const collapsedMap = ref<Record<number, boolean>>({})

function isCollapsed(i: number) {
  return collapsedMap.value[i] !== false
}
function toggleOut(i: number) {
  collapsedMap.value = { ...collapsedMap.value, [i]: !isCollapsed(i) }
}

const cells = computed<ParsedCell[]>(() =>
  rawCells.value.map(c => {
    const source = Array.isArray(c.source) ? c.source.join('') : (c.source || '')
    const outputs: ParsedCell['outputs'] = []
    let errorText = ''
    for (const o of c.outputs || []) {
      if (o.output_type === 'stream') {
        const t = Array.isArray(o.text) ? o.text.join('') : (o.text || '')
        if (t) outputs.push({ kind: 'text', text: t })
      } else if (o.output_type === 'error') {
        errorText = (o.traceback || []).join('\n') || `${o.ename || 'Error'}: ${o.evalue || ''}`
      } else if (o.data) {
        const img = o.data['image/png']
        if (typeof img === 'string' && img) {
          outputs.push({ kind: 'image', src: `data:image/png;base64,${img.replace(/\n/g, '')}` })
        }
        const plain = o.data['text/plain']
        if (typeof plain === 'string' && plain) {
          outputs.push({ kind: 'text', text: plain })
        } else if (Array.isArray(plain)) {
          outputs.push({ kind: 'text', text: plain.join('') })
        }
      }
    }
    return {
      type: c.cell_type === 'code' ? 'code' : c.cell_type,
      source,
      outputs,
      errorText,
      execCount: c.execution_count ?? null,
    }
  }),
)

function refresh() {
  parseError.value = ''
  rawCells.value = []
  collapsedMap.value = {}
  try {
    const nb = JSON.parse(props.content) as { cells?: NbCell[] }
    rawCells.value = Array.isArray(nb.cells) ? nb.cells : []
  } catch (e) {
    parseError.value = `Notebook 解析失败：${e instanceof Error ? e.message : String(e)}`
  }
}
watch(() => props.content, refresh)
refresh()
</script>

<template>
  <div class="nb-view">
    <div v-if="parseError" class="nb-state error">{{ parseError }}</div>
    <div v-else-if="!cells.length" class="nb-state">（无 cell）</div>
    <div v-else class="nb-scroll">
      <div v-for="(cell, i) in cells" :key="i" class="nb-cell" :class="`nb-${cell.type}`">
        <!-- markdown cell：MarkdownRenderer 渲染 -->
        <MarkdownRenderer v-if="cell.type === 'markdown'" :content="cell.source" class="nb-md" />

        <!-- code cell：等宽源码 + 执行序号 -->
        <template v-else-if="cell.type === 'code'">
          <div class="nb-code-head">
            <span class="nb-exec">[{{ cell.execCount ?? ' ' }}]</span>
          </div>
          <pre class="nb-code">{{ cell.source || ' ' }}</pre>
          <!-- 输出区：默认折叠 -->
          <div v-if="cell.outputs.length || cell.errorText" class="nb-outputs">
            <button type="button" class="nb-out-toggle" @click="toggleOut(i)">
              <el-icon :size="12" :class="{ rotated: !isCollapsed(i) }"><ArrowRight /></el-icon>
              输出（{{ cell.outputs.length + (cell.errorText ? 1 : 0) }}）
            </button>
            <div v-show="!isCollapsed(i)" class="nb-out-body">
              <template v-for="(o, oi) in cell.outputs" :key="oi">
                <img v-if="o.kind === 'image'" :src="o.src" class="nb-out-img" alt="cell output" />
                <pre v-else class="nb-out-text">{{ o.text }}</pre>
              </template>
              <pre v-if="cell.errorText" class="nb-out-error">{{ cell.errorText }}</pre>
            </div>
          </div>
        </template>

        <!-- raw cell：纯文本 -->
        <pre v-else class="nb-code">{{ cell.source || ' ' }}</pre>
      </div>
    </div>
  </div>
</template>

<style scoped>
.nb-view {
  height: 100%;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.nb-state {
  display: flex;
  align-items: center;
  justify-content: center;
  flex: 1;
  color: #a8afba;
  font-size: 13px;
}
.nb-state.error {
  color: #f87171;
}
.nb-scroll {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: 12px 20px 24px;
}
.nb-cell {
  margin-bottom: 10px;
}
.nb-md {
  padding: 8px 12px;
  color: #d6dae2;
  font-size: 13.5px;
  line-height: 1.7;
}
.nb-md :deep(p) { margin: 0 0 6px; }
.nb-code-head {
  padding: 2px 12px;
}
.nb-exec {
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 11.5px;
  color: #4b5261;
}
.nb-code {
  margin: 0;
  padding: 8px 12px;
  background: #11141a;
  border-left: 3px solid #4f5dff;
  border-radius: 0 6px 6px 0;
  font-family: 'JetBrains Mono', 'Fira Code', Consolas, monospace;
  font-size: 12.5px;
  line-height: 1.6;
  color: #d6dae2;
  white-space: pre-wrap;
  word-break: break-all;
}
.nb-outputs {
  margin-top: 2px;
}
.nb-out-toggle {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px;
  border: none;
  border-radius: 5px;
  background: none;
  color: #8a94ff;
  font-size: 12px;
  cursor: pointer;
}
.nb-out-toggle:hover {
  background: #232947;
}
.nb-out-toggle .rotated {
  transform: rotate(90deg);
}
.nb-out-body {
  padding: 6px 12px;
}
.nb-out-text {
  margin: 0 0 6px;
  padding: 8px 10px;
  background: #16181d;
  border-radius: 6px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  color: #b8bfca;
  white-space: pre-wrap;
  word-break: break-all;
}
.nb-out-error {
  margin: 0 0 6px;
  padding: 8px 10px;
  background: rgba(220, 38, 38, 0.12);
  border: 1px solid rgba(220, 38, 38, 0.4);
  border-radius: 6px;
  font-family: 'JetBrains Mono', Consolas, monospace;
  font-size: 12px;
  color: #fca5a5;
  white-space: pre-wrap;
  word-break: break-all;
}
.nb-out-img {
  display: block;
  margin: 0 0 6px;
  max-width: 100%;
  background: #fff;
  border-radius: 6px;
}
</style>
