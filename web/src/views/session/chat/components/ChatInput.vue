<script setup lang="ts">
import { ref, computed, reactive, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { WireImage } from '@/types'
import { useWorkDir } from '@/composables/useWorkDir'
import { useModelSelection } from '@/composables/useModelSelection'
import WorkDirPicker from '@/components/WorkDirPicker.vue'

const props = defineProps<{
  loading?: boolean
  sessionActive?: boolean
  inputTokens?: number
  outputTokens?: number
}>()

const emit = defineEmits<{
  (e: 'submit', content: string, images: WireImage[]): void
  (e: 'new-session'): void
}>()

const content = ref('')

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

const tokenLabel = computed(() => {
  const input = props.inputTokens || 0
  const output = props.outputTokens || 0
  return `↑ ${fmtTokens(input)}  ↓ ${fmtTokens(output)}`
})

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

// 工作目录：模块级共享 ref（useWorkDir），新建会话时随 createSession 提交；
// 与首页 WorkDirPicker 同源，改动实时同步。
const { workDir } = useWorkDir()

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
  emit('submit', content.value.trim(), pendingImages.value.slice())
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
        <div class="flex items-center gap-2 w-80 max-w-full shrink-0 workdir-cell">
          <span class="text-ink-2 whitespace-nowrap">工作目录</span>
          <WorkDirPicker v-model="workDir" />
        </div>
        <span v-if="sessionActive" class="text-green-400 flex items-center gap-1">
          <span class="w-1.5 h-1.5 rounded-full bg-green-500 animate-pulse"></span>
          会话已连接
        </span>
        <span v-else class="text-ink-2">将创建新会话</span>
      </div>
      <div class="flex items-center gap-3 text-xs">
        <span class="text-ink-2 font-mono" :title="`输入 ${props.inputTokens || 0} / 输出 ${props.outputTokens || 0} tokens`">
          <span class="text-ink-3">Token</span>
          <span class="ml-2 text-blue-400">↑{{ fmtTokens(props.inputTokens || 0) }}</span>
          <span class="ml-1 text-green-400">↓{{ fmtTokens(props.outputTokens || 0) }}</span>
        </span>
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

    <!-- 输入区 -->
    <div class="flex gap-2 items-end">
      <div class="flex-1 chat-input">
        <el-input ref="textareaRef" v-model="content"
                  type="textarea"
                  :autosize="{ minRows: 2, maxRows: 6 }"
                  placeholder="向 AI 下达命令…（Enter 发送，Shift+Enter 换行，可直接粘贴图片）"
                  resize="none"
                  @keydown="onKeydown"
                  @paste="onPaste" />
      </div>
      <div class="flex flex-col gap-1">
        <!-- 模型选择弹层（对话栏右下）：角色 → 模型 → 思考强度 -->
        <el-popover v-model:visible="modelPopoverVisible" placement="top-end" :width="380" trigger="click">
          <template #reference>
            <el-button plain size="small"
                       class="!bg-transparent !border-line !text-ink-2 hover:!text-white max-w-44"
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
                <el-option v-for="m in catalog?.models || []" :key="m.id" :value="m.id"
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
        <el-button :disabled="pendingImages.length >= 4" plain size="small"
                   class="!bg-transparent !border-line !text-ink-2 hover:!text-white"
                   title="添加图片（或直接粘贴截图）"
                   @click="pickFiles">
          <el-icon><Picture /></el-icon>
        </el-button>
        <el-button type="primary"
                   :loading="loading"
                   :disabled="!canSend"
                   class="!bg-primary !border-primary hover:!bg-[var(--bma-primary-hover)] hover:!border-[var(--bma-primary-hover)]"
                   @click="handleSubmit">
          <el-icon class="mr-1"><Promotion /></el-icon> 发送
          <span class="sr-only">{{ tokenLabel }}</span>
        </el-button>
      </div>
      <input ref="fileRef" type="file" accept="image/png,image/jpeg,image/gif,image/webp" multiple class="hidden" @change="onFileChange" />
    </div>

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
:deep(.chat-input .el-textarea__inner) {
  background-color: var(--bma-page) !important;
  border: 1px solid var(--bma-border);
  color: var(--bma-text);
  box-shadow: none !important;
  font-size: 13px;
  padding: 10px 12px;
}
:deep(.chat-input .el-textarea__inner:focus) {
  border-color: var(--bma-primary);
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
