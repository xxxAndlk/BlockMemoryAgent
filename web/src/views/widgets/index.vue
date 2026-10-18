<script setup lang="ts">
// 界面部件管理页（2026-10-13 Widget 机制）：widgets/ 目录下部件的启停与手动重扫。
// 部件本体由 Agent 经 widget_save 工具创建/更新，本页只做状态管理，不提供编辑。
// 连接器（2026-10-17）：头部「新建接件」按四种类型（url/api/mcp/webhook）创建外部接件；
// 已有连接器的部件提供「设置」按钮复用同一弹窗走 updateWidgetConnector。
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import {
  listWidgets,
  getWidget,
  createWidget,
  updateWidgetConnector,
  enableWidget,
  disableWidget,
  reloadWidgets,
  deleteWidget,
  listTrash,
  restoreWidget,
} from '@/api/widgets'
import type { WidgetConnector, WidgetInfo, WidgetTrashEntry } from '@/types'

const widgets = ref<WidgetInfo[]>([])
const loading = ref(false)
const mutating = ref<string | null>(null)
const trashOpen = ref(false)
const trash = ref<WidgetTrashEntry[]>([])
const trashLoading = ref(false)

// ---- 连接器弹窗（新建接件 / 接件设置复用）----

const dlgOpen = ref(false)
const dlgMode = ref<'create' | 'edit'>('create')
/** 设置模式的目标部件 */
const editWidget = ref<WidgetInfo | null>(null)
const saving = ref(false)
/** 创建/再生成功后的回调地址展示（webhook 先展示地址，点「完成」再关弹窗） */
const hookResult = ref('')
/** 设置模式 webhook 的当前回调地址（列表不带 hook_url，打开弹窗时走详情接口回显） */
const editHookUrl = ref('')

interface HeaderRow {
  key: string
  value: string
}

interface ConnectorForm {
  name: string
  id: string
  type: WidgetConnector['type']
  /** url=页面地址 / api=接口 URL / mcp=工具名 */
  target: string
  method: 'GET' | 'POST'
  headers: HeaderRow[]
  body: string
  /** mcp 调用参数（JSON 文本，提交时校验） */
  argsText: string
  intervalSec: number
  /** webhook 设置模式：勾选后提交即重新生成 token/地址 */
  regenToken: boolean
}

function emptyForm(): ConnectorForm {
  return {
    name: '', id: '', type: 'url', target: '', method: 'GET',
    headers: [], body: '', argsText: '', intervalSec: 30, regenToken: false,
  }
}

const form = ref<ConnectorForm>(emptyForm())

/** hook_url 为相对路径（/api/widgets/<id>/hook/<token>），展示/复制时拼 origin 成完整地址。 */
function absUrl(h: string): string {
  return h.startsWith('http') ? h : `${location.origin}${h}`
}

const fullHookUrl = computed(() => (hookResult.value ? absUrl(hookResult.value) : ''))

/** 连接器类型徽标配色（url/api/mcp/webhook 四色小 tag）。 */
function tagTypeOf(t: string): 'primary' | 'success' | 'warning' | 'danger' | 'info' {
  const map: Record<string, 'primary' | 'success' | 'warning' | 'danger'> = {
    url: 'primary', api: 'success', mcp: 'warning', webhook: 'danger',
  }
  return map[t] ?? 'info'
}

function openCreate() {
  dlgMode.value = 'create'
  editWidget.value = null
  hookResult.value = ''
  editHookUrl.value = ''
  form.value = emptyForm()
  dlgOpen.value = true
}

/** 设置模式回显 connector 字段；webhook 另拉详情取 hook_url 展示当前回调地址。 */
function openEdit(w: WidgetInfo) {
  dlgMode.value = 'edit'
  editWidget.value = w
  hookResult.value = ''
  editHookUrl.value = ''
  const c = w.connector
  form.value = {
    name: w.name || '',
    id: w.id,
    type: c?.type ?? 'url',
    target: c?.target ?? '',
    method: c?.method === 'POST' ? 'POST' : 'GET',
    headers: Object.entries(c?.headers ?? {}).map(([key, value]) => ({ key, value })),
    body: c?.body ?? '',
    argsText: c?.args ? JSON.stringify(c.args, null, 2) : '',
    intervalSec: c?.interval_sec ?? 30,
    regenToken: false,
  }
  if (c?.type === 'webhook') {
    getWidget(w.id)
      .then((d) => { if (d?.hook_url) editHookUrl.value = d.hook_url })
      .catch(() => {}) // 详情拉不到不阻塞设置表单
  }
  dlgOpen.value = true
}

function closeDlg() {
  dlgOpen.value = false
  hookResult.value = ''
  editHookUrl.value = ''
}

/** 按表单类型组装 connector 对象；校验失败返回 null（已弹提示）。 */
function buildConnector(): WidgetConnector | null {
  const f = form.value
  if (f.type === 'webhook') return { type: 'webhook' }
  if (!f.target.trim()) {
    const label = f.type === 'mcp' ? '工具名' : f.type === 'url' ? '目标页面 URL' : '数据接口 URL'
    ElMessage.warning(`请填写${label}`)
    return null
  }
  const c: WidgetConnector = { type: f.type, target: f.target.trim() }
  if (f.type === 'api') {
    c.method = f.method
    const headers: Record<string, string> = {}
    for (const h of f.headers) {
      const k = h.key.trim()
      if (k) headers[k] = h.value
    }
    if (Object.keys(headers).length) c.headers = headers
    if (f.method === 'POST' && f.body) c.body = f.body
    c.interval_sec = f.intervalSec
  } else if (f.type === 'mcp') {
    const text = f.argsText.trim()
    if (text) {
      let args: unknown
      try {
        args = JSON.parse(text)
      } catch {
        ElMessage.error('调用参数不是合法 JSON')
        return null
      }
      if (typeof args !== 'object' || args === null || Array.isArray(args)) {
        ElMessage.error('调用参数需为 JSON 对象')
        return null
      }
      c.args = args as Record<string, unknown>
    }
    c.interval_sec = f.intervalSec
  }
  return c
}

async function submit() {
  const f = form.value
  if (!f.name.trim()) {
    ElMessage.warning('请填写名称')
    return
  }
  const c = buildConnector()
  if (!c) return
  saving.value = true
  try {
    if (dlgMode.value === 'create') {
      const created = await createWidget({
        name: f.name.trim(),
        id: f.id.trim() || undefined,
        connector: c,
      })
      ElMessage.success(`接件 ${created.name || created.id} 已创建`)
      load()
      if (created.hook_url) {
        // webhook：先展示回调地址（请用户保存），点「完成」再关弹窗
        hookResult.value = created.hook_url
      } else {
        dlgOpen.value = false
      }
    } else if (editWidget.value) {
      const updated = await updateWidgetConnector(editWidget.value.id, {
        ...c,
        ...(f.regenToken ? { regen_token: true } : {}),
      })
      const idx = widgets.value.findIndex((x) => x.id === updated.id)
      if (idx >= 0) widgets.value[idx] = updated
      ElMessage.success('连接器设置已保存')
      if (f.regenToken && updated.hook_url) {
        hookResult.value = updated.hook_url
      } else {
        dlgOpen.value = false
      }
    }
  } catch (e) {
    ElMessage.error((dlgMode.value === 'create' ? '创建失败：' : '保存失败：') +
      (e instanceof Error ? e.message : String(e)))
  } finally {
    saving.value = false
  }
}

function copyHook() {
  if (!fullHookUrl.value) return
  navigator.clipboard?.writeText(fullHookUrl.value).then(
    () => ElMessage.success('回调地址已复制'),
    () => ElMessage.info(fullHookUrl.value),
  )
}

function copyEditHook() {
  const text = editHookUrl.value ? absUrl(editHookUrl.value) : ''
  if (!text) return
  navigator.clipboard?.writeText(text).then(
    () => ElMessage.success('回调地址已复制'),
    () => ElMessage.info(text),
  )
}

async function load() {
  loading.value = true
  try {
    const res = await listWidgets()
    widgets.value = res.widgets || []
  } catch (e) {
    ElMessage.error('部件列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

// 热启用/停用：原子改写 widget.json 的 enabled 字段，已打开的卡片下次加载即生效。
async function toggle(w: WidgetInfo) {
  mutating.value = w.id
  try {
    const updated = w.enabled ? await disableWidget(w.id) : await enableWidget(w.id)
    const idx = widgets.value.findIndex(x => x.id === w.id)
    if (idx >= 0) widgets.value[idx] = updated
    ElMessage.success(`部件 ${w.id} 已${updated.enabled ? '启用' : '停用'}`)
  } catch (e) {
    ElMessage.error('操作失败：' + (e instanceof Error ? e.message : String(e)))
    load()
  } finally {
    mutating.value = null
  }
}

/** 删除部件：软删进后端回收站（widgets/.trash/），可从回收站抽屉恢复。 */
async function handleDelete(w: WidgetInfo) {
  mutating.value = w.id
  try {
    await deleteWidget(w.id)
    widgets.value = widgets.value.filter((x) => x.id !== w.id)
    ElMessage.success(`部件 ${w.id} 已移入回收站`)
  } catch (e) {
    ElMessage.error('删除失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    mutating.value = null
  }
}

async function handleReload() {
  loading.value = true
  try {
    const res = await reloadWidgets()
    widgets.value = res.widgets || []
    ElMessage.success('部件已重新扫描')
  } catch (e) {
    ElMessage.error('重载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

/** 回收站：打开抽屉即拉取列表。 */
async function openTrash() {
  trashOpen.value = true
  trashLoading.value = true
  try {
    trash.value = (await listTrash()).trash || []
  } catch (e) {
    ElMessage.error('回收站加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    trashLoading.value = false
  }
}

/** 恢复后同时刷新部件列表与回收站。 */
async function handleRestore(entry: WidgetTrashEntry) {
  trashLoading.value = true
  try {
    await restoreWidget(entry.name)
    ElMessage.success(`部件 ${entry.id || entry.name} 已恢复`)
    load()
    trash.value = (await listTrash()).trash || []
  } catch (e) {
    ElMessage.error('恢复失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    trashLoading.value = false
  }
}

/** updated_at 为后端 RFC3339；解析失败给原文。 */
function formatTime(t?: string) {
  if (!t) return ''
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? t : d.toLocaleString()
}

onMounted(load)
</script>

<template>
  <div class="p-6 h-full overflow-y-auto text-ink">
    <div class="flex items-center justify-between mb-4">
      <div>
        <h2 class="text-lg font-bold text-ink">界面部件</h2>
        <p class="text-xs text-ink-2 mt-1">Agent 自写的渲染部件（二维码画布、美化展示卡等）与外部接件（页面 / 接口 / MCP 工具 / 回调推送），在对话栏以卡片展示。</p>
      </div>
      <div class="flex items-center gap-2">
        <el-button plain class="!bg-transparent !border-line !text-ink" @click="openCreate">
          <el-icon class="mr-1"><Plus /></el-icon> 新建接件
        </el-button>
        <el-button plain class="!bg-transparent !border-line !text-ink" @click="openTrash">
          <el-icon class="mr-1"><Delete /></el-icon> 回收站
        </el-button>
        <el-button plain class="!bg-transparent !border-line !text-ink" :loading="loading" @click="handleReload">
          <el-icon class="mr-1"><Refresh /></el-icon> 重载部件
        </el-button>
      </div>
    </div>

    <div class="grid gap-4 md:grid-cols-2 items-stretch">
      <el-card v-for="w in widgets" :key="w.id" class="!border-line !bg-card widget-card" shadow="never">
        <!-- 标题行：名称 + 徽标 + 右侧开关 -->
        <div class="flex items-start justify-between gap-3">
          <div class="min-w-0 flex-1 flex items-center gap-2 flex-wrap">
            <span class="font-bold text-ink truncate">{{ w.name || w.id }}</span>
            <el-tag size="small" :type="w.enabled ? 'success' : 'info'" effect="plain">
              {{ w.enabled ? 'enabled' : 'disabled' }}
            </el-tag>
            <el-tag v-if="w.connector" size="small" :type="tagTypeOf(w.connector.type)" effect="plain">
              {{ w.connector.type }}
            </el-tag>
            <span v-if="w.version" class="text-[10px] text-ink-3 font-mono">v{{ w.version }}</span>
          </div>
          <el-button v-if="w.connector" size="small" text class="mr-1" @click="openEdit(w)">设置</el-button>
          <el-popconfirm title="删除该部件？可稍后在回收站恢复" confirm-button-text="删除" cancel-button-text="取消"
                         width="220" @confirm="() => handleDelete(w)">
            <template #reference>
              <el-button size="small" text type="danger" class="mr-1" :loading="mutating === w.id"
                         :disabled="mutating === w.id">删除</el-button>
            </template>
          </el-popconfirm>
          <el-switch :model-value="w.enabled" :loading="mutating === w.id"
                     :disabled="mutating === w.id"
                     @change="() => toggle(w)" />
        </div>

        <!-- 描述 -->
        <p v-if="w.description" class="text-xs text-ink-2 mt-2 line-clamp-2 min-h-[2em]">{{ w.description }}</p>

        <!-- 元信息区 -->
        <div class="mt-3 pt-2 border-t border-line space-y-1 text-[11px]">
          <div class="text-ink-2">
            ID：<span class="font-mono text-ink-2">{{ w.id }}</span>
          </div>
          <div class="text-ink-2">
            入口：<span class="font-mono text-ink-2">{{ w.entry }}</span>
            <span v-if="w.files?.length"> · {{ w.files.length }} 个文件</span>
          </div>
          <div v-if="w.connector?.target" class="text-ink-2">
            目标：<span class="font-mono text-ink-2 break-all">{{ w.connector.target }}</span>
          </div>
          <div class="text-ink-2">
            Owner：<span class="font-mono text-ink-2">{{ w.owner || '-' }}</span>
          </div>
          <div v-if="w.updated_at" class="text-ink-3">更新于：{{ formatTime(w.updated_at) }}</div>
        </div>
      </el-card>
    </div>

    <div v-if="!loading && !widgets.length" class="text-center text-sm text-ink-2 py-16">
      暂无界面部件。部件由 Agent 经 widget_save 创建，目录为 config.yaml 同级的 widgets/。
    </div>

    <!-- 回收站抽屉：软删部件列表与恢复 -->
    <el-drawer v-model="trashOpen" title="部件回收站" size="360px">
      <div v-loading="trashLoading" class="space-y-2">
        <div v-for="t in trash" :key="t.name"
             class="flex items-center justify-between gap-2 border border-line rounded px-3 py-2">
          <div class="min-w-0">
            <div class="text-xs text-ink font-mono truncate">{{ t.id || t.name }}</div>
            <div v-if="t.deleted_at" class="text-[10px] text-ink-3">删除于 {{ formatTime(t.deleted_at) }}</div>
          </div>
          <el-button size="small" plain type="primary" :disabled="trashLoading" @click="handleRestore(t)">
            恢复
          </el-button>
        </div>
        <div v-if="!trashLoading && !trash.length" class="text-center text-xs text-ink-3 py-8">回收站为空</div>
      </div>
    </el-drawer>

    <!-- 新建接件 / 接件设置（复用同一弹窗；webhook 创建/再生成功先展示回调地址，点「完成」关闭） -->
    <el-dialog v-model="dlgOpen" :title="dlgMode === 'create' ? '新建接件' : `接件设置：${editWidget?.name || editWidget?.id || ''}`"
               width="560px" top="6vh">
      <div v-if="hookResult">
        <div class="text-xs text-ink-2 mb-1">回调地址（外部系统向此地址 POST 数据）</div>
        <div class="flex items-center gap-2">
          <el-input :model-value="fullHookUrl" readonly spellcheck="false" />
          <el-button plain class="!bg-transparent !border-line !text-ink" @click="copyHook">复制</el-button>
        </div>
        <p class="text-[11px] text-ink-3 mt-2">请保存此地址，页面关闭后可在详情（设置）中再次查看。</p>
      </div>

      <div v-else class="space-y-3">
        <!-- 通用：名称 / ID / 类型 -->
        <div>
          <div class="text-xs text-ink-2 mb-1">名称 <span class="text-red-400">*</span></div>
          <el-input v-model="form.name" :disabled="dlgMode === 'edit'" placeholder="如：Grafana 监控 / 汇率行情" spellcheck="false" />
        </div>
        <div v-if="dlgMode === 'create'">
          <div class="text-xs text-ink-2 mb-1">ID（可选，缺省由后端生成）</div>
          <el-input v-model="form.id" placeholder="留空自动生成" spellcheck="false" />
        </div>
        <div>
          <div class="text-xs text-ink-2 mb-1">类型</div>
          <el-select v-model="form.type" :disabled="dlgMode === 'edit'" class="w-full">
            <el-option label="外部页面（url 直连 iframe）" value="url" />
            <el-option label="数据接口（后端定时拉 HTTP）" value="api" />
            <el-option label="插件工具（后端定时调 MCP）" value="mcp" />
            <el-option label="回调推送（webhook 反向接入）" value="webhook" />
          </el-select>
        </div>

        <!-- url：目标页面 URL -->
        <div v-if="form.type === 'url'">
          <div class="text-xs text-ink-2 mb-1">目标页面 URL <span class="text-red-400">*</span></div>
          <el-input v-model="form.target" placeholder="https://…（如 Grafana 面板页）" spellcheck="false" />
        </div>

        <!-- api：数据接口 URL / 方法 / 请求头 / 请求体 / 刷新间隔 -->
        <template v-if="form.type === 'api'">
          <div>
            <div class="text-xs text-ink-2 mb-1">数据接口 URL <span class="text-red-400">*</span></div>
            <el-input v-model="form.target" placeholder="https://api.example.com/data" spellcheck="false" />
          </div>
          <div class="flex gap-3">
            <div class="w-32">
              <div class="text-xs text-ink-2 mb-1">方法</div>
              <el-select v-model="form.method" class="w-full">
                <el-option label="GET" value="GET" />
                <el-option label="POST" value="POST" />
              </el-select>
            </div>
            <div class="flex-1">
              <div class="text-xs text-ink-2 mb-1">刷新间隔（秒）</div>
              <el-input-number v-model="form.intervalSec" :min="1" class="!w-full" />
            </div>
          </div>
          <div>
            <div class="text-xs text-ink-2 mb-1">请求头（key = value，值支持 ${ENV_VAR} 由后端展开）</div>
            <div class="space-y-1">
              <div v-for="(h, i) in form.headers" :key="i" class="flex items-center gap-1">
                <el-input v-model="h.key" placeholder="Header 名" class="!w-40" spellcheck="false" />
                <el-input v-model="h.value" placeholder="值" spellcheck="false" />
                <el-button text type="danger" class="!px-1 shrink-0" @click="form.headers.splice(i, 1)">✕</el-button>
              </div>
              <el-button text type="primary" class="!px-1" @click="form.headers.push({ key: '', value: '' })">
                + 添加请求头
              </el-button>
            </div>
          </div>
          <div v-if="form.method === 'POST'">
            <div class="text-xs text-ink-2 mb-1">请求体（可选，原文透传）</div>
            <el-input v-model="form.body" type="textarea" :rows="3" spellcheck="false" />
          </div>
        </template>

        <!-- mcp：工具名 / 调用参数 / 刷新间隔 -->
        <template v-if="form.type === 'mcp'">
          <div>
            <div class="text-xs text-ink-2 mb-1">工具名 <span class="text-red-400">*</span></div>
            <el-input v-model="form.target" placeholder="如 web_search__firecrawl_search" spellcheck="false" />
          </div>
          <div>
            <div class="text-xs text-ink-2 mb-1">调用参数（JSON 对象，可选）</div>
            <el-input v-model="form.argsText" type="textarea" :rows="4" placeholder='{"query": "…"}' spellcheck="false" />
          </div>
          <div>
            <div class="text-xs text-ink-2 mb-1">刷新间隔（秒）</div>
            <el-input-number v-model="form.intervalSec" :min="1" class="!w-full" />
          </div>
        </template>

        <!-- webhook：创建后由后端生成回调地址 -->
        <div v-if="form.type === 'webhook'">
          <p class="text-[11px] text-ink-3 leading-relaxed">
            创建后后端生成回调地址与 token；外部系统向该地址 POST 数据，即推送给部件展示。
          </p>
          <template v-if="dlgMode === 'edit'">
            <div v-if="editHookUrl" class="mt-2">
              <div class="text-xs text-ink-2 mb-1">当前回调地址</div>
              <div class="flex items-center gap-2">
                <el-input :model-value="absUrl(editHookUrl)" readonly spellcheck="false" />
                <el-button plain class="!bg-transparent !border-line !text-ink" @click="copyEditHook">复制</el-button>
              </div>
            </div>
            <el-checkbox v-model="form.regenToken" class="!text-ink-2 mt-2">重新生成地址（旧地址立即失效）</el-checkbox>
          </template>
        </div>
      </div>

      <template #footer>
        <template v-if="hookResult">
          <el-button type="primary" @click="closeDlg">完成</el-button>
        </template>
        <template v-else>
          <el-button @click="dlgOpen = false">取消</el-button>
          <el-button type="primary" :loading="saving" @click="submit">
            {{ dlgMode === 'create' ? '创建' : '保存' }}
          </el-button>
        </template>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

/* 卡片 hover：轻微阴影 + 边框高亮（与插件管理页一致） */
.widget-card {
  transition: box-shadow 0.15s ease, border-color 0.15s ease;
}
.widget-card:hover {
  box-shadow: 0 2px 12px rgba(0, 0, 0, 0.08);
  border-color: var(--bma-primary, #409eff) !important;
}
</style>
