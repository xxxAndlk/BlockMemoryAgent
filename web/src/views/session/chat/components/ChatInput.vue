<script setup lang="ts">
import { ref, computed, reactive, watch, nextTick, onUnmounted } from 'vue'
import { ElMessage } from 'element-plus'
import type { WireImage, SessionGear, SessionThinking, TrustMode, Session, SessionRunMode, SessionDirectiveSpec, SessionDirective } from '@/types'
import { setTrustMode, setSessionGear, setSessionThinking, setSessionPlugins, setSessionModel, getSessionDirective, setSessionDirective } from '@/api/session'
import { useModelSelection } from '@/composables/useModelSelection'
import { usePluginSelection } from '@/composables/usePluginSelection'
import { useRouter } from 'vue-router'
import WorkDirPicker from '@/components/WorkDirPicker.vue'
import { useWorkDir } from '@/composables/useWorkDir'
import { convertXlsxToCsv } from '@/utils/sheetConvert'
import { formatTokens } from '@/utils/tokens'

const props = defineProps<{
  loading?: boolean
  /** 当前会话：绑定后档位/思考强度/信任模式下拉变为"即时切换后端"（原本在会话头，2026-09-16 移入对话栏） */
  session?: Session | null
  sessionActive?: boolean
  /** 注入提示开关：仅真 running 为 true。sessionActive 还含 awaiting_clarify/awaiting_child
   * （会话活着、可发送），但那时发送是澄清答复/唤醒新消息，不是即时注入当前执行。 */
  injectHint?: boolean
  /** 运行中（session.status==='running' 且无销毁倒计时）：发送按钮切换为红色「停止」。 */
  running?: boolean
  /** 有真实待澄清提问（clarifyPending 非空）。注意 awaiting_clarify 状态亦承载"已停止"停放态，
   *  该态没有提问——两者必须靠本 prop 区分（停放态走普通发送=续跑，不是答复）。 */
  clarifyActive?: boolean
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
  (e: 'submit', content: string, images: WireImage[], files: File[], gear: SessionGear, thinking: SessionThinking, plugins: string[], directive: SessionDirectiveSpec | null, model: string): void
  (e: 'new-session'): void
  (e: 'update-workdir', dir: string): void
  (e: 'stop'): void
}>()

const content = ref('')

// 服务端默认工作目录（新建会话不选目录时的落盘根）：会话页挂载时经 /api/capabilities 填好，
// 这里只读来渲染目录选择器占位——用户建会话前就知道文件会写到哪个目录。
const { defaultDir } = useWorkDir()

// 待澄清语境（2026-09-20）：会话 awaiting_clarify 时发送 = 回答上方问题（走 /clarify），
// 不是下新指令。placeholder 与边框高亮跟着语境变，用户不必猜此刻该在哪输入。
// 注意 awaiting_clarify 亦承载"已停止"停放态（无提问）：只有真实提问（clarifyActive）
// 才按"答复"语境渲染，停放态按普通发送（消息即续跑）。
const awaitingClarify = computed(() => props.clarifyActive === true && props.session?.status === 'awaiting_clarify')
const placeholderText = computed(() =>
  awaitingClarify.value
    ? '回答上方问题，回车发送（Shift+Enter 换行）'
    : '向 AI 下达命令…（Enter 发送，Shift+Enter 换行；可直接粘贴/拖入图片与文件）',
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

// 会话级模型覆盖（2026-10-19）：未绑定 = 创建选择（localStorage 记忆）；已绑定 = 即时 POST
// 切换，下一次 LLM 调用生效；空串 = 跟随全局角色绑定（默认取全局配置，如 ark-glm-5.3-flash）。
// 只影响本会话顶层 Agent，不改全局角色绑定。
const MODEL_STORAGE_KEY = 'bma:newSessionModel'
const sessionModel = ref(localStorage.getItem(MODEL_STORAGE_KEY) || '')
// 绑定会话后以会话快照回显（model 空 = 跟随全局配置）。
watch(
  () => [props.sessionBound, props.session?.model] as const,
  ([bound, m]) => { if (bound) sessionModel.value = m || '' },
  { immediate: true },
)

async function onSessionModelChange(v: string) {
  if (v === sessionModel.value) return // 一键列表点选已生效项：不重复提交、不弹提示
  sessionModel.value = v
  if (!props.sessionBound || !props.session) {
    localStorage.setItem(MODEL_STORAGE_KEY, v) // 创建选择：下次开新会话默认上次所选
    return
  }
  try {
    await setSessionModel(props.session.id, v)
    ElMessage.success(v ? `会话模型已切换为 ${v}（仅本会话，下一次 LLM 调用生效）` : '会话模型已恢复跟随全局配置')
  } catch (e) {
    ElMessage.error('切换会话模型失败：' + (e instanceof Error ? e.message : String(e)))
    sessionModel.value = props.session.model || ''
  }
}

// ── 会话级指定插件（KIMI 式插件选择）：选中 = 优先/强制使用（后端预挂工具 + 提示词强约束），
// 不选 = 模型自行判断。未绑定会话 = 创建选择（localStorage 记忆，随创建请求携带）；
// 已绑定 = 勾选即 POST 即时切换（下一轮起跑生效），失败回滚勾选。
const PLUGINS_STORAGE_KEY = 'bma:newSessionPlugins'
const { selectable: selectablePlugins, loading: pluginsLoading, ensureLoaded: ensurePluginsLoaded } = usePluginSelection()
const pluginPopoverVisible = ref(false)
const pluginSaving = ref(false)
const router = useRouter()

function loadStoredPlugins(): string[] {
  try {
    const raw = JSON.parse(localStorage.getItem(PLUGINS_STORAGE_KEY) || '[]')
    return Array.isArray(raw) ? raw.filter((x): x is string => typeof x === 'string') : []
  } catch {
    return []
  }
}
const pinnedPlugins = ref<string[]>(loadStoredPlugins())
// 绑定会话后以会话快照回显（pinned_plugins 空 = 未指定）。
watch(
  () => [props.sessionBound, props.session?.pinned_plugins] as const,
  ([bound, ids]) => { if (bound) pinnedPlugins.value = [...(ids ?? [])] },
  { immediate: true },
)
watch(pluginPopoverVisible, (v) => { if (v) ensurePluginsLoaded() })

async function applyPinnedPlugins(next: string[]) {
  const prev = pinnedPlugins.value
  pinnedPlugins.value = next
  if (!props.sessionBound || !props.session) {
    localStorage.setItem(PLUGINS_STORAGE_KEY, JSON.stringify(next))
    return
  }
  pluginSaving.value = true
  try {
    await setSessionPlugins(props.session.id, next)
    ElMessage.success(next.length ? `已指定 ${next.length} 个插件（优先使用，下一轮生效）` : '已清空指定插件（模型自行判断）')
  } catch (e) {
    ElMessage.error('切换指定插件失败：' + (e instanceof Error ? e.message : String(e)))
    pinnedPlugins.value = prev
  } finally {
    pluginSaving.value = false
  }
}

function onTogglePlugin(id: string, checked: boolean) {
  const set = new Set(pinnedPlugins.value)
  if (checked) set.add(id)
  else set.delete(id)
  applyPinnedPlugins([...set])
}

function clearPlugins() {
  applyPinnedPlugins([])
}

function goPluginsPage() {
  pluginPopoverVisible.value = false
  router.push('/plugins')
}

// ── 会话指令模式（goal/loop，2026-10-18）：normal=普通单轮；goal=多轮自主推进直到完成
//（遇审核等待自动安排回查）；loop=按间隔重复执行本条指令，切回普通即停止。
// 未激活（新会话/停放态）：选择随创建/发送请求携带，localStorage 记忆；
// 会话激活：切换/改参防抖 300ms PUT 后端即时生效，失败提示并回滚到服务端当前状态。
// 绑定会话后以 GET /sessions/:id/directive 回填（不走会话快照；已结束的指令不回填）。
const RUN_MODE_STORAGE_KEY = 'bma.chat.runmode'
const RUN_MODE_PARAMS_STORAGE_KEY = 'bma.chat.runmode.params'
const ENDED_DIRECTIVE_STATUSES = new Set(['complete', 'failed', 'stopped'])

const directivePopoverVisible = ref(false)
const runMode = ref<SessionRunMode>(normalizeRunMode(localStorage.getItem(RUN_MODE_STORAGE_KEY)))
const completionCriterion = ref('')
const maxTurns = ref(0)
// 截止时间存 Date|null（el-date-picker datetime 无 value-format 时即 Date），提交时转 ISO。
const deadline = ref<Date | null>(null)
const loopIntervalMin = ref(10)
const loopUntil = ref('')

function normalizeRunMode(m: string | null | undefined): SessionRunMode {
  return m === 'goal' || m === 'loop' ? m : 'normal'
}

/** localStorage 记忆（照 GEAR_STORAGE_KEY 模式，模式存裸值、参数单独 try/catch JSON 序列化）。 */
function persistRunModeLocal() {
  try {
    localStorage.setItem(RUN_MODE_STORAGE_KEY, runMode.value)
    localStorage.setItem(RUN_MODE_PARAMS_STORAGE_KEY, JSON.stringify({
      completionCriterion: completionCriterion.value,
      maxTurns: maxTurns.value,
      deadline: deadline.value ? deadline.value.toISOString() : null,
      loopIntervalMin: loopIntervalMin.value,
      loopUntil: loopUntil.value,
    }))
  } catch { /* localStorage 不可用（隐私模式等）：静默回落内存态 */ }
}

function loadRunModeParams() {
  try {
    const raw = JSON.parse(localStorage.getItem(RUN_MODE_PARAMS_STORAGE_KEY) || 'null')
    if (!raw || typeof raw !== 'object') return
    if (typeof raw.completionCriterion === 'string') completionCriterion.value = raw.completionCriterion
    if (typeof raw.maxTurns === 'number') maxTurns.value = raw.maxTurns
    deadline.value = typeof raw.deadline === 'string' && raw.deadline ? new Date(raw.deadline) : null
    if (typeof raw.loopIntervalMin === 'number') loopIntervalMin.value = raw.loopIntervalMin
    if (typeof raw.loopUntil === 'string') loopUntil.value = raw.loopUntil
  } catch { /* 脏数据回落默认值 */ }
}
loadRunModeParams()

/** 组装指令载荷（normal 时调用方传 null，不进这里）；空串/0/null 字段省略，objective 不传（后端回落为消息内容）。 */
function buildDirectiveSpec(): SessionDirectiveSpec {
  const spec: SessionDirectiveSpec = { mode: runMode.value }
  if (runMode.value === 'goal') {
    if (completionCriterion.value.trim()) spec.completion_criterion = completionCriterion.value.trim()
    if (maxTurns.value > 0) spec.max_turns = maxTurns.value
    if (deadline.value) spec.deadline = deadline.value.toISOString()
  } else if (runMode.value === 'loop') {
    spec.loop_interval_sec = loopIntervalMin.value * 60
    if (loopUntil.value.trim()) spec.loop_until = loopUntil.value.trim()
  }
  return spec
}

/** 把服务端指令应用到表单（回填/失败回滚共用）；已结束的指令不回填（保持 normal，避免重新激活）。 */
function applyDirectiveView(d: SessionDirective | null, notifyEnded: boolean) {
  if (!d || d.mode === 'normal') {
    runMode.value = 'normal'
    return
  }
  if (ENDED_DIRECTIVE_STATUSES.has(d.status)) {
    if (notifyEnded && d.last_eval && endedNotifiedId !== d.id) {
      endedNotifiedId = d.id // 同一指令只提示一次
      ElMessage.info(`指令已结束：${d.last_eval}`)
    }
    return
  }
  runMode.value = d.mode
  completionCriterion.value = d.completion_criterion || ''
  maxTurns.value = d.max_turns || 0
  deadline.value = d.deadline ? new Date(d.deadline) : null
  loopIntervalMin.value = d.loop_interval_sec ? d.loop_interval_sec / 60 : 10
  loopUntil.value = d.loop_until || ''
}

/** 拉取服务端指令并应用；返回期间会话已切走则丢弃。 */
async function backfillDirective(id: string, notifyEnded: boolean) {
  try {
    const view = await getSessionDirective(id)
    if (props.session?.id !== id) return
    backfilling = true
    try {
      applyDirectiveView(view.directive, notifyEnded)
    } finally {
      backfilling = false
    }
  } catch { /* 静默：指令徽标侧独立轮询，这里失败不影响输入 */ }
}

// 回填/回滚守卫：期间对 runMode/参数的写入不再触发 PUT 或 localStorage 持久化。
let backfilling = false
// 已结束指令提示去重（按指令 id）。
let endedNotifiedId = ''
let directiveDebounceTimer: number | undefined

function scheduleDirectivePush() {
  if (!props.sessionActive || !props.session) return
  window.clearTimeout(directiveDebounceTimer)
  directiveDebounceTimer = window.setTimeout(() => {
    const sess = props.session
    if (!sess) return
    setSessionDirective(sess.id, buildDirectiveSpec()).catch((e) => {
      ElMessage.error('切换会话指令失败：' + (e instanceof Error ? e.message : String(e)))
      void backfillDirective(sess.id, false) // 回滚选择：恢复到服务端当前指令状态
    })
  }, 300)
}

watch(runMode, () => {
  if (backfilling) return
  if (props.sessionActive && props.session) scheduleDirectivePush()
  else persistRunModeLocal()
})
watch([completionCriterion, maxTurns, deadline, loopIntervalMin, loopUntil], () => {
  if (backfilling) return
  if (props.sessionActive && props.session) scheduleDirectivePush()
  else persistRunModeLocal()
})

// 绑定会话 → GET 指令回填；切回新会话 → 恢复 localStorage 记忆（清掉上一个会话的状态）。
// immediate：ChatView 视图切换会重挂本组件，挂载时若已绑定会话须立即回填。
watch(
  () => props.session?.id,
  (id, prev) => {
    if (id === prev) return
    window.clearTimeout(directiveDebounceTimer)
    backfilling = true
    try {
      runMode.value = 'normal'
      completionCriterion.value = ''
      maxTurns.value = 0
      deadline.value = null
      loopIntervalMin.value = 10
      loopUntil.value = ''
      if (!id) {
        // 新会话态：恢复 localStorage 记忆的指令选择
        runMode.value = normalizeRunMode(localStorage.getItem(RUN_MODE_STORAGE_KEY))
        loadRunModeParams()
      }
    } finally {
      backfilling = false
    }
    if (id) void backfillDirective(id, true)
  },
  { immediate: true },
)
onUnmounted(() => window.clearTimeout(directiveDebounceTimer))

// 待发送图片（任务 111 Web 侧同步）：顺序与 [image:N] 占位符编号升序对齐
// （顺序对齐非解析对齐，与 TUI 同策略）。base64 不带 data: 前缀。
const pendingImages = ref<WireImage[]>([])
// 与后端 agent.ParseWireImages 同规则：最多 4 张、单张解码后 ≤4MiB。
const MAX_IMAGES = 4
const MAX_IMAGE_BYTES = 4 << 20
const MIME_WHITELIST = ['image/png', 'image/jpeg', 'image/gif', 'image/webp']

// ── 待发送文件附件（2026-10-14）：粘贴/选择/拖入 SQL、Excel 等文件，发送时随消息
// multipart 上传到会话工作区 .bma/uploads/（Agent 按引用路径直读）；xlsx/xls 由前端
// SheetJS 转 CSV 随原文件一并上传（后端无 Excel 解析能力，ReadFile 读二进制是乱码）。──
interface PendingFileItem {
  id: number
  file: File
  name: string
  size: number
  /** 由 Excel 自动转写的 CSV（随原文件一并上传） */
  derived?: boolean
  /** Excel 转换进行中（禁发 + 转圈，防派生 CSV 缺席） */
  converting?: boolean
}
const pendingFiles = ref<PendingFileItem[]>([])
let pendingFileSeq = 0
// 与后端 agent 包上限同规则：最多 8 个、单文件 ≤64MiB、总量 ≤256MiB。
const MAX_FILES = 8
const MAX_FILE_BYTES = 64 << 20
const MAX_FILES_TOTAL_BYTES = 256 << 20

/** 转换完可发的文件；转换中整体禁发（防派生 CSV 缺席）。 */
const readyFiles = computed(() => pendingFiles.value.filter((f) => !f.converting))
const convertingAny = computed(() => pendingFiles.value.some((f) => f.converting))
// 只带文件不写字也允许发送（服务端会拼附件引用行，消息非空）。
const canSend = computed(
  () =>
    (content.value.trim().length > 0 || pendingImages.value.length > 0 || readyFiles.value.length > 0) &&
    !props.loading &&
    !convertingAny.value,
)

// token 数值格式化：统一走 @/utils/tokens（≥1000 显示为 k）。


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

// 通用附件收口（粘贴/选择/拖入共用）：命中图片白名单且 ≤4MiB 的走原图片链路
//（[image:N] 占位与 base64 传输不变）；其余任意文件进 pendingFiles。
function addAnyFiles(files: File[]) {
  const imageFiles: File[] = []
  const docFiles: File[] = []
  for (const f of files) {
    if (MIME_WHITELIST.includes(f.type) && f.size <= MAX_IMAGE_BYTES) {
      imageFiles.push(f)
      continue
    }
    if (f.type.startsWith('image/') && MIME_WHITELIST.includes(f.type)) {
      // 超过图片 4MiB 上限的图片：改按文件附件携带（原规则直接拒收，现在有更好去处）。
      ElMessage.info(`图片 ${f.name} 超过 4MiB，已按文件附件携带`)
    }
    docFiles.push(f)
  }
  if (imageFiles.length) void addImageFiles(imageFiles)
  for (const f of docFiles) addDocumentFile(f)
}

/** 单个文件进 pendingFiles（xlsx/xls 触发异步转 CSV 派生项）。 */
function addDocumentFile(f: File) {
  if (f.size > MAX_FILE_BYTES) {
    ElMessage.warning(`文件 ${f.name} 超过大小上限（64MiB）`)
    return
  }
  if (pendingFiles.value.length >= MAX_FILES) {
    ElMessage.warning(`单条消息最多携带 ${MAX_FILES} 个文件`)
    return
  }
  if (pendingFiles.value.reduce((n, it) => n + it.size, 0) + f.size > MAX_FILES_TOTAL_BYTES) {
    ElMessage.warning('附件总量超过上限（256MiB）')
    return
  }
  const item: PendingFileItem = { id: ++pendingFileSeq, file: f, name: f.name, size: f.size }
  pendingFiles.value.push(item)
  if (/\.(xlsx|xls)$/i.test(f.name)) {
    item.converting = true
    void convertXlsxToCsv(f).then((res) => {
      if (!pendingFiles.value.includes(item)) return // 转换途中已被移除
      item.converting = false
      if (res.skipped) {
        ElMessage.info(`${f.name}：${res.skipped}`)
        return
      }
      for (const csv of res.csvs) {
        if (pendingFiles.value.length >= MAX_FILES) {
          ElMessage.warning(`单条消息最多携带 ${MAX_FILES} 个文件，部分转换结果未加入`)
          break
        }
        pendingFiles.value.push({ id: ++pendingFileSeq, file: csv, name: csv.name, size: csv.size, derived: true })
      }
    })
  }
}

// 粘贴（对齐 TUI Alt+V 入口）：剪贴板里的图片与文件都收（此前只认图片，SQL/Excel
// 等文件被静默丢弃）；纯文本粘贴不拦，正常落输入框。
function onPaste(e: ClipboardEvent) {
  const files: File[] = []
  for (const item of e.clipboardData?.items || []) {
    if (item.kind !== 'file') continue
    const f = item.getAsFile()
    if (f) files.push(f)
  }
  if (files.length) {
    e.preventDefault() // 阻止把文件当文本插入
    addAnyFiles(files)
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

function removeFile(idx: number) {
  pendingFiles.value.splice(idx, 1)
}

function pickAnyFiles() {
  fileInputRef.value?.click()
}

function onAnyFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  if (input.files?.length) addAnyFiles(Array.from(input.files))
  input.value = '' // 允许重复选择同一文件
}

// 拖拽文件到 composer：仅对"文件拖拽"preventDefault（否则浏览器会直接打开文件），
// 纯文本拖拽维持默认行为（拖字进输入框等不受影响）。
function onDragOver(e: DragEvent) {
  if (e.dataTransfer?.types?.includes('Files')) e.preventDefault()
}

function onDrop(e: DragEvent) {
  const files = Array.from(e.dataTransfer?.files || [])
  if (!files.length) return
  e.preventDefault()
  addAnyFiles(files)
}

/** 附件大小展示（与后端引用行同档：B/KB/MB）。 */
function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

const textareaRef = ref()
const fileRef = ref<HTMLInputElement>()
const fileInputRef = ref<HTMLInputElement>()

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

// ── 本会话模型一键列表（2026-10-19 改版）：点选即切，替代旧"会话模型"下拉 ──
// 选择随会话各自记忆：已绑定 = 会话快照 session.model（后端持久化，重开会话仍在）；
// 未绑定 = 创建默认值（localStorage，下次开新会话带上）。
const modelFilter = ref('')
const sessionModelCandidates = computed(() => {
  const kw = modelFilter.value.trim().toLowerCase()
  const all = catalog.value?.models ?? []
  if (!kw) return all
  return all.filter((m) =>
    `${m.id} ${m.provider} ${m.model} ${m.name ?? ''}`.toLowerCase().includes(kw),
  )
})
// 本会话顶层角色按档位映射（与档位文案一致）：跟随全局时展示该角色的全局绑定模型。
const TOP_ROLE_BY_GEAR: Record<SessionGear, string> = { fast: 'doc_assistant', daily: 'domain', cluster: 'meta' }
const sessionTopRole = computed(
  () => catalog.value?.roles.find((r) => r.role_id === TOP_ROLE_BY_GEAR[gear.value]) ?? null,
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
  const files = readyFiles.value.map((f) => f.file)
  // 会话指令（goal/loop）随创建/发送请求携带；normal 传 null（objective 由后端回落为消息内容）。
  const directive = runMode.value === 'normal' ? null : buildDirectiveSpec()
  emit('submit', content.value.trim(), pendingImages.value.slice(), files, gear.value, thinking.value, pinnedPlugins.value.slice(), directive, sessionModel.value)
  content.value = ''
  pendingImages.value = []
  pendingFiles.value = []
}

// ── /提炼 快捷指令面板：已绑定会话时输入 "/" 开头弹出。后端拦截 /提炼 做记忆提炼，
// 前端只负责补全与发送（无需新 API）。 ──
const SLASH_COMMANDS = [
  { cmd: '/提炼', aliases: ['/distill'], desc: '把本会话上文提炼为经验技能与块记忆' },
] as const

const slashHighlight = ref(0)
// Esc 关闭标记：可见性由内容推导，Esc 只是"本次输入别再弹"，内容一变即重置。
const slashDismissed = ref(false)

const slashMatches = computed(() => {
  if (!props.sessionBound) return []
  const text = content.value
  if (!text.startsWith('/') || text.includes('\n')) return []
  return SLASH_COMMANDS.filter(
    (c) => c.cmd.startsWith(text) || c.aliases.some((a) => a.startsWith(text)),
  )
})

const slashPanelVisible = computed(() => {
  if (slashDismissed.value || !slashMatches.value.length) return false
  const trimmed = content.value.trim()
  // 已是某命令完整形态（含别名）：隐藏面板，让 Enter 直接发送给后端拦截处理。
  return !SLASH_COMMANDS.some((c) => trimmed === c.cmd || c.aliases.some((a) => trimmed === a))
})

watch(content, () => {
  slashHighlight.value = 0
  slashDismissed.value = false
})

// 补全：写入命令完整形态、关面板、光标到末尾；用户再按 Enter 走原 handleSubmit 发送。
async function applySlashCommand(cmd: string) {
  content.value = cmd
  slashDismissed.value = true
  await nextTick()
  const el = textareaRef.value?.textarea as HTMLTextAreaElement | undefined
  if (el) {
    el.focus()
    el.selectionStart = el.selectionEnd = el.value.length
  }
}

function onKeydown(e: KeyboardEvent) {
  // 中文输入法组词回车不发送（isComposing）；Shift+Enter 换行走默认行为，Enter 直接发送
  if (e.isComposing || e.keyCode === 229) return
  // 快捷指令面板可见时优先接管按键（Enter 分支须在现有发送逻辑之前判断）
  if (slashPanelVisible.value) {
    if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
      e.preventDefault()
      const n = slashMatches.value.length
      slashHighlight.value = (slashHighlight.value + (e.key === 'ArrowDown' ? 1 : n - 1)) % n
      return
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      e.preventDefault()
      const item = slashMatches.value[slashHighlight.value]
      if (item) applySlashCommand(item.cmd)
      return
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      slashDismissed.value = true
      return
    }
  }
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault()
    handleSubmit()
  }
}
</script>

<template>
  <div class="border-t border-line bg-card px-6 py-3">
    <!-- Kimi 风格 composer：一张 bg-card + border-line + rounded-card 统一卡片，
         内部分三层（目录条 / 输入框 / 工具条），层间统一间距；
         focus-within 时边框转主色；待澄清时琥珀高亮（此刻发送=回答上方问题） -->
    <div class="composer flex flex-col gap-2 p-3 rounded-card border bg-page transition-colors"
         :class="awaitingClarify ? 'border-amber-400 dark:border-yellow-600' : 'border-line focus-within:border-primary'"
         @dragover="onDragOver"
         @drop="onDrop">
      <!-- 目录条：单行等高对齐——文件夹图标 + 路径 + 选择目录 + 状态提示 + 新建会话 -->
      <div class="flex items-center gap-2 min-w-0">
        <div class="flex items-center gap-2 min-w-0 flex-1">
          <el-icon class="text-sm text-ink-3 shrink-0"><FolderOpened /></el-icon>
          <span class="text-xs text-ink-2 whitespace-nowrap shrink-0">{{ workDirLabel }}</span>
          <WorkDirPicker class="min-w-0 flex-1" :model-value="workDir || ''" :existing="knownDirs"
                         :placeholder="defaultDir || '默认目录'"
                         @update:model-value="(d: string) => emit('update-workdir', d)" />
          <el-icon v-if="workDirSaving" class="animate-spin text-ink-3 shrink-0"><Loading /></el-icon>
          <span v-if="workDirHint" class="text-[10px] text-ink-3 whitespace-nowrap shrink-0"
                title="目录在每回合开始时读取，正在执行的工具调用仍按旧目录">下回合生效</span>
        </div>
        <div class="flex items-center gap-3 text-xs shrink-0 ml-2">
          <span v-if="sessionActive" class="text-green-400 flex items-center gap-1 whitespace-nowrap">
            <span class="w-1.5 h-1.5 rounded-full bg-green-500 animate-pulse"></span>
            会话已连接
          </span>
          <!-- 运行中的输入语义：不再是"排到队尾"，而是即时注入当前执行（仅真 running；
               待澄清是答复、挂起等子是唤醒新消息，都不是注入，故用 injectHint 而非 sessionActive） -->
          <span v-if="injectHint" class="text-[10px] text-ink-3 whitespace-nowrap"
                title="运行中发送的指令会投进当前 Agent 的邮箱：等待子 Agent 时立即读到并重新规划，其他阶段在当前步骤结束后生效">
            运行中发送＝即时注入当前执行
          </span>
          <span v-else class="text-ink-3 whitespace-nowrap">将创建新会话</span>
          <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-1.5 shrink-0"
                     @click="emit('new-session')">
            <el-icon class="mr-1"><Plus /></el-icon> 新建会话
          </el-button>
        </div>
      </div>

      <!-- 待发送图片预览（可移除） -->
      <div v-if="pendingImages.length" class="flex gap-2 flex-wrap">
        <div v-for="(img, i) in pendingImages" :key="i"
             class="relative w-14 h-14 rounded border border-line overflow-hidden group">
          <img :src="previewUrl(img)" class="w-full h-full object-cover" alt="待发送图片" />
          <button @click="removeImage(i)"
                  class="absolute inset-0 hidden group-hover:flex items-center justify-center bg-black/60 text-red-400 text-xs">
            移除
          </button>
        </div>
      </div>

      <!-- 待发送文件附件（粘贴/选择/拖入；Excel 转换中带转圈，转写 CSV 带「转换」徽标） -->
      <div v-if="pendingFiles.length" class="flex gap-2 flex-wrap">
        <div v-for="(f, i) in pendingFiles" :key="f.id"
             class="flex items-center gap-1.5 px-2 py-1 rounded border border-line bg-card text-xs max-w-64">
          <el-icon v-if="f.converting" class="is-loading text-ink-3 shrink-0"><Loading /></el-icon>
          <el-icon v-else class="text-ink-3 shrink-0"><Document /></el-icon>
          <span class="truncate" :title="f.name">{{ f.name }}</span>
          <span v-if="f.derived" class="text-[10px] px-1 rounded bg-page border border-line shrink-0"
                title="由 Excel 自动转写（后端无 Excel 解析能力，Agent 读取此 CSV），随原文件一并上传">转换</span>
          <span class="text-ink-3 shrink-0">{{ fmtBytes(f.size) }}</span>
          <button class="text-ink-3 hover:text-red-400 shrink-0" title="移除" @click="removeFile(i)">✕</button>
        </div>
      </div>
      <div class="relative">
        <!-- /提炼 快捷指令面板：已绑定会话且输入 "/" 开头时弹出，Enter/Tab/点击补全 -->
        <div v-if="slashPanelVisible"
             class="absolute bottom-full left-0 mb-2 w-full bg-card border border-line rounded shadow-lg z-10 overflow-hidden">
          <div v-for="(c, i) in slashMatches" :key="c.cmd"
               class="flex items-baseline gap-2 px-3 py-2 cursor-pointer"
               :class="i === slashHighlight ? 'bg-page' : ''"
               @mouseenter="slashHighlight = i"
               @click="applySlashCommand(c.cmd)">
            <span class="font-mono font-bold text-sm text-ink">{{ c.cmd }}</span>
            <span class="text-xs text-ink-2">{{ c.desc }}</span>
          </div>
        </div>
        <el-input ref="textareaRef" v-model="content"
                  type="textarea"
                  :autosize="{ minRows: 2, maxRows: 8 }"
                  :placeholder="placeholderText"
                  resize="none"
                  class="chat-input"
                  @keydown="onKeydown"
                  @paste="onPaste" />
      </div>
      <!-- 工具条：左侧 模型与会话设置/插件/图片（统一 small 高度、gap-1.5；
           信任模式/执行档位/思考强度已收编进模型弹层"本会话设置"分区），
           右侧 token 计数 + 发送按钮；过窄时自动折行 -->
      <div class="flex items-center justify-between gap-2">
        <div class="flex items-center gap-1.5 min-w-0 flex-wrap">
          <!-- 会话指令模式弹层：normal=普通对话 / goal=目标模式（多轮自主推进直到完成）/
               loop=循环模式（按间隔重复执行本条指令）；激活会话改动即时 PUT 后端 -->
          <el-popover v-model:visible="directivePopoverVisible" placement="top-start" :width="280" trigger="click">
            <template #reference>
              <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-2"
                         :class="runMode !== 'normal' ? '!text-primary' : ''"
                         title="会话指令模式：目标 / 循环 / 普通">
                <el-icon><Aim /></el-icon>
                <span class="ml-1 text-xs">指令{{ runMode === 'goal' ? '·目标' : runMode === 'loop' ? '·循环' : '' }}</span>
              </el-button>
            </template>
            <div class="space-y-2 text-xs">
              <div>
                <div class="mb-1 text-ink-2">运行模式</div>
                <el-select v-model="runMode" size="small" class="w-full">
                  <el-option value="normal" label="普通对话" />
                  <el-option value="goal" label="目标模式" />
                  <el-option value="loop" label="循环模式" />
                </el-select>
              </div>
              <template v-if="runMode === 'goal'">
                <div>
                  <div class="mb-1 text-ink-2">完成条件</div>
                  <el-input v-model="completionCriterion" size="small" clearable
                            placeholder="完成条件（可空，按目标语义判定）" />
                </div>
                <div>
                  <div class="mb-1 text-ink-2">最大轮数（0 = 不限）</div>
                  <el-input-number v-model="maxTurns" size="small" :min="0" :max="999" style="width: 120px" />
                </div>
                <div>
                  <div class="mb-1 text-ink-2">截止时间</div>
                  <el-date-picker v-model="deadline" size="small" type="datetime" clearable
                                  placeholder="截止时间（可空）" style="width: 100%" />
                </div>
              </template>
              <template v-else-if="runMode === 'loop'">
                <div>
                  <div class="mb-1 text-ink-2">重复间隔</div>
                  <div class="flex items-center gap-2">
                    <el-input-number v-model="loopIntervalMin" size="small" :min="1" :max="10080" style="width: 140px" />
                    <span class="text-ink-3 shrink-0">分钟</span>
                  </div>
                </div>
                <div>
                  <div class="mb-1 text-ink-2">停止条件</div>
                  <el-input v-model="loopUntil" size="small" clearable placeholder="停止条件（可空 = 手动停）" />
                </div>
              </template>
              <div v-if="runMode === 'goal'" class="text-[10px] text-ink-3 leading-relaxed border-t border-line pt-2">
                目标模式：多轮自主推进直到完成；遇审核等待会自动安排回查
              </div>
              <div v-else-if="runMode === 'loop'" class="text-[10px] text-ink-3 leading-relaxed border-t border-line pt-2">
                循环模式：按间隔重复执行本条指令，切换回普通即停止
              </div>
            </div>
          </el-popover>
          <!-- 模型与会话设置弹层：本会话模型一键点选（各自记住）；本会话设置（档位/思考/信任）；全局角色绑定（收底） -->
          <el-popover v-model:visible="modelPopoverVisible" placement="top-start" :width="380" trigger="click">
          <template #reference>
            <el-button text size="small" class="!text-ink-2 hover:!text-primary max-w-52 !px-2"
                       :class="sessionModel ? '!text-primary' : ''"
                       title="模型与会话设置：本会话模型点选即切（仅本会话，各自记住）；档位/思考/信任；全局角色绑定">
              <el-icon><Coin /></el-icon>
              <span class="ml-1 truncate text-xs">{{ sessionModel || sessionTopRole?.model || currentRoleStatus?.model || '模型' }}</span>
            </el-button>
          </template>
          <div class="space-y-2 text-xs">
            <!-- 本会话模型：点选即切（热生效，下一次 LLM 调用）；每个会话各自记住选择，互不串联 -->
            <div>
              <div class="mb-1 flex items-center justify-between text-ink-2">
                <span title="只影响本会话顶层 Agent；每个会话各自记住自己的选择，切换会话不互相影响">本会话模型（点选即切，仅本会话）</span>
                <span class="text-[10px] text-ink-3 shrink-0">{{ sessionBound ? '已记住到本会话' : '作为新会话默认' }}</span>
              </div>
              <el-input v-model="modelFilter" size="small" clearable placeholder="搜索模型 id / provider / 名称" />
              <div class="mt-1.5 max-h-56 overflow-y-auto">
                <div class="model-row" :class="{ active: !sessionModel }" @click="onSessionModelChange('')">
                  <div class="min-w-0">
                    <div class="text-ink truncate">跟随全局配置</div>
                    <div class="text-[10px] text-ink-3 truncate">
                      {{ sessionTopRole ? `${sessionTopRole.role_id} → ${sessionTopRole.provider}/${sessionTopRole.model}` : '按角色绑定（models.json）' }}
                    </div>
                  </div>
                  <el-icon v-if="!sessionModel" class="shrink-0 text-primary"><Check /></el-icon>
                </div>
                <div v-for="m in sessionModelCandidates" :key="m.id"
                     class="model-row" :class="{ active: sessionModel === m.id }"
                     @click="onSessionModelChange(m.id)">
                  <div class="min-w-0">
                    <div class="text-ink truncate">{{ m.id }}<span v-if="m.name" class="text-ink-3"> · {{ m.name }}</span></div>
                    <div class="text-[10px] text-ink-3 truncate">{{ m.provider }}/{{ m.model }}</div>
                  </div>
                  <el-icon v-if="sessionModel === m.id" class="shrink-0 text-primary"><Check /></el-icon>
                </div>
                <div v-if="!sessionModelCandidates.length" class="py-3 text-center text-ink-3">无匹配模型</div>
              </div>
            </div>
            <!-- 本会话设置（自工具栏收编，输入栏精简）：热生效，切换即 POST 后端 -->
            <div class="border-t border-line pt-2 space-y-2">
              <div>
                <div class="mb-1 text-ink-2" title="快速档=文档助手直达，日常档=DomainAgent 直接执行，集群档=Meta 全装编排">执行档位（本会话）</div>
                <el-select v-model="gear" size="small" class="w-full"
                           @change="onGearChange($event as SessionGear)">
                  <el-option value="fast" label="快速档（文档助手直达）" />
                  <el-option value="daily" label="日常档（DomainAgent 直接执行）" />
                  <el-option value="cluster" label="集群档（Meta 全装编排）" />
                </el-select>
              </div>
              <div>
                <div class="mb-1 text-ink-2" title="只影响本会话顶层 Agent；运行中可改，下一次 LLM 调用生效">思考强度（本会话）</div>
                <el-select v-model="thinking" size="small" class="w-full"
                           @change="onThinkingChange($event as SessionThinking)">
                  <el-option value="" label="跟随角色默认" />
                  <el-option value="off" label="off" />
                  <el-option value="low" label="low" />
                  <el-option value="medium" label="medium" />
                  <el-option value="high" label="high" />
                </el-select>
              </div>
              <div v-if="sessionBound && session">
                <div class="mb-1 text-ink-2" title="变更类操作的审批档位，切换下一工具调用生效">信任模式（本会话）</div>
                <el-select :model-value="trustMode" size="small" class="w-full"
                           @update:model-value="onTrustModeChange($event as TrustMode)">
                  <el-option value="suggest" label="suggest 逐条审批" />
                  <el-option value="auto-edit" label="auto-edit 审命令" />
                  <el-option value="full-auto" label="full-auto 全自主" />
                </el-select>
              </div>
            </div>
            <!-- 全局角色绑定：写入 models.json role_bindings，改变所有「跟随全局配置」会话的默认模型 -->
            <div class="border-t border-line pt-2">
              <div class="mb-1 text-[10px] text-ink-3" title="写入 config/models.json role_bindings；只影响选择「跟随全局配置」的会话">全局角色绑定（影响所有会话的默认）</div>
              <div class="space-y-2">
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
                  <div class="mb-1 text-ink-2">思考强度（角色绑定）</div>
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
            </div>
          </div>
        </el-popover>
          <!-- 插件选择弹层（KIMI 式）：勾选 = 本会话优先/强制使用该插件；不勾选 = 模型自行判断 -->
          <el-popover v-model:visible="pluginPopoverVisible" placement="top-start" :width="340" trigger="click">
            <template #reference>
              <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-2"
                         title="指定插件：勾选后本会话优先使用其工具；不勾选由模型自行判断">
                <el-icon><MagicStick /></el-icon>
                <span class="ml-1 text-xs">插件{{ pinnedPlugins.length ? `·${pinnedPlugins.length}` : '' }}</span>
              </el-button>
            </template>
            <div class="text-xs" v-loading="pluginsLoading">
              <div class="mb-1 text-ink-2">指定插件（勾选后本会话优先使用其工具）</div>
              <div v-if="selectablePlugins.length" class="max-h-64 overflow-y-auto">
                <div v-for="p in selectablePlugins" :key="p.id"
                     class="flex items-start gap-2 py-1.5 border-b border-line last:border-b-0">
                  <el-checkbox :model-value="pinnedPlugins.includes(p.id)" :disabled="pluginSaving"
                               class="!mr-0 shrink-0"
                               @change="onTogglePlugin(p.id, $event as boolean)" />
                  <div class="min-w-0 flex-1">
                    <div class="text-ink truncate">{{ p.name || p.id }}</div>
                    <div class="text-ink-3 text-[11px] truncate" :title="p.description">{{ p.description || p.id }}</div>
                  </div>
                  <span class="text-[10px] text-ink-3 shrink-0">{{ p.tools?.length ?? 0 }} 工具</span>
                </div>
              </div>
              <div v-else class="py-3 text-center text-ink-3">
                暂无可用插件（已启用且运行中的插件才会出现在这里）
              </div>
              <div class="flex items-center justify-between pt-2">
                <el-button link type="primary" size="small" @click="goPluginsPage">管理插件</el-button>
                <el-button link size="small" :disabled="!pinnedPlugins.length || pluginSaving" @click="clearPlugins">清空</el-button>
              </div>
            </div>
          </el-popover>
          <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-2"
                     :disabled="pendingImages.length >= 4"
                     title="添加图片（或直接粘贴截图）"
                     @click="pickFiles">
            <el-icon><Picture /></el-icon>
          </el-button>
          <el-button text size="small" class="!text-ink-2 hover:!text-primary !px-2"
                     :disabled="pendingFiles.length >= MAX_FILES"
                     title="添加文件（SQL/Excel/PDF 等，也可直接粘贴或拖入）"
                     @click="pickAnyFiles">
            <el-icon><UploadFilled /></el-icon>
          </el-button>
        </div>
        <div class="flex items-center gap-2 shrink-0">
          <span class="text-[11px] text-ink-3 font-mono hidden sm:inline"
                :title="`输入 ${formatTokens(props.inputTokens || 0)} / 输出 ${formatTokens(props.outputTokens || 0)} tokens`">
            ↑{{ formatTokens(props.inputTokens || 0) }} ↓{{ formatTokens(props.outputTokens || 0) }}
          </span>
          <!-- 运行中 → 红色「停止」（对标 ChatGPT，唯一停止入口：弹窗可选是否同时停子 Agent）；
               其余状态一律「发送」。待澄清时额外挂一个静态「停止」逃生口（放弃当前提问），
               主按钮保持"发送"用于答复——旧"终止"按钮是该态唯一出口，不能没有。 -->
          <el-button v-if="running" type="danger" size="small" round
                     title="停止运行（可选择是否同时停止运行中的子 Agent）；Enter 仍可发送指令"
                     class="!bg-red-500 !border-red-500 hover:!bg-red-600 hover:!border-red-600"
                     @click="emit('stop')">
            <el-icon class="mr-1"><VideoPause /></el-icon> 停止
          </el-button>
          <template v-else>
            <el-button v-if="awaitingClarify" text size="small"
                       class="!text-ink-3 hover:!text-red-500 !px-2 shrink-0"
                       title="放弃当前提问并停止会话"
                       @click="emit('stop')">
              停止
            </el-button>
            <el-button type="primary" size="small" round
                       :loading="loading"
                       :disabled="!canSend"
                       title="Enter 发送，Shift+Enter 换行"
                       class="!bg-primary !border-primary hover:!bg-[var(--bma-primary-hover)] hover:!border-[var(--bma-primary-hover)] disabled:opacity-50"
                       @click="handleSubmit">
              <el-icon class="mr-1"><Promotion /></el-icon> {{ awaitingClarify ? '答复' : '发送' }}
            </el-button>
          </template>
        </div>
      </div>
    </div>
    <input ref="fileRef" type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple class="hidden" @change="onFileChange" />
    <input ref="fileInputRef" type="file" multiple class="hidden" @change="onAnyFileChange" />

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
/* 容器（.composer）负责边框/聚焦态，文本域自身去边框、透明背景；
   min-height 固定 72px，placeholder 用 ink-3 弱色 */
:deep(.composer .el-textarea__inner) {
  background-color: transparent !important;
  border: none;
  color: var(--bma-text);
  box-shadow: none !important;
  font-size: 13px;
  min-height: 72px !important;
  padding: 8px 8px 4px;
}
:deep(.composer .el-textarea__inner)::placeholder {
  color: var(--bma-text-3);
}
/* 本会话模型一键列表行：整行可点，选中项主色描边高亮 */
.model-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 6px 8px;
  border-radius: 6px;
  border: 1px solid transparent;
  cursor: pointer;
}
.model-row:hover {
  background: var(--bma-primary-soft);
}
.model-row.active {
  border-color: var(--bma-primary);
  background: var(--bma-primary-soft);
}
.hidden {
  display: none;
}
</style>
