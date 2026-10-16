<template>
  <div class="bg-card border border-line rounded-card p-4 mb-4">
    <div class="flex items-center justify-between mb-3 flex-wrap gap-2">
      <div>
        <div class="text-sm font-bold">联系表审查（横向对比才暴露漂移）</div>
        <div class="text-xs text-ink-2 mt-0.5">
          按阶段看整批采纳/待审产物拼贴；<span class="text-orange-400">橙框=待审/评委未过</span>，
          <span class="text-yellow-400">黄标=漂移离群</span>，逐格 accept / reject+理由 / 重摇。
        </div>
      </div>
      <div class="w-[180px] shrink-0">
        <el-select v-model="stage" size="small" class="!w-full">
          <el-option v-for="s in stagesWithArtifacts" :key="s" :label="s" :value="s" />
        </el-select>
      </div>
    </div>
    <div v-if="!cells.length" class="text-sm text-ink-2 py-8 text-center">该阶段还没有可展示的产物。</div>
    <div v-else class="grid gap-3" :style="{ gridTemplateColumns: 'repeat(auto-fill, minmax(200px, 1fr))' }">
      <div v-for="c in cells" :key="c.unit.id"
           class="border rounded-card overflow-hidden bg-transparent"
           :class="borderClass(c.unit)">
        <div class="relative">
          <img :src="workspaceUrl(sessionId, c.unit.artifact_path!)" class="w-full h-[180px] object-contain bg-black/10"
               loading="lazy" />
          <el-tag v-if="driftFlag(c.unit)" size="small" type="warning" effect="dark"
                  class="!absolute top-1 left-1">漂移</el-tag>
          <el-tag size="small" :type="statusTagType(c.unit.status)" effect="dark" class="!absolute top-1 right-1">
            {{ statusText(c.unit.status) }}
          </el-tag>
        </div>
        <div class="px-2 py-1.5">
          <div class="text-xs truncate" :title="c.itemName">{{ c.itemName }}</div>
          <div v-if="c.unit.reject_reason" class="text-[11px] text-orange-400 truncate" :title="c.unit.reject_reason">
            {{ c.unit.reject_reason }}
          </div>
          <div class="flex items-center gap-1 mt-1 flex-wrap">
            <template v-if="c.unit.status === 'needs_review'">
              <el-button size="small" type="success" plain text @click="emit('action', c.unit, 'accept')">采纳</el-button>
              <el-button size="small" type="danger" plain text @click="openReject(c.unit)">驳回</el-button>
              <el-button size="small" plain text @click="emit('action', c.unit, 'reroll')">重摇</el-button>
            </template>
            <el-button size="small" plain text @click="emit('open-cell', c.unit.item_id, c.unit.stage)">详情</el-button>
          </div>
        </div>
      </div>
    </div>

    <el-dialog v-model="rejectDialog" title="驳回该单元" width="480px">
      <el-input v-model="rejectReason" type="textarea" :rows="3" placeholder="驳回理由（进配方演化输入）" />
      <template #footer>
        <el-button plain class="!bg-transparent !border-line !text-ink" @click="rejectDialog = false">取消</el-button>
        <el-button type="danger" :disabled="!rejectReason.trim()" @click="confirmReject">驳回</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { workspaceUrl } from '@/api/session'
import type { RosterItemInfo, UnitCheckResult, WorkflowUnit } from '@/api/workflow'

const props = defineProps<{
  sessionId: string
  items: RosterItemInfo[]
  stages: string[]
  units: WorkflowUnit[]
  acting: boolean
}>()

const emit = defineEmits<{
  (e: 'action', unit: WorkflowUnit, action: 'accept' | 'reject' | 'reroll' | 'skip', reason?: string): void
  (e: 'open-cell', itemId: string, stage: string): void
}>()

const stage = ref('')
const rejectDialog = ref(false)
const rejectReason = ref('')
const rejectTarget = ref<WorkflowUnit | null>(null)

const latestByCell = computed(() => {
  const map = new Map<string, WorkflowUnit>()
  for (const u of props.units) {
    const key = `${u.item_id}\u0000${u.stage}`
    const cur = map.get(key)
    if (!cur || u.attempt_no > cur.attempt_no) map.set(key, u)
  }
  return map
})

/** 有产物的阶段（联系表只展示能看图的东西）。 */
const stagesWithArtifacts = computed(() => {
  const seen = new Set<string>()
  for (const u of latestByCell.value.values()) {
    if (u.artifact_path && /\.(png|jpe?g|webp|gif)$/i.test(u.artifact_path)) seen.add(u.stage)
  }
  return props.stages.filter((s) => seen.has(s))
})

watch(stagesWithArtifacts, (list) => {
  if (!stage.value || !list.includes(stage.value)) stage.value = list[0] || ''
}, { immediate: true })

const cells = computed(() => {
  if (!stage.value) return []
  const out: { unit: WorkflowUnit; itemName: string }[] = []
  for (const item of props.items) {
    const u = latestByCell.value.get(`${item.id}\u0000${stage.value}`)
    if (u?.artifact_path && /\.(png|jpe?g|webp|gif)$/i.test(u.artifact_path)) {
      out.push({ unit: u, itemName: item.name || item.id })
    }
  }
  return out
})

function parseCheck(u: WorkflowUnit): UnitCheckResult | null {
  if (!u.check_result) return null
  try {
    return JSON.parse(u.check_result) as UnitCheckResult
  } catch {
    return null
  }
}
function driftFlag(u: WorkflowUnit): boolean {
  return !!parseCheck(u)?.drift?.flag
}
function borderClass(u: WorkflowUnit): string {
  if (u.status === 'needs_review') return 'border-orange-400'
  if (u.status === 'failed') return 'border-red-500'
  if (driftFlag(u)) return 'border-yellow-400'
  return 'border-line'
}
function statusTagType(s: string): 'info' | 'warning' | 'success' | 'danger' {
  switch (s) {
    case 'done': return 'success'
    case 'needs_review': return 'warning'
    case 'failed': return 'danger'
    default: return 'info'
  }
}
function statusText(s: string): string {
  const map: Record<string, string> = {
    pending: '排队', running: '运行中', done: '已采纳', needs_review: '待审', failed: '失败', skipped: '跳过',
  }
  return map[s] || s
}

function openReject(u: WorkflowUnit) {
  rejectTarget.value = u
  rejectReason.value = ''
  rejectDialog.value = true
}
function confirmReject() {
  if (!rejectTarget.value) return
  emit('action', rejectTarget.value, 'reject', rejectReason.value.trim())
  rejectDialog.value = false
}
</script>
