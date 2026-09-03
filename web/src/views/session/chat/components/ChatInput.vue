<script setup lang="ts">
import { ref, computed } from 'vue'
import { ElMessage } from 'element-plus'
import type { WireImage } from '@/types'
import { useWorkDir } from '@/composables/useWorkDir'
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

const quickTags = [
  '展开方案细节',
  '给出可运行代码',
  '解释一下原理',
  '列出关键步骤',
  '改成 Go 实现',
  '总结要点',
]

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

function handleSubmit() {
  if (!canSend.value) return
  emit('submit', content.value.trim(), pendingImages.value.slice())
  content.value = ''
  pendingImages.value = []
}

function onKeydown(e: KeyboardEvent) {
  // Ctrl/Cmd + Enter 提交
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault()
    handleSubmit()
  }
}

function applyQuickTag(tag: string) {
  if (content.value.trim()) {
    content.value = content.value.trim() + '；' + tag
  } else {
    content.value = tag
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

    <div class="flex gap-2 mb-2 flex-wrap">
      <el-tag v-for="t in quickTags" :key="t"
              effect="plain"
              size="small"
              class="!bg-page !border-line !text-ink-2 hover:!text-white cursor-pointer transition-colors"
              @click="applyQuickTag(t)">
        {{ t }}
      </el-tag>
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
                  placeholder="向 AI 下达命令…（Ctrl/Cmd + Enter 发送，可直接粘贴图片）"
                  resize="none"
                  @keydown="onKeydown"
                  @paste="onPaste" />
      </div>
      <div class="flex flex-col gap-1">
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
