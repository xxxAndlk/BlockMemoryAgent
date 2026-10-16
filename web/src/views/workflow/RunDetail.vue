<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div v-if="!run" class="text-sm text-ink-2 py-16 text-center" v-loading="loading">
      <span v-if="!loading">运行不存在或已删除。</span>
    </div>
    <template v-else>
      <!-- 头部控制条 -->
      <div class="flex items-start justify-between flex-wrap gap-3 mb-4">
        <div>
          <div class="flex items-center gap-2 flex-wrap">
            <router-link to="/workflow?tab=runs" class="text-ink-2 hover:text-primary">
              <el-icon class="align-middle"><ArrowLeft /></el-icon>
            </router-link>
            <h2 class="text-lg font-bold font-mono">{{ run.id }}</h2>
            <el-tag size="small" :type="runStatusType(run.status)" effect="plain">{{ runStatusText(run.status) }}</el-tag>
            <el-tag v-if="run.pause_reason" size="small" type="warning" effect="plain">{{ pauseReasonText(run.pause_reason) }}</el-tag>
          </div>
          <p class="text-xs text-ink-2 mt-1">
            配方 {{ run.recipe_id }} v{{ run.recipe_version }} · 会话 {{ run.session_id }} · 件间并发 {{ run.concurrency }} ·
            {{ run.fail_fast ? '已开' : '已关' }}早期熔断
          </p>
        </div>
        <div class="flex items-center gap-2 flex-wrap">
          <el-button v-if="run.status === 'running'" size="small" plain class="!bg-transparent !border-line !text-ink"
                     :loading="acting" @click="doPause">
            <el-icon class="mr-1"><VideoPause /></el-icon>暂停
          </el-button>
          <el-button v-if="run.status === 'paused'" size="small" type="primary" :loading="acting" @click="resumeDialog = true">
            <el-icon class="mr-1"><VideoPlay /></el-icon>续跑
          </el-button>
          <el-button v-if="!isTerminal" size="small" plain class="!bg-transparent !border-line !text-red-400"
                     :loading="acting" @click="doAbort">
            <el-icon class="mr-1"><CircleClose /></el-icon>中止
          </el-button>
          <el-button size="small" plain class="!bg-transparent !border-line !text-ink"
                     :loading="reviewing" @click="toggleReview">
            <el-icon class="mr-1"><Grid /></el-icon>{{ reviewOpen ? '收起审查台' : '联系表审查' }}
          </el-button>
          <el-button size="small" plain class="!bg-transparent !border-line !text-ink"
                     :loading="feedbacking" @click="doFeedback">
            <el-icon class="mr-1"><RefreshLeft /></el-icon>回灌配方
          </el-button>
        </div>
      </div>

      <!-- 进度与预算 -->
      <div class="bg-card border border-line rounded-card p-4 mb-4 flex items-center gap-6 flex-wrap">
        <div class="flex-1 min-w-[240px]">
          <div class="flex items-center justify-between text-xs text-ink-2 mb-1">
            <span>进度（单元最新尝试口径）</span>
            <span>{{ doneCells }}/{{ totalCells }} · 待审 {{ counts.needs_review || 0 }} · 失败 {{ counts.failed || 0 }}</span>
          </div>
          <el-progress :percentage="progressPct" :stroke-width="10"
                       :status="run.status === 'failed' ? 'exception' : run.status === 'done' ? 'success' : undefined" />
        </div>
        <div class="shrink-0 text-xs text-ink-2">
          <div>预算消耗</div>
          <div class="text-sm mt-0.5" :class="run.spent_estimate >= run.budget_cap ? 'text-red-400' : 'text-ink'">
            {{ run.spent_estimate }} / {{ run.budget_cap }} <span class="text-ink-2">次轻量调用</span>
          </div>
        </div>
        <div class="shrink-0 text-xs text-ink-2">
          <div>创建时间</div>
          <div class="text-sm mt-0.5 text-ink">{{ fmtDateTime(run.created_at) }}</div>
        </div>
        <div v-if="run.finished_at" class="shrink-0 text-xs text-ink-2">
          <div>结束时间</div>
          <div class="text-sm mt-0.5 text-ink">{{ fmtDateTime(run.finished_at) }}</div>
        </div>
      </div>

      <!-- 联系表审查台 -->
      <ContactSheet v-if="reviewOpen" :session-id="run.session_id" :items="run.roster" :stages="stageList"
                    :units="units" :acting="acting" @action="onUnitAction" @open-cell="openDrawer" />

      <!-- 名册 × 阶段网格 -->
      <div class="bg-card border border-line rounded-card p-4">
        <div class="flex items-center justify-between mb-2">
          <div class="text-sm font-bold">名册 × 阶段</div>
          <div class="text-xs text-ink-2">点格子看该单元全部尝试与产物</div>
        </div>
        <RosterGrid :items="run.roster" :stages="stageList" :units="units" @open-cell="openDrawer" />
      </div>

      <!-- 单元抽屉：attempts 时间线 + 人审操作 -->
      <el-drawer v-model="drawerOpen" :title="drawerTitle" size="520px">
        <template v-if="drawerCellUnits.length">
          <div v-for="u in drawerCellUnits" :key="u.id" class="border border-line rounded-card p-3 mb-3">
            <div class="flex items-center gap-2 flex-wrap">
              <el-tag size="small" :type="unitStatusType(u.status)" effect="plain">{{ unitStatusText(u.status) }}</el-tag>
              <span class="text-xs text-ink-2">第 {{ u.attempt_no }} 次尝试</span>
              <el-tag v-if="driftFlag(u)" size="small" type="warning" effect="plain">漂移离群</el-tag>
              <el-tag v-if="upgradeAgent(u)" size="small" type="primary" effect="plain">已升级 {{ upgradeAgent(u) }}</el-tag>
              <span class="text-xs text-ink-2 ml-auto">{{ fmtDateTime(u.updated_at) }}</span>
            </div>
            <div v-if="u.reject_reason" class="text-xs text-orange-400 mt-2">{{ u.reject_reason }}</div>
            <div v-if="isImage(u.artifact_path)" class="mt-2">
              <img :src="workspaceUrl(run.session_id, u.artifact_path!)" class="max-h-56 rounded border border-line object-contain"
                   loading="lazy" />
              <div class="text-xs text-ink-2 mt-1 font-mono break-all">{{ u.artifact_path }}</div>
            </div>
            <div v-else-if="u.artifact_path" class="text-xs text-ink-2 mt-2 font-mono break-all">{{ u.artifact_path }}</div>
            <div v-if="checkItems(u).length" class="mt-2 space-y-1">
              <div v-for="(it, i) in checkItems(u)" :key="i" class="text-xs flex items-start gap-1">
                <el-icon :class="it.pass ? 'text-green-500' : 'text-red-400'" class="mt-0.5">
                  <component :is="it.pass ? 'CircleCheck' : 'CircleClose'" />
                </el-icon>
                <span :class="it.pass ? 'text-ink-2' : 'text-red-400'">{{ it.reason }}</span>
              </div>
            </div>
            <div v-if="u.status === 'needs_review'" class="mt-3 flex items-center gap-2 flex-wrap">
              <el-button size="small" type="success" plain @click="onUnitAction(u, 'accept')">采纳</el-button>
              <el-button size="small" type="danger" plain @click="openReject(u)">驳回…</el-button>
              <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="onUnitAction(u, 'reroll')">重摇</el-button>
              <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="onUnitAction(u, 'skip')">跳过</el-button>
            </div>
            <div v-else-if="u.status === 'failed'" class="mt-3 flex items-center gap-2 flex-wrap">
              <el-button size="small" type="danger" plain @click="openReject(u)">驳回…（记录理由）</el-button>
              <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="onUnitAction(u, 'reroll')">重摇</el-button>
              <el-button size="small" plain class="!bg-transparent !border-line !text-ink" @click="onUnitAction(u, 'skip')">跳过</el-button>
            </div>
          </div>
        </template>
      </el-drawer>

      <!-- 驳回理由 -->
      <el-dialog v-model="rejectDialog" title="驳回该单元（理由进配方演化输入）" width="480px">
        <el-input v-model="rejectReason" type="textarea" :rows="3" placeholder="如：边缘有白边、与族系锚主色不符、第 3 帧卡顿…" />
        <template #footer>
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="rejectDialog = false">取消</el-button>
          <el-button type="danger" :disabled="!rejectReason.trim()" @click="confirmReject">驳回</el-button>
        </template>
      </el-dialog>

      <!-- 续跑（预算提额） -->
      <el-dialog v-model="resumeDialog" title="续跑" width="440px">
        <div v-if="run.pause_reason === 'budget_exceeded'" class="text-sm text-ink-2 mb-3">
          因预算超限暂停（已消耗 {{ run.spent_estimate }}/{{ run.budget_cap }}）。续跑需要先提额：
        </div>
        <div v-else class="text-sm text-ink-2 mb-3">确认续跑该运行？</div>
        <div class="flex items-center gap-2" v-if="run.pause_reason === 'budget_exceeded'">
          <span class="text-sm">追加预算</span>
          <div class="w-[140px]">
            <el-input-number v-model="extraBudget" :min="1" :max="100000" size="small" class="!w-full" />
          </div>
          <span class="text-xs text-ink-2">次（新上限 {{ run.budget_cap + extraBudget }}）</span>
        </div>
        <template #footer>
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="resumeDialog = false">取消</el-button>
          <el-button type="primary" @click="doResume">续跑</el-button>
        </template>
      </el-dialog>

      <!-- 回灌配方草案 -->
      <el-dialog v-model="feedbackDialog" :title="`回灌配方 ${run.recipe_id} → v${run.recipe_version + 1} 草案`"
                 width="760px" top="6vh">
        <div class="text-xs text-ink-2 mb-2">
          由本 run 的驳回/失败理由聚合生成，可编辑；保存后成为新版本（本 run 仍绑定 v{{ run.recipe_version }}，后续新 run 用新版本）。
        </div>
        <el-input v-model="feedbackDraft" type="textarea" :autosize="{ minRows: 16, maxRows: 28 }" class="font-mono"
                  spellcheck="false" />
        <template #footer>
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="feedbackDialog = false">取消</el-button>
          <el-button type="primary" :loading="savingDraft" @click="saveDraft">确认落盘</el-button>
        </template>
      </el-dialog>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import {
  abortRun, getRun, pauseRun, resumeRun, runFeedback, saveRecipe, unitAction,
  type UnitCheckResult, type WorkflowRun, type WorkflowUnit,
} from '@/api/workflow'
import { workspaceUrl } from '@/api/session'
import { fmtDateTime } from '@/utils/date'
import RosterGrid from './RosterGrid.vue'
import ContactSheet from './ContactSheet.vue'

const route = useRoute()
const runId = String(route.params.id || '')

const run = ref<WorkflowRun | null>(null)
const units = ref<WorkflowUnit[]>([])
const loading = ref(false)
const acting = ref(false)
const reviewing = ref(false)
const reviewOpen = ref(false)
let timer: ReturnType<typeof setInterval> | null = null

const drawerOpen = ref(false)
const drawerCell = ref<{ itemId: string; stage: string } | null>(null)
const rejectDialog = ref(false)
const rejectTarget = ref<WorkflowUnit | null>(null)
const rejectReason = ref('')
const resumeDialog = ref(false)
const extraBudget = ref(50)
const feedbacking = ref(false)
const feedbackDialog = ref(false)
const feedbackDraft = ref('')
const savingDraft = ref(false)

const isTerminal = computed(() => ['done', 'failed', 'aborted'].includes(run.value?.status || ''))

/** 阶段顺序：按 units 首次出现顺序（建 run 时按名册 × 配方阶段序生成）。 */
const stageList = computed(() => {
  const seen: string[] = []
  for (const u of units.value) {
    if (!seen.includes(u.stage)) seen.push(u.stage)
  }
  return seen
})

/** cell 口径：每个 (item, stage) 的最新 attempt。 */
const latestByCell = computed(() => {
  const map = new Map<string, WorkflowUnit>()
  for (const u of units.value) {
    const key = `${u.item_id}\u0000${u.stage}`
    const cur = map.get(key)
    if (!cur || u.attempt_no > cur.attempt_no) map.set(key, u)
  }
  return map
})

const counts = computed(() => {
  const c: Record<string, number> = {}
  for (const u of latestByCell.value.values()) c[u.status] = (c[u.status] || 0) + 1
  return c
})
const totalCells = computed(() => latestByCell.value.size)
const doneCells = computed(() => (counts.value.done || 0) + (counts.value.skipped || 0))
const progressPct = computed(() => (totalCells.value === 0 ? 0 : Math.round((doneCells.value / totalCells.value) * 100)))

const drawerCellUnits = computed(() => {
  if (!drawerCell.value) return []
  return units.value
    .filter((u) => u.item_id === drawerCell.value!.itemId && u.stage === drawerCell.value!.stage)
    .sort((a, b) => a.attempt_no - b.attempt_no)
})
const drawerTitle = computed(() => {
  if (!drawerCell.value) return ''
  const item = run.value?.roster.find((i) => i.id === drawerCell.value!.itemId)
  return `${item?.name || drawerCell.value.itemId} · ${drawerCell.value.stage}`
})

async function load() {
  loading.value = true
  try {
    const res = await getRun(runId)
    run.value = res.run
    units.value = res.units || []
  } catch (e) {
    ElMessage.error('读取运行失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  // 无 run 级 SSE 先例：非终态 3s 轮询（会话页同款 epoch 语义由整页加载覆盖）。
  timer = setInterval(() => {
    if (!isTerminal.value) load()
  }, 3000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})

async function doPause() {
  acting.value = true
  try {
    await pauseRun(runId)
    ElMessage.success('已在边界暂停（在途单元完成后停下）')
    await load()
  } catch (e) {
    ElMessage.error('暂停失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    acting.value = false
  }
}

async function doResume() {
  acting.value = true
  try {
    const extra = run.value?.pause_reason === 'budget_exceeded' ? extraBudget.value : 0
    await resumeRun(runId, extra)
    resumeDialog.value = false
    ElMessage.success('已续跑')
    await load()
  } catch (e) {
    ElMessage.error('续跑失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    acting.value = false
  }
}

async function doAbort() {
  try {
    await ElMessageBox.confirm('中止后未完成的单元不再执行（已完成产物保留）。确认中止？', '中止运行', {
      type: 'warning', confirmButtonText: '中止', cancelButtonText: '取消',
    })
  } catch {
    return
  }
  acting.value = true
  try {
    await abortRun(runId)
    ElMessage.success('已中止')
    await load()
  } catch (e) {
    ElMessage.error('中止失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    acting.value = false
  }
}

function openDrawer(itemId: string, stage: string) {
  drawerCell.value = { itemId, stage }
  drawerOpen.value = true
}

function openReject(u: WorkflowUnit) {
  rejectTarget.value = u
  rejectReason.value = ''
  rejectDialog.value = true
}

async function confirmReject() {
  if (!rejectTarget.value) return
  await onUnitAction(rejectTarget.value, 'reject', rejectReason.value.trim())
  rejectDialog.value = false
}

async function onUnitAction(u: WorkflowUnit, action: 'accept' | 'reject' | 'reroll' | 'skip', reason = '') {
  acting.value = true
  try {
    await unitAction(u.id, action, reason)
    const label: Record<string, string> = { accept: '已采纳', reject: '已驳回', reroll: '已重摇（新一次尝试已排队）', skip: '已跳过' }
    ElMessage.success(label[action] || action)
    await load()
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    acting.value = false
  }
}

function toggleReview() {
  reviewOpen.value = !reviewOpen.value
}

async function doFeedback() {
  feedbacking.value = true
  try {
    const res = await runFeedback(runId)
    feedbackDraft.value = res.draft
    feedbackDialog.value = true
  } catch (e) {
    ElMessage.error('生成配方草案失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    feedbacking.value = false
  }
}

async function saveDraft() {
  if (!run.value) return
  savingDraft.value = true
  try {
    const res = await saveRecipe(run.value.workdir, run.value.recipe_id, feedbackDraft.value)
    ElMessage.success(`配方 ${res.id} 已更新到 v${res.version}（后续新 run 使用）`)
    feedbackDialog.value = false
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    savingDraft.value = false
  }
}

// ---- 展示小工具 ----

function parseCheck(u: WorkflowUnit): UnitCheckResult | null {
  if (!u.check_result) return null
  try {
    return JSON.parse(u.check_result) as UnitCheckResult
  } catch {
    return null
  }
}
function checkItems(u: WorkflowUnit): { pass: boolean; reason: string }[] {
  const cr = parseCheck(u)
  if (!cr) return []
  const out: { pass: boolean; reason: string }[] = []
  for (const it of cr.program?.items || []) out.push({ pass: it.pass, reason: `程序[${it.key}] ${it.reason || ''}` })
  for (const it of cr.judge?.items || []) out.push({ pass: it.pass, reason: `评委 ${it.item}：${it.reason || ''}` })
  if (cr.judge?.err) out.push({ pass: false, reason: `评委不可用：${cr.judge.err}` })
  if (cr.drift?.flag) out.push({ pass: true, reason: `漂移离群（距离 ${cr.drift.distance}，样本 ${cr.drift.samples}）` })
  if (cr.error) out.push({ pass: false, reason: cr.error })
  return out
}
function driftFlag(u: WorkflowUnit): boolean {
  return !!parseCheck(u)?.drift?.flag
}
function upgradeAgent(u: WorkflowUnit): string {
  return parseCheck(u)?.upgrade?.agent_id || ''
}
function isImage(p?: string): boolean {
  return !!p && /\.(png|jpe?g|webp|gif)$/i.test(p)
}

function runStatusType(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'running': return 'warning'
    case 'paused': return 'warning'
    case 'done': return 'success'
    case 'failed': return 'danger'
    default: return 'info'
  }
}
function runStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队中', running: '运行中', paused: '已暂停', done: '已完成', failed: '失败', aborted: '已中止',
  }
  return map[s] || s
}
function pauseReasonText(r?: string): string {
  const map: Record<string, string> = {
    budget_exceeded: '预算超限', user: '用户暂停', circuit_breaker: '早期熔断',
  }
  return map[r || ''] || (r || '')
}
function unitStatusType(s: string): 'info' | 'warning' | 'success' | 'danger' | 'primary' {
  switch (s) {
    case 'done': return 'success'
    case 'running': return 'warning'
    case 'needs_review': return 'warning'
    case 'failed': return 'danger'
    case 'skipped': return 'info'
    default: return 'info'
  }
}
function unitStatusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队中', running: '运行中', done: '已采纳', needs_review: '待审', failed: '失败', skipped: '已跳过',
  }
  return map[s] || s
}
</script>
