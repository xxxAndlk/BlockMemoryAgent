<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  listDags,
  saveDag,
  deleteDag,
  triggerDag,
  getRunning,
  type DagJob,
  type DagTask,
} from '@/api/dag'
import { fmtDateTime } from '@/utils/date'

const dags = ref<DagJob[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)

async function load() {
  loading.value = true
  try {
    dags.value = (await listDags()) || []
  } catch (e) {
    ElMessage.error('定时任务列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// ---------- 启用/禁用 ----------
async function toggle(d: DagJob) {
  mutating.value = d.id
  try {
    const updated = await saveDag({ ...d, enabled: !d.enabled })
    const idx = dags.value.findIndex(x => x.id === d.id)
    if (idx >= 0) dags.value[idx] = updated
    ElMessage.success(`任务「${d.name || d.id}」已${updated.enabled ? '启用' : '停用'}`)
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
    load()
  } finally {
    mutating.value = null
  }
}

// ---------- 立即触发 ----------
async function trigger(d: DagJob) {
  mutating.value = d.id
  try {
    await triggerDag(d.id)
    ElMessage.success(`已触发「${d.name || d.id}」，可在「运行快照」中查看进度`)
  } catch (e) {
    ElMessage.error('触发失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    mutating.value = null
  }
}

// ---------- 删除 ----------
async function remove(d: DagJob) {
  try {
    await ElMessageBox.confirm(
      `确定删除定时任务「${d.name || d.id}」吗？删除后不可恢复。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return // 用户取消
  }
  try {
    await deleteDag(d.id)
    dags.value = dags.value.filter(x => x.id !== d.id)
    ElMessage.success('已删除')
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// ---------- 新建/编辑对话框 ----------
const dialogVisible = ref(false)
const saving = ref(false)
const editing = ref(false)
const form = ref<DagJob>(emptyForm())
// 简单模式（默认）：名称 + 任务描述 + 时间选择，后端落单节点 DAG；
// 高级模式：多节点 DAG 编辑器（编辑多节点任务时自动进入）。
const advanced = ref(false)
const description = ref('')

function emptyForm(): DagJob {
  return {
    id: `dag-${Date.now()}`,
    name: '',
    cron: '',
    enabled: true,
    tasks: [],
    created_at: '',
    updated_at: '',
  }
}

function openCreate() {
  editing.value = false
  advanced.value = false
  description.value = ''
  form.value = emptyForm()
  form.value.tasks.push(emptyTask())
  resetSchedule()
  dialogVisible.value = true
}

function openEdit(d: DagJob) {
  editing.value = true
  // 深拷贝，避免编辑过程中直接改到列表数据
  form.value = JSON.parse(JSON.stringify(d))
  form.value.tasks = form.value.tasks || []
  // 单节点任务走简单模式；多节点自动进入高级模式
  advanced.value = form.value.tasks.length > 1
  description.value = form.value.tasks.length === 1 ? form.value.tasks[0].goal : ''
  parseCronToForm(form.value.cron)
  dialogVisible.value = true
}

function emptyTask(): DagTask {
  return { id: '', goal: '', depends_on: [], status: 'pending', session_id: '' }
}

function addTask() {
  form.value.tasks.push(emptyTask())
}

function removeTask(idx: number) {
  form.value.tasks.splice(idx, 1)
}

/** 某行可选的依赖：其他 task 的 id（排除自身与空 id）。 */
function depOptions(row: DagTask): string[] {
  return form.value.tasks.map(t => t.id).filter(id => id && id !== row.id)
}

// ---------- 调度规则：友好选择 <-> cron 互转 ----------
type SchedMode = 'manual' | 'daily' | 'weekly' | 'monthly' | 'interval' | 'cron'
const schedMode = ref<SchedMode>('daily')
const schedTime = ref('09:00')
const schedWeekDays = ref<number[]>([1])
const schedMonthDay = ref(1)
const intervalNum = ref(30)
const intervalUnit = ref<'m' | 'h'>('h')

const weekDayOptions = [
  { value: 1, label: '周一' },
  { value: 2, label: '周二' },
  { value: 3, label: '周三' },
  { value: 4, label: '周四' },
  { value: 5, label: '周五' },
  { value: 6, label: '周六' },
  { value: 0, label: '周日' },
]

function resetSchedule() {
  schedMode.value = 'daily'
  schedTime.value = '09:00'
  schedWeekDays.value = [1]
  schedMonthDay.value = 1
  intervalNum.value = 30
  intervalUnit.value = 'h'
}

function pad2(n: number | string): string {
  return String(n).padStart(2, '0')
}

/** 当前选择对应的 cron 表达式（对话框实时预览 + 保存时写入）。 */
function buildCron(): string {
  const [h, m] = (schedTime.value || '09:00').split(':').map(Number)
  switch (schedMode.value) {
    case 'manual':
      return ''
    case 'daily':
      return `${m || 0} ${h || 0} * * *`
    case 'weekly': {
      const ds = [...schedWeekDays.value].sort((a, b) => a - b)
      return `${m || 0} ${h || 0} * * ${ds.join(',')}`
    }
    case 'monthly':
      return `${m || 0} ${h || 0} ${schedMonthDay.value} * *`
    case 'interval':
      return `${intervalNum.value}${intervalUnit.value}`
    case 'cron':
      return form.value.cron.trim()
  }
}

/** 编辑时把已有 cron 反解到选择器；认不出的落入「cron 表达式」模式原样编辑。 */
function parseCronToForm(cron: string) {
  resetSchedule()
  const c = (cron || '').trim()
  if (!c) {
    schedMode.value = 'manual'
    return
  }
  let m = c.match(/^(\d{1,2}) (\d{1,2}) \* \* \*$/)
  if (m) {
    schedMode.value = 'daily'
    schedTime.value = `${pad2(+m[2])}:${pad2(+m[1])}`
    return
  }
  m = c.match(/^(\d{1,2}) (\d{1,2}) \* \* ([\d,]+)$/)
  if (m) {
    schedMode.value = 'weekly'
    schedTime.value = `${pad2(+m[2])}:${pad2(+m[1])}`
    schedWeekDays.value = m[3].split(',').map(Number)
    return
  }
  m = c.match(/^(\d{1,2}) (\d{1,2}) (\d{1,2}) \* \*$/)
  if (m) {
    schedMode.value = 'monthly'
    schedTime.value = `${pad2(+m[2])}:${pad2(+m[1])}`
    schedMonthDay.value = +m[3]
    return
  }
  m = c.match(/^(\d+)([mh])$/)
  if (m) {
    schedMode.value = 'interval'
    intervalNum.value = +m[1]
    intervalUnit.value = m[2] as 'm' | 'h'
    return
  }
  schedMode.value = 'cron'
}

/** 列表页调度规则的人类可读描述。 */
function describeCron(cron: string): string {
  const c = (cron || '').trim()
  if (!c) return '仅手动'
  let m = c.match(/^(\d{1,2}) (\d{1,2}) \* \* \*$/)
  if (m) return `每天 ${pad2(+m[2])}:${pad2(+m[1])}`
  m = c.match(/^(\d{1,2}) (\d{1,2}) \* \* ([\d,]+)$/)
  if (m) {
    const names = '日一二三四五六'
    return `每周${m[3].split(',').map(d => names[+d]).join('、')} ${pad2(+m[2])}:${pad2(+m[1])}`
  }
  m = c.match(/^(\d{1,2}) (\d{1,2}) (\d{1,2}) \* \*$/)
  if (m) return `每月 ${+m[3]} 日 ${pad2(+m[2])}:${pad2(+m[1])}`
  m = c.match(/^(\d+)([mh])$/)
  if (m) return `每隔 ${+m[1]} ${m[2] === 'm' ? '分钟' : '小时'}`
  return c
}

/** 保存前校验：名称必填；简单模式校验描述；高级模式校验节点图。 */
function validate(): string | null {
  const f = form.value
  if (!f.name.trim()) return '请填写任务名称'
  if (schedMode.value === 'weekly' && !schedWeekDays.value.length) return '请选择每周的哪几天'
  if (schedMode.value === 'interval' && (!intervalNum.value || intervalNum.value <= 0)) return '间隔必须为正整数'
  if (!advanced.value) {
    if (!description.value.trim()) return '请填写任务描述'
    return null
  }
  if (!f.tasks.length) return '至少需要一个任务节点'
  const ids = new Set<string>()
  for (const t of f.tasks) {
    if (!t.id.trim()) return '任务节点 id 不能为空'
    if (ids.has(t.id)) return `任务节点 id 重复：${t.id}`
    ids.add(t.id)
  }
  for (const t of f.tasks) {
    for (const dep of t.depends_on || []) {
      if (!ids.has(dep)) return `任务「${t.id}」依赖了不存在的节点：${dep}`
      if (dep === t.id) return `任务「${t.id}」不能依赖自身`
    }
  }
  return null
}

async function save() {
  const err = validate()
  if (err) {
    ElMessage.warning(err)
    return
  }
  saving.value = true
  try {
    const payload = { ...form.value, cron: buildCron() }
    if (!advanced.value) {
      // 简单模式：落单节点；编辑已有单节点任务时保留原节点 id/状态
      const node: DagTask =
        editing.value && form.value.tasks.length === 1
          ? { ...form.value.tasks[0], goal: description.value.trim() }
          : { id: 'main', goal: description.value.trim(), depends_on: [], status: 'pending', session_id: '' }
      payload.tasks = [node]
    }
    // 保存后后端刷新 updated_at，cron 以此刻为基准重新计时
    const saved = await saveDag(payload)
    const idx = dags.value.findIndex(x => x.id === saved.id)
    if (idx >= 0) dags.value[idx] = saved
    else dags.value.push(saved)
    ElMessage.success('已保存，定时基准已从当前时刻重新计算')
    dialogVisible.value = false
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

// ---------- 运行快照 ----------
const runningVisible = ref(false)
const runningLoading = ref(false)
const running = ref<DagJob[]>([])

async function openRunning() {
  runningVisible.value = true
  runningLoading.value = true
  try {
    const snap = await getRunning()
    running.value = Object.values(snap || {})
  } catch (e) {
    ElMessage.error('运行快照加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    runningLoading.value = false
  }
}

// 状态徽标：pending/running/completed/failed 四色
function statusType(s: string): 'info' | 'warning' | 'success' | 'danger' {
  switch (s) {
    case 'running':
      return 'warning'
    case 'completed':
      return 'success'
    case 'failed':
      return 'danger'
    default:
      return 'info'
  }
}

const runningEmpty = computed(() => !runningLoading.value && !running.value.length)

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-ink">定时任务</h2>
        <p class="text-xs text-ink-2 mt-1">按 cron 定时触发 DAG 任务流水线；任务节点按依赖关系串/并行派发为会话执行。</p>
      </div>
      <div class="flex gap-2">
        <el-button plain class="!bg-transparent !border-line !text-ink" @click="openRunning">
          <el-icon class="mr-1"><Monitor /></el-icon> 运行快照
        </el-button>
        <el-button plain class="!bg-transparent !border-line !text-ink" :loading="loading" @click="load">
          <el-icon class="mr-1"><Refresh /></el-icon> 刷新
        </el-button>
        <el-button type="primary" @click="openCreate">
          <el-icon class="mr-1"><Plus /></el-icon> 新建任务
        </el-button>
      </div>
    </div>

    <el-table :data="dags" v-loading="loading" class="!bg-card" :header-cell-style="{ background: 'transparent' }">
      <el-table-column label="名称" min-width="160">
        <template #default="{ row }">
          <div class="font-bold text-ink">{{ row.name || row.id }}</div>
          <div class="text-[10px] text-ink-3 font-mono">{{ row.id }}</div>
        </template>
      </el-table-column>
      <el-table-column label="调度规则" width="150">
        <template #default="{ row }">
          <div class="text-xs text-ink">{{ describeCron(row.cron) }}</div>
          <div v-if="row.cron && describeCron(row.cron) !== row.cron" class="font-mono text-[10px] text-ink-3">{{ row.cron }}</div>
        </template>
      </el-table-column>
      <el-table-column label="任务数" width="80" align="center">
        <template #default="{ row }">{{ row.tasks?.length || 0 }}</template>
      </el-table-column>
      <el-table-column label="更新时间" width="130">
        <template #default="{ row }">
          <span class="text-xs text-ink-2">{{ fmtDateTime(row.updated_at) }}</span>
        </template>
      </el-table-column>
      <el-table-column label="启用" width="90" align="center">
        <template #default="{ row }">
          <el-switch
            :model-value="row.enabled"
            :loading="mutating === row.id"
            :disabled="mutating === row.id"
            @change="() => toggle(row)"
          />
        </template>
      </el-table-column>
      <el-table-column label="操作" width="220" align="right">
        <template #default="{ row }">
          <el-button size="small" link type="primary" :disabled="mutating === row.id" @click="trigger(row)">立即触发</el-button>
          <el-button size="small" link @click="openEdit(row)">编辑</el-button>
          <el-button size="small" link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
      <template #empty>
        <div class="text-sm text-ink-2 py-10">暂无定时任务，点击「新建任务」创建第一条流水线。</div>
      </template>
    </el-table>

    <!-- 新建/编辑对话框 -->
    <el-dialog
      v-model="dialogVisible"
      :title="editing ? '编辑定时任务' : '新建定时任务'"
      width="720px"
      top="6vh"
    >
      <el-form label-width="90px" label-position="left">
        <el-form-item v-if="advanced || editing" label="ID">
          <el-input v-model="form.id" :disabled="editing" class="font-mono" />
        </el-form-item>
        <el-form-item label="任务名称" required>
          <el-input v-model="form.name" placeholder="例如：每日 AI 新闻日报" />
        </el-form-item>
        <el-form-item v-if="!advanced" label="任务描述" required>
          <el-input
            v-model="description"
            type="textarea"
            :rows="3"
            placeholder="到点要 Agent 做什么。例如：汇总最近 36 小时的 AI 大事件，生成 HTML 日报保存到 ai-daily/reports/ 目录"
          />
        </el-form-item>
        <el-form-item label="执行时间">
          <div class="w-full space-y-2">
            <div class="flex items-center gap-2 flex-wrap">
              <el-select v-model="schedMode" class="!w-32">
                <el-option label="仅手动触发" value="manual" />
                <el-option label="每天" value="daily" />
                <el-option label="每周" value="weekly" />
                <el-option label="每月" value="monthly" />
                <el-option label="每隔一段" value="interval" />
                <el-option label="cron 表达式" value="cron" />
              </el-select>
              <el-time-picker
                v-if="schedMode === 'daily' || schedMode === 'weekly' || schedMode === 'monthly'"
                v-model="schedTime"
                format="HH:mm"
                value-format="HH:mm"
                placeholder="选择时间"
                class="!w-28"
              />
              <el-select
                v-if="schedMode === 'weekly'"
                v-model="schedWeekDays"
                multiple
                collapse-tags
                placeholder="哪几天"
                class="!w-44"
              >
                <el-option v-for="o in weekDayOptions" :key="o.value" :label="o.label" :value="o.value" />
              </el-select>
              <template v-if="schedMode === 'monthly'">
                <el-input-number v-model="schedMonthDay" :min="1" :max="31" class="!w-28" />
                <span class="text-xs text-ink-2">日</span>
              </template>
              <template v-if="schedMode === 'interval'">
                <span class="text-xs text-ink-2">每隔</span>
                <el-input-number v-model="intervalNum" :min="1" :max="9999" class="!w-28" />
                <el-select v-model="intervalUnit" class="!w-24">
                  <el-option label="分钟" value="m" />
                  <el-option label="小时" value="h" />
                </el-select>
              </template>
              <el-input
                v-if="schedMode === 'cron'"
                v-model="form.cron"
                placeholder="0 9 * * * 或 30m / 24h"
                class="flex-1 font-mono"
              />
            </div>
            <div class="text-[11px] text-ink-3 leading-5">
              <template v-if="schedMode === 'manual'">不自动触发，只能在列表里手动「立即触发」。</template>
              <template v-else>
                将按 <code>{{ buildCron() }}</code> 执行；保存后以此刻为基准重新计时。
              </template>
              「每隔一段」为相对间隔，适合巡检类；定点执行用每天/每周/每月。
            </div>
          </div>
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item v-if="advanced" label="任务节点" required>
          <div class="w-full space-y-3">
            <el-card v-for="(t, i) in form.tasks" :key="i" class="!border-line !bg-page" shadow="never">
              <div class="flex items-center gap-2 mb-2">
                <el-input v-model="t.id" placeholder="节点 id（如 fetch）" class="!w-44 font-mono" size="small" />
                <el-select
                  v-model="t.depends_on"
                  multiple
                  collapse-tags
                  placeholder="依赖节点（可多选）"
                  size="small"
                  class="flex-1"
                >
                  <el-option v-for="opt in depOptions(t)" :key="opt" :label="opt" :value="opt" />
                </el-select>
                <el-button size="small" link type="danger" @click="removeTask(i)">
                  <el-icon><Delete /></el-icon>
                </el-button>
              </div>
              <el-input
                v-model="t.goal"
                type="textarea"
                :rows="2"
                placeholder="该节点的目标描述（goal），将作为会话任务派发执行"
              />
            </el-card>
            <el-button plain size="small" class="!bg-transparent !border-line !text-ink" @click="addTask">
              <el-icon class="mr-1"><Plus /></el-icon> 添加任务
            </el-button>
          </div>
        </el-form-item>
        <el-form-item>
          <el-button link type="primary" size="small" @click="advanced = !advanced">
            {{ advanced ? '收起多节点编辑（简单模式）' : '需要多个步骤有依赖关系？展开多节点编辑' }}
          </el-button>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <!-- 运行快照对话框 -->
    <el-dialog v-model="runningVisible" title="运行中的 DAG 快照" width="640px" top="8vh">
      <div v-loading="runningLoading">
        <div v-if="runningEmpty" class="text-sm text-ink-2 text-center py-10">当前没有运行中的 DAG。</div>
        <el-card v-for="d in running" :key="d.id" class="!border-line !bg-page mb-3" shadow="never">
          <div class="flex items-center gap-2 mb-2">
            <span class="font-bold text-ink">{{ d.name || d.id }}</span>
            <span class="text-[10px] text-ink-3 font-mono">{{ d.id }}</span>
          </div>
          <div class="space-y-1">
            <div v-for="t in d.tasks" :key="t.id" class="flex items-center gap-2 text-xs">
              <el-tag size="small" :type="statusType(t.status)" effect="plain">{{ t.status || 'pending' }}</el-tag>
              <span class="font-mono text-ink">{{ t.id }}</span>
              <span v-if="t.session_id" class="text-ink-3 font-mono truncate">session: {{ t.session_id }}</span>
            </div>
          </div>
        </el-card>
      </div>
      <template #footer>
        <el-button @click="runningVisible = false">关闭</el-button>
        <el-button type="primary" @click="openRunning">刷新</el-button>
      </template>
    </el-dialog>
  </div>
</template>
