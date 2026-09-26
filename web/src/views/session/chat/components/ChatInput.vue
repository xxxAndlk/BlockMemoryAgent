<script setup lang="ts">
import { ref, computed, reactive, watch, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import type { WireImage, SessionGear, SessionThinking, TrustMode, Session } from '@/types'
import { setTrustMode, setSessionGear, setSessionThinking } from '@/api/session'
import { useModelSelection } from '@/composables/useModelSelection'
import WorkDirPicker from '@/components/WorkDirPicker.vue'
import { useWorkDir } from '@/composables/useWorkDir'

const props = defineProps<{
  loading?: boolean
  /** 当前会话：绑定后档位/思考强度/信任模式下拉变为"即时切换后端"（原本在会话头，2026-09-16 移入对话栏） */
  session?: Session | null
  sessionActive?: boolean
  /** 注入提示开关：仅真 running 为 true。sessionActive 还含 awaiting_clarify/awaiting_child
   * （会话活着、可发送），但那时发送是澄清答复/唤醒新消息，不是即时注入当前执行。 */
  injectHint?: boolean
  inputTokens?: number
  outputTokens?: number
  /** 是否绑定了会话（有 activeSession）。区别于 sessionActive=运行中：
   * 已完成的会话同样"有本会话目录"，不能因为没在跑就显示成"新会话目录"。 */
  sessionBound?: boolean
  /** 工作目录：有会话=本会话目录；无会话=新会话默认目录（空串=服务端默认工作目录 agent.default_workdir） */
  workDir?: string
  /** 目录保存中（请求在途，按钮转圈） */
  workDirSaving?: boolean
  /** 已有工作目录（选择器里可直接复用的旧目录） */
  knownDirs?: string[]
}>()

const emit = defineEmits<{
  (e: 'submit', content: string, images: WireImage[], gear: SessionGear, thinking: SessionThinking): void
  (e: 'new-session'): void
  (e: 'update-workdir', dir: string): void
}>()

const content = ref('')

// 服务端默认工作目录（新建会话不选目录时的落盘根）：会话页挂载时经 /api/capabilities 填好，
// 这里只读来渲染目录选择器占位——用户建会话前就知道文件会写到哪个目录。
const { defaultDir } = useWorkDir()

// 待澄清语境（2026-09-20）：会话 awaiting_clarify 时发送 = 回答上方问题（走 /clarify），
// 不是下新指令。placeholder 与边框高亮跟着语境变，用户不必猜此刻该在哪输入。
const awaitingClarify = computed(() => props.session?.status === 'awaiting_clarify')
const placeholderText = computed(() =>
  awaitingClarify.value
    ? '回答上方问题，回车发送（Shift+Enter 换行）'
    : '向 AI 下达命令…（Enter 发送，Shift+Enter 换行，可直接粘贴图片）',
)

// 执行档位三档（TODO #14 三档全手动，2026-09-16 由会话头移入对话栏）：
// 未绑定会话 = 创建选择（localStorage 记忆）；已绑定 = 即时 POST 切档，下一轮按新档选角色。
const GEAR_STORAGE_KEY = 'bma:newSessionGear'
const GEAR_LABEL: Record<SessionGear, string> = { fast: '快速', daily: '日常', cluster: '集群' }
function normalizeGear(g: string | null | undefined): SessionGear {
  // 历史值 'auto'（规则自动选档，已退役）/未知值：回落默认日常档。
  return g === 'fast' || g === 'daily' || g === 'cluster' ? g : 'daily'
}
const gear = ref<SessionGear>(normalizeGear(localStorage.getItem(GEAR_STORAGE_KEY)))
// 绑定会话后以会话快照回显（gear 空 = 未设置，显示默认 daily）。
watch(
  () => [props.sessionBound, props.session?.gear] as const,
  ([bound, g]) => { if (bound) gear.value = normalizeGear(g) },
  { immediate: true },
)

async function onGearChange(v: SessionGear) {
  gear.value = v
  if (!props.sessionBound || !props.session) {
    localStorage.setItem(GEAR_STORAGE_KEY, v) // 创建选择：下次开新会话默认上次所选
    return
  }
  try {
    await setSessionGear(props.session.id, v)
    ElMessage.success(`执行档位已切换为${GEAR_LABEL[v]}档（即时生效，下一轮按新档选角色）`)
  } catch (e) {
    ElMessage.error('切换执行档位失败：' + (e instanceof Error ? e.message : String(e)))
    gear.value = normalizeGear(props.session.gear)
  }
}

// 会话级思考强度（2026-09-16）：空串 = 跟随角色默认；未绑定 = 随创建请求下发（localStorage 记忆），
// 已绑定 = 即时 POST，下一次 LLM 调用生效（热）。只影响本会话顶层 Agent。
const THINKING_STORAGE_KEY = 'bma:newSessionThinking'
function normalizeThinking(t: string | null | undefined): SessionThinking {
  return t === 'off' || t === 'low' || t === 'medium' || t === 'high' ? t : ''
}
const thinking = ref<SessionThinking>(normalizeThinking(localStorage.getItem(THINKING_STORAGE_KEY)))
watch(
  () => [props.sessionBound, props.session?.thinking] as const,
  ([bound, t]) => { if (bound) thinking.value = normalizeThinking(t) },
  { immediate: true },
)

async function onThinkingChange(v: SessionThinking) {
  thinking.value = v
  if (!props.sessionBound || !props.session) {
    localStorage.setItem(THINKING_STORAGE_KEY, v)
    return
  }
  try {
    await setSessionThinking(props.session.id, v)
    ElMessage.success(`思考强度已切换为 ${v === '' ? '跟随角色默认' : v}（下一次 LLM 调用生效）`)
  } catch (e) {
    ElMessage.error('切换思考强度失败：' + (e instanceof Error ? e.message : String(e)))
    thinking.value = normalizeThinking(props.session.thinking)
  }
}

// 信任模式（TODO 第10⑥ 三级信任，对标 Codex；2026-09-16 由会话头移入对话栏）：
// 仅绑定会话可见，切换即时 POST 后端，下一工具调用生效。
// 本地值以会话快照回显（trust_mode 空 = 后端回退现网语义，显示 full-auto）。
const trustMode = ref<TrustMode>('full-auto')
watch(
  () => props.session?.trust_mode,
  (m) => { trustMode.value = m === 'suggest' || m === 'auto-edit' ? m : 'full-auto' },
  { immediate: true },
)

async function onTrustModeChange(mode: TrustMode) {
  if (!props.session) return
  try {
    await setTrustMode(props.session.id, mode)
    trustMode.value = mode
    ElMessage.success(`信任模式已切换为 ${mode}（下一工具调用生效）`)
  } catch (e) {
    ElMessage.error('切换信任模式失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

// 待发送图片（任务 111 Web 侧同步）：顺序与 [image:N] 占位符编号升序对齐
// （顺序对齐非解析对齐，与 TUI 同策略）。base64 不带 data: 前缀。
const pendingImages = ref<WireImage[]>([])
// 与后端 agent.ParseWireImages 同规则：最多 4 张、单张解码后 ≤4MiB。
const MAX_IMAGES = 4
const MAX_IMAGE_BYTES = 4 << 20
const MIME_WHITELIST = ['image/png', 'image/jpeg', 'image/gif', 'image/webp']

const canSend = computed(() => (content.value.trim().length > 0 || pendingImages.value.length > 0) && !props.loading)

// token 数值格式化：≥1000 显示为 1.2k，否则原样显示。
function fmtTokens(n: number): string {
  if (!n) return '0'
  if (n < 1000) return String(n)
  return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'
}


// 把图片加入待发送列表并在光标处插入 [image:N] 占位符。
async function addImageFiles(files: File[]) {
  for (const f of files) {
    if (!MIME_WHITELIST.includes(f.type)) {
      ElMessage.warning(`不支持的图片格式 ${f.type || '(未知)'}，仅支持 png/jpeg/gif/webp`)
      continue
    }
    if (f.size > MAX_IMAGE_BYTES) {
      ElMessage.warning('单张图片超过大小上限（4MiB）')
      continue
    }
    if (pendingImages.value.length >= MAX_IMAGES) {
      ElMessage.warning(`单条消息最多携带 ${MAX_IMAGES} 张图片`)
      break
    }
    const dataUrl = await new Promise<string>((resolve, reject) => {
      const reader = new FileReader()
      reader.onload = () => resolve(reader.result as string)
      reader.onerror = () => reject(reader.error)
      reader.readAsDataURL(f)
    })
    // dataURL 形如 data:image/png;base64,xxxx —— 剥前缀取纯 base64。
    const base64 = dataUrl.split(',', 2)[1] || ''
    if (!base64) continue
    pendingImages.value.push({ mime_type: f.type, data: base64 })
    insertPlaceholder(`[image:${pendingImages.value.length}]`)
  }
}

// 在 textarea 光标处插入占位符文本。
function insertPlaceholder(text: string) {
  const el = textareaRef.value?.textarea as HTMLTextAreaElement | undefined
  if (!el || el.selectionStart == null) {
    content.value += text
    return
  }
  const start = el.selectionStart
  const end = el.selectionEnd
  content.value = content.value.slice(0, start) + text + content.value.slice(end)
}

// 粘贴剪贴板图片（对齐 TUI Alt+V 的粘贴入口）。
function onPaste(e: ClipboardEvent) {
  const files: File[] = []
  for (const item of e.clipboardData?.items || []) {
    if (item.kind === 'file' && item.type.startsWith('image/')) {
      const f = item.getAsFile()
      if (f) files.push(f)
    }
  }
  if (files.length) {
    e.preventDefault() // 阻止把图片当文件名文本插入
    addImageFiles(files)
  }
}

// 预览图缩略 data URL（img src 直接可用）。
function previewUrl(img: WireImage) {
  return `data:${img.mime_type};base64,${img.data}`
}

function removeImage(idx: number) {
  // 移除预览图；已插入的占位符文本不回收（与 TUI 手删占位符不移除图片本体
  // 同一容忍策略：外观错位可接受，编号不重排以保持与已插占位符一致）。
  pendingImages.value.splice(idx, 1)
}

function pickFiles() {
  fileRef.value?.click()
}

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  if (input.files?.length) addImageFiles(Array.from(input.files))
  input.value = '' // 允许重复选择同一文件
}

const textareaRef = ref()
const fileRef = ref<HTMLInputElement>()

// 工作目录：有会话时是"本会话目录"（改动经父级落库保存，下一回合生效），
// 无会话时是新会话默认目录（父级写全局默认）。不再是模块级共享 ref——
// 那会让所有会话共显一个值、在会话页改动只影响"下一条新会话"却看起来像改了当前会话。
const workDirLabel = computed(() => (props.sessionBound ? '本会话目录' : '新会话目录'))
/**
 * 刚改完目录的短提示（8s 后自动消失）：目录在每回合开始时读取，正在执行的工具调用
 * 仍按旧目录解析——只在"确实改过"时提示，常驻会变成噪音。
 */
const workDirJustChanged = ref(false)
let workDirHintTimer: number | undefined
watch(
  () => props.workDir,
  (v, prev) => {
    if (prev === undefined || v === prev) return // 首次渲染/未变化
    if (!props.sessionActive) return // 新会话尚未创建，无"当前回合"概念
    workDirJustChanged.value = true
    window.clearTimeout(workDirHintTimer)
    workDirHintTimer = window.setTimeout(() => {
      workDirJustChanged.value = false
    }, 8000)
  },
)
onUnmounted(() => window.clearTimeout(workDirHintTimer))
const workDirHint = computed(() => workDirJustChanged.value)

// ── 模型选择（对话栏右下弹层）：角色 → 模型 → 思考强度；切换/新增落 config/models.json ──
const {
  catalog, loading: modelsLoading,
  selectedRole, selectedThinking,
  ensureLoaded, doSwitch, doAdd,
} = useModelSelection()

const modelPopoverVisible = ref(false)
const switching = ref(false)
// 选中的目标模型条目 ID（切换动作的提交值；打开弹层/切角色时预选当前生效值）。
const selectedModelId = ref('')

const currentRoleStatus = computed(
  () => catalog.value?.roles.find((r) => r.role_id === selectedRole.value) ?? null,
)

// 切换候选：selectable_roles 白名单按目标角色过滤（缺省=全员可用）。
const switchableModels = computed(() =>
  (catalog.value?.models ?? []).filter(
    (m) => !m.selectable_roles || m.selectable_roles.includes(selectedRole.value),
  ),
)

watch(modelPopoverVisible, (v) => {
  if (v) ensureLoaded()
})

// 切角色时把模型/思考档预置为该角色当前生效值（思考档空=跟随角色默认）。
watch(selectedRole, () => {
  const role = currentRoleStatus.value
  selectedModelId.value = role?.model_id ?? ''
  selectedThinking.value = role?.bound ? role.thinking || '' : ''
})

async function onSwitchModel() {
  if (!selectedModelId.value) return
  switching.value = true
  try {
    const resp = await doSwitch(selectedModelId.value)
    ElMessage.success(`已切换为 ${resp.model}（domain/叶子下次派发、meta 下一会话生效）`)
    modelPopoverVisible.value = false
  } catch (e) {
    ElMessage.error(`切换失败: ${e instanceof Error ? e.message : String(e)}`)
  } finally {
    switching.value = false
  }
}

// 新增模型对话框（4 字段落盘 models.json，免重启）。
const addDialogVisible = ref(false)
const adding = ref(false)
const addForm = reactive({ provider: 'anthropic', model: '', api_key: '', base_url: '' })

async function onAddModel() {
  if (!addForm.provider.trim() || !addForm.model.trim()) {
    ElMessage.warning('provider 与 model 必填')
    return
  }
  adding.value = true
  try {
    const resp = await doAdd({
      provider: addForm.provider.trim(),
      model: addForm.model.trim(),
      api_key: addForm.api_key.trim(),
      base_url: addForm.base_url.trim(),
    })
    ElMessage.success(`已新增 ${resp.id} 到 models.json`)
    addDialogVisible.value = false
    selectedModelId.value = resp.id
    addForm.model = ''
    addForm.api_key = ''
    addForm.base_url = ''
  } catch (e) {
    ElMessage.error(`新增失败: ${e instanceof Error ? e.message : String(e)}`)
  } finally {
    adding.value = false
  }
}

function handleSubmit() {
  if (!canSend.value) return
  emit('submit', content.value.trim(), pendingImages.value.slice(), gear.value, thinking.value)
  content.value = ''
  pendingImages.value = []
}

function onKeydown(e: KeyboardEvent) {
  // 中文输入法组词回车不发送（isComposing）；Shift+Enter 换行走默认行为，Enter 直接发送
  if (e.isComposing || e.keyCode === 229) return
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    handleSubmit()
  }
}
</script>

<template>
  <div class="border-t border-line bg-card px-6 py-3">
    <!-- 模式开关 + 快捷标签 + Token 计数 -->
    <div class="flex items-center justify-between mb-2 flex-wrap gap-2">
      <div class="flex items-center gap-4 text-xs text-ink-2 flex-1 min-w-0">
        <div class="flex items-center gap-2 min-w-0 max-w-[26rem] workdir-cell">
          <span class="text-ink-2 whitespace-nowrap">{{ workDirLabel }}</span>
          <WorkDirPicker :model-value="workDir || ''" :existing="knownDirs"
                         :placeholder="defaultDir || '默认目录'"
                         @update:model-value="(d: string) => emit('update-workdir', d)" />
          <el-icon v-if="workDirSaving" class="animate-spin text-ink-3 shrink-0"><Loading /></el-icon>
          <span v-if="workDirHint" class="text-[10px] text-ink-3 whitespace-nowrap shrink-0"
                title="目录在每回合开始时读取，正在执行的工具调用仍按旧目录">下回合生效</span>
        </div>
        <span v-if="sessionActive" class="text-green-400 flex items-center gap-1">
          <span class="w-1.5 h-1.5 rounded-full bg-green-500 animate-pulse"></span>
          会话已连接
        </span>
        <!-- 运行中的输入语义：不再是"排到队尾"，而是即时注入当前执行（仅真 running；
             待澄清是答复、挂起等子是唤醒新消息，都不是注入，故用 injectHint 而非 sessionActive） -->
        <span v-if="injectHint" class="text-[10px] text-ink-3 whitespace-nowrap shrink-0"
              title="运行中发送的指令会投进当前 Agent 的邮箱：等待子 Agent 时立即读到并重新规划，其他阶段在当前步骤结束后生效">
          运行中发送＝即时注入当前执行
        </span>
        <span v-else class="text-ink-2">将创建新会话</span>
      </div>
      <div class="flex items-center gap-3 text-xs">
        <el-button size="small" plain class="!bg-transparent !border-line !text-ink-2 hover:!text-white"
                   @click="emit('new-session')">
          <el-icon class="mr-1"><Plus /></el-icon> 新建会话
        </el-button>
      </div>
    </div>

    <!-- 待发送图片预览（可移除） -->
    <div v-if="pendingImages.length" class="flex gap-2 mb-2 flex-wrap">
      <div v-for="(img, i) in pendingImages" :key="i"
           class="relative w-14 h-14 rounded border border-line overflow-hidden group">
        <img :src="previewUrl(img)" class="w-full h-full object-cover" alt="待发送图片" />
        <button @click="removeImage(i)"
                class="absolute inset-0 hidden group-hover:flex items-center justify-center bg-black/60 text-red-400 text-xs">
          移除
        </button>
      </div>
    </div>

    <!-- 输入区：圆角容器内 上=文本域 下=工具行（左：模型/图片；右：发送），对齐主流对话产品。
         待澄清时边框琥珀高亮（此刻发送=回答上方问题，见 script 注释） -->
    <div class="chat-box flex flex-col rounded-xl border bg-page transition-colors"
         :class="awaitingClarify ? 'border-amber-400 dark:border-yellow-600' : 'border-line focus-within:border-primary'">
      <el-input ref="textareaRef" v-model="content"
                type="textarea"
                :autosize="{ minRows: 2, maxRows: 8 }"
                :placeholder="placeholderText"
                resize="none"
                class="chat-input"
                @keydown="onKeydown"
                @paste="onPaste" />
      <div class="flex items-center gap-1 px-2 pb-2">
        <!-- 模型选择弹层（工具行左）：角色 → 模型 → 思考强度 -->
        <el-popover v-model:visible="modelPopoverVisible" placement="top-start" :width="380" trigger="click">
          <template #reference>
            <el-button text size="small" class="!text-ink-2 hover:!text-primary max-w-52 !px-2"
                       title="选择模型（角色 / 模型 / 思考强度，可新增模型）">
              <el-icon><Coin /></el-icon>
              <span class="ml-1 truncate text-xs">{{ currentRoleStatus?.model || '模型' }}</span>
            </el-button>
          </template>
          <div class="space-y-2 text-xs">
            <div>
              <div class="mb-1 text-ink-2">角色</div>
              <el-select v-model="selectedRole" size="small" class="w-full"
                         placeholder="选择角色" :loading="modelsLoading">
                <el-option v-for="r in catalog?.roles || []" :key="r.role_id" :value="r.role_id"
                           :label="`${r.role_id}（${r.provider}/${r.model}）`" />
              </el-select>
            </div>
            <div>
              <div class="mb-1 text-ink-2">模型</div>
              <el-select v-model="selectedModelId" size="small" class="w-full" filterable
                         placeholder="选择模型" :loading="modelsLoading">
                <el-option v-for="m in switchableModels" :key="m.id" :value="m.id"
                           :label="`${m.id} · ${m.provider}/${m.model}`" />
              </el-select>
            </div>
            <div>
              <div class="mb-1 text-ink-2">思考强度</div>
              <el-select v-model="selectedThinking" size="small" class="w-full">
                <el-option value="" label="跟随角色默认" />
                <el-option v-for="t in ['off', 'low', 'medium', 'high']" :key="t" :value="t" :label="t" />
              </el-select>
            </div>
            <div class="flex items-center justify-between pt-1">
              <el-button link type="primary" size="small" @click="addDialogVisible = true">＋ 新增模型</el-button>
              <el-button type="primary" size="small" :loading="switching"
                         :disabled="!selectedModelId" @click="onSwitchModel">
                切换
              </el-button>
            </div>
          </div>
        </el-popover>
        <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-2"
                   :disabled="pendingImages.length >= 4"
                   title="添加图片（或直接粘贴截图）"
                   @click="pickFiles">
          <el-icon><Picture /></el-icon>
        </el-button>
        <span class="ml-auto mr-1 text-[11px] text-ink-3 font-mono hidden sm:inline"
              :title="`输入 ${props.inputTokens || 0} / 输出 ${props.outputTokens || 0} tokens`">
          ↑{{ fmtTokens(props.inputTokens || 0) }} ↓{{ fmtTokens(props.outputTokens || 0) }}
        </span>
        <!-- 信任模式（仅已绑定会话）：suggest=变更逐条审批 / auto-edit=命令与破坏性工具审批 / full-auto=全自主 -->
        <el-select v-if="sessionBound && session" :model-value="trustMode" size="small" class="!w-32 shrink-0"
                   title="信任模式：变更类操作的审批档位，切换下一工具调用生效"
                   @update:model-value="onTrustModeChange($event as TrustMode)">
          <el-option value="suggest" label="suggest 逐条审批" />
          <el-option value="auto-edit" label="auto-edit 审命令" />
          <el-option value="full-auto" label="full-auto 全自主" />
        </el-select>
        <!-- 执行档位（TODO #14 三档全手动）：未绑会话=创建时选档；已绑会话=即时切档，下一轮按新档选角色 -->
        <el-select v-model="gear" size="small" class="!w-24 shrink-0"
                   title="执行档位：快速档=文档助手直达，日常档=DomainAgent 直接执行，集群档=Meta 全装编排"
                   @change="onGearChange($event as SessionGear)">
          <el-option value="fast" label="快速档" />
          <el-option value="daily" label="日常档" />
          <el-option value="cluster" label="集群档" />
        </el-select>
        <!-- 思考强度（2026-09-16 会话级）：空=跟随角色默认；只影响本会话顶层 Agent，切换下一次 LLM 调用生效 -->
        <el-select v-model="thinking" size="small" class="!w-28 shrink-0"
                   title="思考强度（本会话顶层 Agent）：空=跟随角色默认；运行中可改，下一次 LLM 调用生效"
                   @change="onThinkingChange($event as SessionThinking)">
          <el-option value="" label="思考 跟随角色" />
          <el-option value="off" label="思考 off" />
          <el-option value="low" label="思考 low" />
          <el-option value="medium" label="思考 medium" />
          <el-option value="high" label="思考 high" />
        </el-select>
        <el-button type="primary" size="small" round
                   :loading="loading"
                   :disabled="!canSend"
                   class="!bg-primary !border-primary hover:!bg-[var(--bma-primary-hover)] hover:!border-[var(--bma-primary-hover)]"
                   @click="handleSubmit">
          <el-icon class="mr-1"><Promotion /></el-icon> 发送
        </el-button>
      </div>
    </div>
    <input ref="fileRef" type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple class="hidden" @change="onFileChange" />

    <!-- 新增模型对话框：4 字段落盘 config/models.json，免重启生效 -->
    <el-dialog v-model="addDialogVisible" title="新增模型（写入 config/models.json，免重启）" width="460" append-to-body>
      <div class="space-y-3 text-xs">
        <div>
          <div class="mb-1 text-ink-2">provider *</div>
          <el-select v-model="addForm.provider" class="w-full" filterable allow-create default-first-option
                     placeholder="anthropic / openai-chat / openai-responses / ollama">
            <el-option v-for="p in ['anthropic', 'openai-chat', 'openai-responses', 'ollama']" :key="p" :value="p" :label="p" />
          </el-select>
        </div>
        <div>
          <div class="mb-1 text-ink-2">model *</div>
          <el-input v-model="addForm.model" placeholder="如 glm-5.3-flash" />
        </div>
        <div>
          <div class="mb-1 text-ink-2">api_key</div>
          <el-input v-model="addForm.api_key" show-password placeholder="密钥或 ${ENV} 引用（可空）" />
        </div>
        <div>
          <div class="mb-1 text-ink-2">base_url</div>
          <el-input v-model="addForm.base_url" placeholder="自定义端点（可空，支持 ${ENV}）" />
        </div>
      </div>
      <template #footer>
        <el-button size="small" @click="addDialogVisible = false">取消</el-button>
        <el-button type="primary" size="small" :loading="adding" @click="onAddModel">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
/* 容器（.chat-box）负责边框/聚焦态，文本域自身去边框 */
:deep(.chat-box .el-textarea__inner) {
  background-color: transparent !important;
  border: none;
  color: var(--bma-text);
  box-shadow: none !important;
  font-size: 13px;
  padding: 10px 12px 4px;
}
:deep(.workdir-cell .el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
  padding: 0 8px;
  min-height: 28px;
}
:deep(.workdir-cell .el-input__inner) {
  font-size: 12px;
}
.hidden {
  display: none;
}
</style>
