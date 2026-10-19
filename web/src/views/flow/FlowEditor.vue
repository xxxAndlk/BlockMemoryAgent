<template>
  <div class="h-[calc(100vh-104px)] flex flex-col text-ink -m-6 p-4">
    <!-- 顶部栏：flow 元信息 + 保存 / 试运行 -->
    <div class="flex items-center gap-2 flex-wrap shrink-0 pb-3 border-b border-line">
      <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="goBack">
        <el-icon class="mr-1"><Back /></el-icon>返回
      </el-button>
      <el-input v-model="flowId" size="small" :disabled="!isNew" placeholder="流程 ID（字母数字_-）"
                class="!w-[180px] font-mono" />
      <el-input v-model="flowName" size="small" placeholder="名称" class="!w-[160px]" />
      <el-input v-model="flowDesc" size="small" placeholder="描述（可选）" class="!w-[220px]" />
      <el-tag size="small" effect="plain" class="!border-line">v{{ flowVersion }}</el-tag>
      <div class="flex-1"></div>
      <el-button size="small" type="primary" :loading="saving" :disabled="!workdir" @click="save">
        <el-icon class="mr-1"><Check /></el-icon>保存
      </el-button>
      <el-button size="small" type="success" plain class="!bg-transparent" :disabled="!workdir" @click="openTrial">
        <el-icon class="mr-1"><CaretRight /></el-icon>试运行
      </el-button>
    </div>

    <div v-if="!workdir" class="text-sm text-ink-2 py-16 text-center">
      缺少工作目录参数——请从<a class="text-primary hover:underline cursor-pointer" @click="goBack">流程编排页</a>进入。
    </div>

    <template v-else>
      <div class="flex flex-1 min-h-0 gap-3 pt-3">
        <!-- 左侧节点面板 -->
        <div class="w-36 shrink-0 flex flex-col gap-2">
          <div class="text-xs text-ink-3 mb-1">点击加入画布中央</div>
          <button v-for="t in NODE_TYPES" :key="t.type" type="button"
                  class="flex items-center gap-2 px-3 py-2 rounded border border-line bg-card text-sm hover:border-primary hover:text-primary transition-colors"
                  @click="addNode(t.type)">
            <span class="w-2 h-2 rounded-full shrink-0" :style="{ background: t.color }"></span>
            {{ t.label }}
          </button>
          <div class="flex-1"></div>
          <div class="text-[11px] text-ink-3 leading-5">
            拖拽节点边缘圆点连线；<br>选中后按 Delete 删除<br>（start 节点不可删）。
          </div>
        </div>

        <!-- 中间画布 -->
        <div ref="wrapRef" class="flex-1 min-w-0 relative rounded border border-line overflow-hidden bg-card">
          <VueFlow :id="FLOW_INSTANCE_ID" v-model:nodes="graphNodes" v-model:edges="graphEdges"
                   :delete-key-code="['Backspace', 'Delete']"
                   :connection-radius="48" :min-zoom="0.3" :max-zoom="1.5"
                   fit-view-on-init @connect="onConnect" @node-click="onNodeClick" @pane-click="selectedId = null"
                   @before-delete="onBeforeDelete">
            <template #node-flow="slotProps">
              <FlowNodeCard :data="(slotProps.data as FlowGraphData)" :selected="slotProps.selected" @remove="removeNode" />
            </template>
            <template #no-data>
              <div class="text-xs text-ink-3 mt-8 text-center">点左侧按钮添加节点开始编排</div>
            </template>
          </VueFlow>
        </div>
      </div>

      <!-- 右侧配置抽屉：按选中节点类型渲染表单 -->
      <el-drawer :model-value="drawerOpen" :title="selectedDef ? `节点 ${selectedDef.id}（${typeLabel(selectedDef.type)}）` : '节点配置'"
                 size="440px" @update:model-value="(v: boolean) => { if (!v) selectedId = null }">
        <div v-if="selectedDef" class="flex flex-col gap-3">
          <div class="flex items-center gap-2">
            <span class="text-xs text-ink-2 shrink-0">名称</span>
            <el-input v-model="selectedDef.name" size="small" placeholder="节点显示名（可选）" />
          </div>

          <!-- start：入参定义 -->
          <template v-if="selectedDef.type === 'start'">
            <div class="text-xs font-bold text-ink">输入参数（试运行时收集）</div>
            <div v-for="(f, i) in ioFields(selectedDef.config.inputs)" :key="i" class="flex items-center gap-1">
              <el-input v-model="f.key" size="small" placeholder="key" class="!w-28 font-mono" />
              <el-select v-model="f.type" size="small" class="!w-24">
                <el-option v-for="t in IO_TYPES" :key="t" :label="t" :value="t" />
              </el-select>
              <el-checkbox v-model="f.required" size="small">必填</el-checkbox>
              <el-input v-model="f.description" size="small" placeholder="说明" class="flex-1" />
              <el-button size="small" text type="danger" @click="ioFields(selectedDef.config.inputs).splice(i, 1)">
                <el-icon><Delete /></el-icon>
              </el-button>
            </div>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink self-start"
                       @click="ioFields(selectedDef.config.inputs).push({ key: '', type: 'string', required: false, description: '' })">
              <el-icon class="mr-1"><Plus /></el-icon>加一项
            </el-button>
          </template>

          <!-- llm -->
          <template v-else-if="selectedDef.type === 'llm'">
            <div class="flex items-center gap-2">
              <span class="text-xs text-ink-2 shrink-0">模型</span>
              <el-input v-model="llmCfg.model" size="small" placeholder="留空=轻量模型；填角色 ID（如 meta_agent）用该角色模型" />
            </div>
            <div class="flex items-center gap-2">
              <span class="text-xs text-ink-2 shrink-0">温度</span>
              <el-input-number v-model="llmCfg.temperature" size="small" :min="0" :max="2" :step="0.1" />
            </div>
            <div class="text-xs font-bold text-ink">System</div>
            <VarChips :vars="varsFor(selectedDef.id)" @insert="(t: string) => insertVar('llm-system', () => llmCfg.system || '', v => llmCfg.system = v, t)" />
            <el-input :ref="setInputRef('llm-system')" v-model="llmCfg.system" type="textarea" :rows="3"
                      placeholder="系统提示词（可选）" spellcheck="false" />
            <div class="text-xs font-bold text-ink">Prompt</div>
            <VarChips :vars="varsFor(selectedDef.id)" @insert="(t: string) => insertVar('llm-prompt', () => llmCfg.prompt || '', v => llmCfg.prompt = v, t)" />
            <el-input :ref="setInputRef('llm-prompt')" v-model="llmCfg.prompt" type="textarea" :rows="5"
                      placeholder="用户提示词，可引用 {{node.key}}" spellcheck="false" />
            <div class="text-xs font-bold text-ink">输出声明（供下游引用）</div>
            <div v-for="(f, i) in ioFields(selectedDef.config.outputs)" :key="i" class="flex items-center gap-1">
              <el-input v-model="f.key" size="small" placeholder="key" class="!w-32 font-mono" />
              <el-select v-model="f.type" size="small" class="!w-28">
                <el-option v-for="t in IO_TYPES" :key="t" :label="t" :value="t" />
              </el-select>
              <el-button size="small" text type="danger" @click="ioFields(selectedDef.config.outputs).splice(i, 1)">
                <el-icon><Delete /></el-icon>
              </el-button>
            </div>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink self-start"
                       @click="ioFields(selectedDef.config.outputs).push({ key: '', type: 'string' })">
              <el-icon class="mr-1"><Plus /></el-icon>加一项
            </el-button>
          </template>

          <!-- tool -->
          <template v-else-if="selectedDef.type === 'tool'">
            <div class="flex items-center gap-2">
              <span class="text-xs text-ink-2 shrink-0">工具</span>
              <el-input v-model="toolCfg.tool" size="small" placeholder="工具名（如 WriteFile）" />
            </div>
            <div class="text-xs font-bold text-ink">参数 args</div>
            <div v-for="(row, i) in argRows" :key="i" class="flex items-center gap-1">
              <el-input v-model="row.key" size="small" placeholder="key" class="!w-28 font-mono" />
              <el-input :ref="setInputRef('arg-' + i)" v-model="row.value" size="small" placeholder="值（字符串，可含 {{}}）" class="flex-1" />
              <el-button size="small" text type="danger" @click="argRows.splice(i, 1)">
                <el-icon><Delete /></el-icon>
              </el-button>
            </div>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink self-start"
                       @click="argRows.push({ key: '', value: '' })">
              <el-icon class="mr-1"><Plus /></el-icon>加一项
            </el-button>
            <div class="text-[11px] text-ink-3">值按字符串编辑，保存时原样写入 JSON。</div>
          </template>

          <!-- branch -->
          <template v-else-if="selectedDef.type === 'branch'">
            <div class="text-xs font-bold text-ink">分支 cases（命中任一条件组走 next）</div>
            <div v-for="(c, ci) in branchCfg.cases" :key="ci" class="border border-line rounded p-2 flex flex-col gap-2">
              <div class="flex items-center gap-1">
                <el-input v-model="c.label" size="small" placeholder="分支标签（连线上显示）" class="!w-40" />
                <el-select v-model="c.next" size="small" class="flex-1" placeholder="目标节点（出边）">
                  <el-option v-for="t in outTargets(selectedDef.id)" :key="t" :label="t" :value="t" />
                </el-select>
                <el-button size="small" text type="danger" @click="branchCfg.cases.splice(ci, 1)">
                  <el-icon><Delete /></el-icon>
                </el-button>
              </div>
              <div v-for="(w, wi) in c.when" :key="wi" class="flex items-center gap-1">
                <el-input v-model="w.left" size="small" placeholder="左值（字面量或 {{}}）" class="flex-1 font-mono" />
                <el-select v-model="w.op" size="small" class="!w-[118px]">
                  <el-option v-for="op in BRANCH_OPS" :key="op" :label="op" :value="op" />
                </el-select>
                <el-input v-model="w.right" size="small" placeholder="右值" class="flex-1 font-mono" />
                <el-button size="small" text type="danger" @click="c.when.splice(wi, 1)">
                  <el-icon><Delete /></el-icon>
                </el-button>
              </div>
              <div class="flex items-center gap-2">
                <el-button size="small" text type="primary" @click="c.when.push({ left: '', op: '==', right: '' })">
                  <el-icon class="mr-1"><Plus /></el-icon>加条件
                </el-button>
                <span v-if="!c.when.length" class="text-[11px] text-orange-400">无条件=恒命中</span>
              </div>
            </div>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink self-start"
                       @click="branchCfg.cases.push({ label: '', when: [{ left: '', op: '==', right: '' }], next: '' })">
              <el-icon class="mr-1"><Plus /></el-icon>加分支
            </el-button>
            <div class="flex items-center gap-2">
              <span class="text-xs text-ink-2 shrink-0">默认</span>
              <el-select v-model="branchCfg.default" size="small" class="flex-1" placeholder="不匹配任何 case 时走（可空）" clearable>
                <el-option v-for="t in outTargets(selectedDef.id)" :key="t" :label="t" :value="t" />
              </el-select>
            </div>
            <div class="text-[11px] text-ink-3 leading-5">
              next / 默认只能从出边目标里选：先连好线再配置分支；保存时会校验每条出边都被某个 case 或默认覆盖。
            </div>
          </template>

          <!-- approval -->
          <template v-else-if="selectedDef.type === 'approval'">
            <div class="text-xs font-bold text-ink">审批问题</div>
            <VarChips :vars="varsFor(selectedDef.id)" @insert="(t: string) => insertVar('approval-q', () => approvalCfg.question || '', v => approvalCfg.question = v, t)" />
            <el-input :ref="setInputRef('approval-q')" v-model="approvalCfg.question" type="textarea" :rows="4"
                      placeholder="将向审批人展示的问题，可引用 {{node.key}}" spellcheck="false" />
          </template>

          <!-- answer -->
          <template v-else>
            <div class="text-xs font-bold text-ink">运行产出（模板或字面量）</div>
            <div v-for="(row, i) in answerRows" :key="i" class="flex flex-col gap-1">
              <div class="flex items-center gap-1">
                <el-input v-model="row.key" size="small" placeholder="key" class="!w-40 font-mono" />
                <el-button size="small" text type="danger" @click="answerRows.splice(i, 1)">
                  <el-icon><Delete /></el-icon>
                </el-button>
              </div>
              <VarChips :vars="varsFor(selectedDef.id)" @insert="(t: string) => insertVar('ans-' + i, () => row.value, v => row.value = v, t)" />
              <el-input :ref="setInputRef('ans-' + i)" v-model="row.value" type="textarea" :rows="2"
                        placeholder="值（模板或字面量）" spellcheck="false" />
            </div>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink self-start"
                       @click="answerRows.push({ key: '', value: '' })">
              <el-icon class="mr-1"><Plus /></el-icon>加一项
            </el-button>
          </template>
        </div>
      </el-drawer>

      <!-- 试运行：按 start.inputs 收集输入 -->
      <el-dialog v-model="trialOpen" title="试运行" width="520px" top="8vh">
        <div v-if="!startDef" class="text-sm text-red-400">图中没有 start 节点，无法试运行。</div>
        <template v-else>
          <div v-if="!startInputs.length" class="text-xs text-ink-2 mb-2">该流程没有声明输入参数，直接发起。</div>
          <div v-for="f in startInputs" :key="f.key" class="flex items-center gap-2 mb-2">
            <span class="text-xs text-ink-2 w-32 shrink-0 truncate" :title="f.description">{{ f.key }}</span>
            <el-input-number v-if="f.type === 'number'" v-model="trialValues[f.key]" size="small" class="flex-1" />
            <el-switch v-else-if="f.type === 'boolean'" v-model="trialValues[f.key]" size="small" />
            <el-input v-else v-model="trialValues[f.key]" size="small" class="flex-1" :placeholder="f.description || f.key" />
            <span v-if="f.required" class="text-red-400 text-xs">*</span>
          </div>
        </template>
        <template #footer>
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="trialOpen = false">取消</el-button>
          <el-button type="primary" :loading="startingRun" :disabled="!startDef" @click="launchRun">发起运行</el-button>
        </template>
      </el-dialog>

      <!-- 底部运行抽屉：节点状态 + 入参/产出 + 审批/中止 -->
      <el-drawer v-model="runDrawerOpen" direction="btt" size="360px" :with-header="false" class="!rounded-t-lg">
        <div v-if="runDetail" class="h-full flex flex-col">
          <div class="flex items-center gap-2 pb-2 border-b border-line shrink-0">
            <span class="text-sm font-bold">运行 {{ runDetail.run.id }}</span>
            <el-tag size="small" :type="statusTag(runDetail.run.status)" effect="plain">{{ runStatusText(runDetail.run.status) }}</el-tag>
            <span v-if="polling" class="text-xs text-ink-3">轮询中…</span>
            <div class="flex-1"></div>
            <el-button v-if="runActiveStatus(runDetail.run.status)" size="small" plain type="danger" class="!bg-transparent" @click="abortRun">
              中止
            </el-button>
            <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="runDrawerOpen = false">
              收起
            </el-button>
          </div>
          <div v-if="runDetail.run.error" class="text-xs text-red-400 py-1 shrink-0">运行错误：{{ runDetail.run.error }}</div>
          <div v-if="runDetail.run.status === 'done'" class="py-1 shrink-0">
            <span class="text-xs font-bold text-ink">运行产出：</span>
            <code class="text-xs text-ink-2 break-all">{{ JSON.stringify(runDetail.run.outputs) }}</code>
          </div>
          <div class="flex-1 overflow-y-auto mt-1">
            <div v-for="nr in runDetail.node_runs" :key="nr.id" class="border border-line rounded p-2 mb-2">
              <div class="flex items-center gap-2 flex-wrap">
                <span class="font-mono text-xs font-bold">{{ nr.node_id }}</span>
                <span class="text-[10px] text-ink-3">{{ nr.node_type }}</span>
                <el-tag size="small" :type="nodeStatusTag(nr.status)" effect="plain">{{ nodeStatusText(nr.status) }}</el-tag>
                <div class="flex-1"></div>
                <!-- approval 等待裁决：通过/拒绝 -->
                <template v-if="nr.status === 'waiting' && runDetail.run.status === 'waiting_approval'">
                  <el-input v-model="decideComments[nr.node_id]" size="small" placeholder="审批意见（可选）" class="!w-44" />
                  <el-button size="small" type="success" plain :loading="deciding === nr.node_id" @click="decide(nr.node_id, true)">通过</el-button>
                  <el-button size="small" type="danger" plain :loading="deciding === nr.node_id" @click="decide(nr.node_id, false)">拒绝</el-button>
                </template>
              </div>
              <div v-if="nr.error" class="text-xs text-red-400 mt-1">错误：{{ nr.error }}</div>
              <div v-if="nr.inputs && Object.keys(nr.inputs).length" class="text-[11px] text-ink-2 mt-1 break-all">
                入参：{{ JSON.stringify(nr.inputs) }}
              </div>
              <div v-if="nr.outputs && Object.keys(nr.outputs).length" class="text-[11px] text-ink-2 mt-1 break-all">
                产出：{{ JSON.stringify(nr.outputs) }}
              </div>
            </div>
            <div v-if="!runDetail.node_runs.length" class="text-xs text-ink-3 py-6 text-center">尚无节点执行记录</div>
          </div>
        </div>
        <div v-else class="text-xs text-ink-3 py-10 text-center">发起运行后这里显示节点执行追踪</div>
      </el-drawer>
    </template>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch, nextTick, onMounted, onUnmounted, h, defineComponent, type PropType, type Ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { VueFlow, useVueFlow, type Node, type Edge, type Connection, type GraphNode, type GraphEdge } from '@vue-flow/core'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import {
  getFlow, saveFlow, startFlowRun, getFlowRun, abortFlowRun, decideFlowNode,
  type FlowDef, type FlowNodeDef, type FlowNodeType, type FlowNodeConfig, type FlowIOField, type FlowBranchCase,
  type FlowNodeRun,
} from '@/api/flow'
import FlowNodeCard, { type FlowGraphData } from './FlowNodeCard.vue'

const FLOW_INSTANCE_ID = 'flow-editor'
const IO_TYPES = ['string', 'number', 'boolean', 'array', 'object']
const BRANCH_OPS = ['==', '!=', 'contains', 'not_contains', '>', '<', '>=', '<=', 'exists', 'not_exists']
const NODE_TYPES: { type: FlowNodeType; label: string; color: string }[] = [
  { type: 'start', label: '开始 start', color: '#22c55e' },
  { type: 'llm', label: 'LLM', color: '#3b82f6' },
  { type: 'tool', label: '工具 tool', color: '#8b5cf6' },
  { type: 'branch', label: '分支 branch', color: '#f59e0b' },
  { type: 'approval', label: '审批 approval', color: '#ec4899' },
  { type: 'answer', label: '回答 answer', color: '#10b981' },
]

/** 可引用变量 chips：无 chips 时不渲染。 */
const VarChips = defineComponent({
  props: { vars: { type: Array as PropType<string[]>, default: () => [] } },
  emits: ['insert'],
  setup(props, { emit }) {
    return () =>
      props.vars.length
        ? h('div', { class: 'flex flex-wrap items-center gap-1' }, [
            h('span', { class: 'text-[11px] text-ink-3 shrink-0' }, '可引用变量：'),
            props.vars.map((v) =>
              h('button', {
                type: 'button',
                class: 'text-[11px] font-mono px-1.5 py-0.5 rounded border border-line text-ink-2 hover:text-primary hover:border-primary transition-colors',
                onClick: () => emit('insert', `{{${v}}}`),
              }, v),
            ),
          ])
        : null
  },
})

const route = useRoute()
const router = useRouter()
const workdir = (route.query.workdir as string) || ''
const isNew = route.query.new === '1'
const flowIdFromQuery = (route.query.id as string) || ''

const flowId = ref(flowIdFromQuery || 'my_flow')
const flowName = ref('')
const flowDesc = ref('')
const flowVersion = ref(1)
const saving = ref(false)

// ---- 画布状态：graphNodes/graphEdges 是 Vue Flow 的真相源，def 挂在 data.def 上 ----
const wrapRef = ref<HTMLElement | null>(null)
// 注意：vue-flow 的 Node/Edge 泛型套 ref() 推断会在 TS5.9 触发 TS2589，显式 Ref 注解绕开。
const graphNodes: Ref<Node[]> = ref([])
const graphEdges: Ref<Edge[]> = ref([])
const { viewport, fitView } = useVueFlow(FLOW_INSTANCE_ID)

/** 节点 data 统一按 FlowGraphData 取（我们创建节点时必定挂 def）。 */
function dataOf(n: { data?: unknown }): FlowGraphData {
  return n.data as FlowGraphData
}

function typeLabel(t: string): string {
  return NODE_TYPES.find((n) => n.type === t)?.label || t
}

/** 从 def 构建 Vue Flow 节点/边；branch 出边按 case.next/default 标 label。 */
function loadGraph(nodes: FlowNodeDef[]) {
  graphNodes.value = nodes.map((def) => ({
    id: def.id,
    type: 'flow',
    position: { ...def.position },
    data: { def, status: '', onRemove: removeNode },
  })) as unknown as Node[]
  const edges: Edge[] = []
  for (const def of nodes) {
    for (const dep of def.depends_on || []) {
      const id = `e:${dep}->${def.id}`
      if (!edges.some((e) => e.id === id)) edges.push(makeEdge(dep, def.id))
    }
  }
  graphEdges.value = edges
  refreshEdgeLabels()
}

function makeEdge(source: string, target: string): Edge {
  const gn = graphNodes.value.find((n) => n.id === source)
  const sourceDef = gn ? dataOf(gn).def : undefined
  return {
    id: `e:${source}->${target}`,
    source,
    target,
    type: 'smoothstep',
    label: edgeLabel(sourceDef, target),
    labelBgStyle: { fill: 'var(--el-bg-color-page, #f5f7fa)' },
    animated: sourceDef?.type === 'branch',
  } as unknown as Edge
}

/** branch 出边 label：case 标签 > default > 未分配。 */
function edgeLabel(sourceDef: FlowNodeDef | undefined, target: string): string {
  if (sourceDef?.type !== 'branch') return ''
  const cases = sourceDef.config.cases || []
  const hit = cases.find((c) => c.next === target)
  if (hit) return hit.label || 'case'
  if (sourceDef.config.default === target) return 'default'
  return '未分配'
}

/** 连线 label 依赖 branch 配置，深监听节点数据变化重建 label。 */
function refreshEdgeLabels() {
  for (const e of graphEdges.value) {
    const gn = graphNodes.value.find((n) => n.id === e.source)
    const sourceDef = gn ? dataOf(gn).def : undefined
    e.label = edgeLabel(sourceDef, e.target)
    e.animated = sourceDef?.type === 'branch'
  }
}
watch(graphNodes, refreshEdgeLabels, { deep: true })

function onConnect(conn: Connection) {
  if (!conn.source || !conn.target || conn.source === conn.target) return
  if (graphEdges.value.some((e) => e.id === `e:${conn.source}->${conn.target}`)) return
  graphEdges.value.push(makeEdge(conn.source, conn.target))
  refreshEdgeLabels()
}

/** 键盘/按钮删除统一入口：start 节点不可删（连同关联边一起清）。 */
function onBeforeDelete(params: { nodes: GraphNode[]; edges: GraphEdge[] }): boolean {
  if (params.nodes.some((n) => dataOf(n).def.type === 'start')) {
    ElMessage.warning('start 节点不可删除')
    return false
  }
  return true
}

function removeNode(id: string) {
  const gn = graphNodes.value.find((n) => n.id === id)
  const def = gn ? dataOf(gn).def : undefined
  if (!def || def.type === 'start') {
    ElMessage.warning('start 节点不可删除')
    return
  }
  graphNodes.value = graphNodes.value.filter((n) => n.id !== id)
  graphEdges.value = graphEdges.value.filter((e) => e.source !== id && e.target !== id)
  if (selectedId.value === id) selectedId.value = null
}

// ---- 节点添加 ----
function addNode(type: FlowNodeType) {
  let n = graphNodes.value.length + 1
  while (graphNodes.value.some((g) => g.id === `node_${n}`)) n++
  const def = defaultDef(`node_${n}`, type)
  // 落点：视口中央（画布容器坐标 → flow 坐标）
  const el = wrapRef.value
  const vp = viewport.value || { x: 0, y: 0, zoom: 1 }
  const x = el ? (el.clientWidth / 2 - vp.x) / vp.zoom : 200
  const y = el ? (el.clientHeight / 2 - vp.y) / vp.zoom : 120
  graphNodes.value.push({ id: def.id, type: 'flow', position: { x, y }, data: { def, status: '', onRemove: removeNode } })
}

function defaultDef(id: string, type: FlowNodeType): FlowNodeDef {
  const base: FlowNodeDef = { id, type, position: { x: 0, y: 0 }, depends_on: [], config: {} }
  switch (type) {
    case 'start':
      base.config = { inputs: [] }
      break
    case 'llm':
      base.config = { model: '', system: '', prompt: '', temperature: 0.3, outputs: [{ key: 'text', type: 'string' }] }
      break
    case 'tool':
      base.config = { tool: '', args: {} }
      break
    case 'branch':
      base.config = { cases: [], default: '' }
      break
    case 'approval':
      base.config = { question: '' }
      break
    case 'answer':
      base.config = { outputs: {} }
      break
  }
  return base
}

// ---- 配置抽屉 ----
const selectedId = ref<string | null>(null)
const drawerOpen = computed({
  get: () => selectedId.value !== null,
  set: (v: boolean) => {
    if (!v) selectedId.value = null
  },
})
const selectedDef = computed<FlowNodeDef | null>(() => {
  if (!selectedId.value) return null
  const gn = graphNodes.value.find((n) => n.id === selectedId.value)
  return gn ? dataOf(gn).def : null
})

function onNodeClick(e: { node: GraphNode }) {
  selectedId.value = e.node.id
}

/** config 子对象的安全访问（选中节点切换时类型收窄）。 */
function cfg(): FlowNodeConfig {
  return selectedDef.value?.config || {}
}
const llmCfg = computed(() => {
  const c = cfg()
  if (c.model === undefined) c.model = ''
  if (c.system === undefined) c.system = ''
  if (c.prompt === undefined) c.prompt = ''
  if (c.temperature === undefined) c.temperature = 0.3
  return c
})
const toolCfg = computed(() => {
  const c = cfg()
  if (c.tool === undefined) c.tool = ''
  if (c.args === undefined) c.args = {}
  return c
})
const branchCfg = computed(() => {
  const c = cfg()
  if (!c.cases) c.cases = []
  if (c.default === undefined) c.default = ''
  return c as FlowNodeConfig & { cases: FlowBranchCase[]; default: string }
})
const approvalCfg = computed(() => {
  const c = cfg()
  if (c.question === undefined) c.question = ''
  return c
})

/** start.inputs / llm.outputs 统一当数组编辑（answer.outputs 是 map，走 answerRows）。 */
function ioFields(v: unknown): FlowIOField[] {
  if (!v) return []
  return v as FlowIOField[]
}

/** branch 出边目标（next/default 下拉选项）。 */
function outTargets(id: string): string[] {
  const list: string[] = []
  for (const e of graphEdges.value) {
    if (e.source === id && !list.includes(e.target)) list.push(e.target)
  }
  return list
}

// answer outputs / tool args 的键值镜像（值按字符串编辑）
const answerRows = ref<{ key: string; value: string }[]>([])
const argRows = ref<{ key: string; value: string }[]>([])

watch(selectedDef, (def) => {
  if (!def) return
  if (def.type === 'answer') {
    const outputs = (def.config.outputs || {}) as Record<string, unknown>
    answerRows.value = Object.entries(outputs).map(([key, v]) => ({ key, value: typeof v === 'string' ? v : JSON.stringify(v) }))
  }
  if (def.type === 'tool') {
    const args = def.config.args || {}
    argRows.value = Object.entries(args).map(([key, v]) => ({ key, value: typeof v === 'string' ? v : JSON.stringify(v) }))
  }
})

// 镜像 → config（保证保存用的 config 始终最新）
watch(answerRows, (rows) => {
  if (selectedDef.value?.type !== 'answer') return
  const out: Record<string, unknown> = {}
  for (const r of rows) if (r.key) out[r.key] = r.value
  selectedDef.value.config.outputs = out
}, { deep: true })
watch(argRows, (rows) => {
  if (selectedDef.value?.type !== 'tool') return
  const out: Record<string, unknown> = {}
  for (const r of rows) if (r.key) out[r.key] = r.value
  selectedDef.value.config.args = out
}, { deep: true })

// ---- 可引用变量 ----
/** 上游声明输出（BFS 入边）：start=inputs key，llm=outputs key，tool=result/path，approval=approved/comment。 */
function varsFor(nodeId: string): string[] {
  const result = new Set<string>()
  const queue = [nodeId]
  const seen = new Set<string>()
  const outs = graphEdges.value
  while (queue.length) {
    const cur = queue.shift() as string
    if (seen.has(cur)) continue
    seen.add(cur)
    for (const e of outs) {
      if (e.target !== cur) continue
      queue.push(e.source)
      const src = graphNodes.value.find((n) => n.id === e.source)
      const def = src ? dataOf(src).def : undefined
      if (!def) continue
      const add = (suffix: string[]) => suffix.forEach((s) => result.add(`${def.id}.${s}`))
      switch (def.type) {
        case 'start':
          add(ioFields(def.config.inputs).filter((f) => f.key).map((f) => f.key))
          break
        case 'llm':
          add(ioFields(def.config.outputs).filter((f) => f.key).map((f) => f.key))
          break
        case 'tool':
          add(['result', 'path'])
          break
        case 'approval':
          add(['approved', 'comment'])
          break
      }
    }
  }
  return [...result]
}

// ---- 文本域光标处插入变量 ----
interface InputHolder { input?: HTMLInputElement | HTMLTextAreaElement; textarea?: HTMLTextAreaElement }
const inputRefs = new Map<string, InputHolder | null>()
function setInputRef(key: string) {
  return (el: unknown) => inputRefs.set(key, (el as InputHolder | null) ?? null)
}

function insertVar(refKey: string, get: () => string, set: (v: string) => void, token: string) {
  const ta = inputRefs.get(refKey)?.input ?? inputRefs.get(refKey)?.textarea
  const base = get() || ''
  let pos = base.length
  if (ta && document.activeElement === ta && ta.selectionStart != null) {
    const s = ta.selectionStart
    const e2 = ta.selectionEnd ?? s
    set(base.slice(0, s) + token + base.slice(e2))
    pos = s + token.length
  } else {
    set(base + token)
    pos = base.length + token.length
  }
  nextTick(() => {
    if (ta) {
      ta.focus()
      ta.setSelectionRange(pos, pos)
    }
  })
}

// ---- 保存：组装 flow JSON + 静态校验 ----
function buildFlowDef(): FlowDef {
  const nodes: FlowNodeDef[] = graphNodes.value.map((gn) => {
    const def = dataOf(gn).def
    const deps: string[] = []
    for (const e of graphEdges.value) {
      if (e.target === gn.id && !deps.includes(e.source)) deps.push(e.source)
    }
    return {
      id: def.id,
      type: def.type,
      name: def.name,
      position: { x: gn.position.x, y: gn.position.y },
      depends_on: deps,
      config: def.config,
    }
  })
  return { id: flowId.value.trim(), name: flowName.value.trim(), version: flowVersion.value, description: flowDesc.value.trim(), nodes }
}

/** 保存前静态校验，返回错误列表（空=通过）。 */
function validateFlow(def: FlowDef): string[] {
  const errs: string[] = []
  if (!/^[A-Za-z0-9_-]+$/.test(def.id)) errs.push(`流程 ID「${def.id}」不合法（仅字母/数字/_/-）`)
  const starts = def.nodes.filter((n) => n.type === 'start')
  if (starts.length !== 1) errs.push(`必须有且仅有一个 start 节点（当前 ${starts.length} 个）`)
  if (!def.nodes.some((n) => n.type === 'answer')) errs.push('至少需要一个 answer 节点')
  const ids = new Set<string>()
  for (const n of def.nodes) {
    if (!/^[A-Za-z0-9_-]+$/.test(n.id)) errs.push(`节点 id「${n.id}」不合法（仅字母/数字/_/-）`)
    if (ids.has(n.id)) errs.push(`节点 id「${n.id}」重复`)
    ids.add(n.id)
    // 各类型必填段（对齐后端 validateNodeConfig，提前在前台拦住）
    switch (n.type) {
      case 'start':
      case 'llm': {
        const fields = ioFields(n.type === 'start' ? n.config.inputs : n.config.outputs)
        fields.forEach((f, i) => {
          if (!f.key) errs.push(`节点 ${n.id} 第 ${i + 1} 个变量未填 key`)
          else if (!/^[A-Za-z][A-Za-z0-9_-]*$/.test(f.key)) errs.push(`节点 ${n.id} 变量 key「${f.key}」非法（字母开头，限字母数字_-）`)
        })
        if (n.type === 'llm' && !(n.config.prompt || '').trim()) errs.push(`llm 节点 ${n.id} 的 prompt 不能为空`)
        break
      }
      case 'tool':
        if (!(n.config.tool || '').trim()) errs.push(`tool 节点 ${n.id} 未填工具名`)
        break
      case 'approval':
        if (!(n.config.question || '').trim()) errs.push(`approval 节点 ${n.id} 的 question 不能为空`)
        break
      case 'branch':
        if (!(n.config.cases || []).length) errs.push(`branch 节点 ${n.id} 至少一条 case`)
        break
      case 'answer': {
        const outs = Object.keys((n.config.outputs || {}) as Record<string, unknown>)
        if (!outs.length) errs.push(`answer 节点 ${n.id} 至少一个输出`)
        break
      }
    }
  }
  // branch：case.next/default 必须指向出边目标，且每条出边都要被覆盖
  for (const n of def.nodes) {
    if (n.type !== 'branch') continue
    const targets = def.nodes.filter((x) => x.depends_on.includes(n.id)).map((x) => x.id)
    const covered = new Set<string>()
    for (const c of n.config.cases || []) {
      if (!c.next) errs.push(`branch 节点 ${n.id} 有 case 未选目标`)
      else if (!targets.includes(c.next)) errs.push(`branch 节点 ${n.id} 的 case「${c.label || '?'}」目标 ${c.next} 不在其出边上`)
      else covered.add(c.next)
    }
    if (n.config.default) {
      if (!targets.includes(n.config.default)) errs.push(`branch 节点 ${n.id} 的 default ${n.config.default} 不在其出边上`)
      else covered.add(n.config.default)
    }
    for (const t of targets) {
      if (!covered.has(t)) errs.push(`branch 节点 ${n.id} 有出边指向 ${t}，但未被任何 case/default 覆盖`)
    }
  }
  // {{}} 引用校验：首段必须是上游可引用节点、第二段必须在声明输出里
  const refRe = /\{\{([^}]+)\}\}/g
  const scan = (nodeId: string, where: string, text: unknown) => {
    if (typeof text !== 'string') return
    let m: RegExpExecArray | null
    while ((m = refRe.exec(text))) {
      const path = m[1].trim().split('.')
      const upstream = upstreamVars(nodeId)
      const full = path.join('.')
      if (!upstream.has(path[0])) errs.push(`节点 ${nodeId} 的${where}引用了不可达节点 ${path[0]}（{{${full}}}）`)
      else if (path.length < 2 || !upstream.get(path[0])!.includes(path[1])) errs.push(`节点 ${nodeId} 的${where}引用了未声明输出 {{${full}}}（${path[0]} 仅声明：${upstream.get(path[0])!.join('、') || '无'}）`)
    }
  }
  for (const n of def.nodes) {
    switch (n.type) {
      case 'llm':
        scan(n.id, 'prompt', n.config.prompt)
        scan(n.id, 'system', n.config.system)
        break
      case 'tool':
        scan(n.id, 'args', JSON.stringify(n.config.args || {}))
        break
      case 'approval':
        scan(n.id, 'question', n.config.question)
        break
      case 'answer':
        for (const [k, v] of Object.entries((n.config.outputs || {}) as Record<string, unknown>)) scan(n.id, `输出 ${k}`, v)
        break
      case 'branch':
        for (const c of n.config.cases || []) {
          for (const w of c.when) {
            scan(n.id, `分支条件`, w.left)
            scan(n.id, `分支条件`, w.right)
          }
        }
        break
    }
  }
  return errs
}

/** 节点上游（含传递上游）的可引用表：nodeId → 声明输出 keys。 */
function upstreamVars(nodeId: string): Map<string, string[]> {
  const map = new Map<string, string[]>()
  const queue = [nodeId]
  const seen = new Set<string>()
  while (queue.length) {
    const cur = queue.shift() as string
    if (seen.has(cur)) continue
    seen.add(cur)
    for (const e of graphEdges.value) {
      if (e.target !== cur) continue
      queue.push(e.source)
      const src = graphNodes.value.find((n) => n.id === e.source)
      const def = src ? dataOf(src).def : undefined
      if (!def) continue
      switch (def.type) {
        case 'start':
          map.set(def.id, ioFields(def.config.inputs).filter((f) => f.key).map((f) => f.key))
          break
        case 'llm':
          map.set(def.id, ioFields(def.config.outputs).filter((f) => f.key).map((f) => f.key))
          break
        case 'tool':
          map.set(def.id, ['result', 'path'])
          break
        case 'approval':
          map.set(def.id, ['approved', 'comment'])
          break
      }
    }
  }
  return map
}

async function save() {
  const def = buildFlowDef()
  const errs = validateFlow(def)
  if (errs.length) {
    ElMessageBox.alert(`<div style="max-height:300px;overflow:auto">${errs.map((e) => `• ${e}`).join('<br>')}</div>`, '保存校验未通过', {
      dangerouslyUseHTMLString: true,
    })
    return
  }
  saving.value = true
  try {
    const res = await saveFlow(workdir, def.id, JSON.stringify(def, null, 2))
    flowVersion.value = res.version
    ElMessage.success(`已保存 ${res.id}（v${res.version}）`)
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

// ---- 试运行与追踪 ----
const startDef = computed<FlowNodeDef | null>(() => {
  const gn = graphNodes.value.find((n) => dataOf(n).def.type === 'start')
  return gn ? dataOf(gn).def : null
})
const startInputs = computed<FlowIOField[]>(() => ioFields(startDef.value?.config.inputs).filter((f) => f.key))
const trialOpen = ref(false)
const trialValues = ref<Record<string, unknown>>({})
const startingRun = ref(false)
const runDrawerOpen = ref(false)
const runDetail = ref<{ run: { id: string; status: string; error?: string; outputs?: Record<string, unknown> }; node_runs: FlowNodeRun[] } | null>(null)
const polling = ref(false)
const deciding = ref('')
const decideComments = ref<Record<string, string>>({})
let pollTimer: ReturnType<typeof setInterval> | null = null

function openTrial() {
  trialValues.value = {}
  trialOpen.value = true
}

function runActiveStatus(s: string): boolean {
  return s === 'pending' || s === 'running' || s === 'waiting_approval'
}

async function launchRun() {
  if (!startDef.value) return
  // 按声明类型收敛输入值
  const inputs: Record<string, unknown> = {}
  for (const f of startInputs.value) {
    let v = trialValues.value[f.key]
    if (v === undefined || v === null || v === '') {
      if (f.required) {
        ElMessage.warning(`缺少必填输入 ${f.key}`)
        return
      }
      continue
    }
    if (f.type === 'number') v = Number(v)
    if (f.type === 'boolean') v = Boolean(v)
    inputs[f.key] = v
  }
  startingRun.value = true
  try {
    const res = await startFlowRun(workdir, flowId.value.trim(), inputs)
    trialOpen.value = false
    runDrawerOpen.value = true
    for (const gn of graphNodes.value) dataOf(gn).status = ''
    runDetail.value = { run: { id: res.run_id, status: res.status }, node_runs: [] }
    decideComments.value = {}
    startPolling(res.run_id)
  } catch (e) {
    ElMessage.error('发起运行失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    startingRun.value = false
  }
}

function startPolling(runId: string) {
  stopPolling()
  polling.value = true
  const tick = async () => {
    try {
      const d = await getFlowRun(runId)
      runDetail.value = d
      const byNode = new Map(d.node_runs.map((nr) => [nr.node_id, nr.status]))
      for (const gn of graphNodes.value) dataOf(gn).status = byNode.get(gn.id) || ''
      if (!runActiveStatus(d.run.status)) stopPolling()
    } catch {
      // 单次轮询失败（后端重启等）静默，下轮再试
    }
  }
  void tick()
  pollTimer = setInterval(() => void tick(), 1500)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
  polling.value = false
}

async function abortRun() {
  if (!runDetail.value) return
  try {
    await abortFlowRun(runDetail.value.run.id)
    ElMessage.success('已中止')
    const d = await getFlowRun(runDetail.value.run.id)
    runDetail.value = d
    stopPolling()
  } catch (e) {
    ElMessage.error('中止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function decide(nodeId: string, approved: boolean) {
  if (!runDetail.value) return
  deciding.value = nodeId
  try {
    await decideFlowNode(runDetail.value.run.id, nodeId, approved, decideComments.value[nodeId] || undefined)
    ElMessage.success(approved ? '已通过，继续执行' : '已拒绝')
    const d = await getFlowRun(runDetail.value.run.id)
    runDetail.value = d
    if (runActiveStatus(d.run.status)) startPolling(d.run.id)
  } catch (e) {
    ElMessage.error('裁决失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    deciding.value = ''
  }
}

function statusTag(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'primary'
    case 'waiting_approval': return 'warning'
    case 'done': return 'success'
    case 'failed': return 'danger'
    case 'aborted': return 'info'
    default: return 'info'
  }
}
function runStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队中', running: '运行中', waiting_approval: '等待审批', done: '已完成', failed: '失败', aborted: '已中止',
  }
  return map[s] || s
}
function nodeStatusTag(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'primary'
    case 'waiting': return 'warning'
    case 'success': return 'success'
    case 'failed': return 'danger'
    default: return 'info' // pending|skipped
  }
}
function nodeStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '等待', running: '运行中', success: '成功', failed: '失败', waiting: '等待审批', skipped: '跳过',
  }
  return map[s] || s
}

// ---- 加载 ----
function loadDefaultTemplate() {
  const start = defaultDef('start', 'start')
  start.position = { x: 40, y: 200 }
  const llm = defaultDef('llm', 'llm')
  llm.position = { x: 320, y: 200 }
  llm.depends_on = ['start']
  llm.config.prompt = ''
  const answer = defaultDef('answer', 'answer')
  answer.position = { x: 620, y: 200 }
  answer.depends_on = ['llm']
  answer.config.outputs = { result: '{{llm.text}}' }
  flowId.value = flowIdFromQuery || 'my_flow'
  flowName.value = '新流程'
  loadGraph([start, llm, answer])
}

async function loadExisting() {
  try {
    const detail = await getFlow(workdir, flowIdFromQuery)
    flowName.value = detail.name || flowIdFromQuery
    flowVersion.value = detail.version || 1
    let def: FlowDef | null = null
    try {
      def = JSON.parse(detail.json) as FlowDef
    } catch {
      def = null
    }
    if (!def || !Array.isArray(def.nodes)) {
      ElMessageBox.alert(detail.error || '流程 JSON 解析失败，无法用可视化编辑器打开。', '流程文件损坏', { type: 'error' })
      loadDefaultTemplate()
      return
    }
    flowId.value = def.id || flowIdFromQuery
    flowName.value = def.name || flowName.value
    flowDesc.value = def.description || ''
    flowVersion.value = detail.version || def.version || 1
    const nodes = def.nodes.map((n) => {
      const d = defaultDef(n.id || 'node', n.type)
      return {
        ...d,
        ...n,
        position: n.position || d.position,
        depends_on: n.depends_on || [],
        config: { ...d.config, ...((n.config || {}) as FlowNodeConfig) },
      }
    })
    loadGraph(nodes)
  } catch (e) {
    ElMessage.error('读取流程失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

function goBack() {
  router.push({ path: '/flow', query: { workdir: workdir || undefined } })
}

onMounted(async () => {
  if (!workdir) return
  if (isNew || !flowIdFromQuery) loadDefaultTemplate()
  else await loadExisting()
  await nextTick()
  fitView({ padding: 0.2 })
})
onUnmounted(stopPolling)
</script>
