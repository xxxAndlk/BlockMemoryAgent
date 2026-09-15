<script setup lang="ts">
// 工作目录设置抽屉：侧栏目录树的「项目偏好与测试助手」入口。
// 内容与 /projects 页右栏同源（同一组 API），放进抽屉是为了不打断侧栏里的浏览动线。
import { ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { getProjectPreferences, saveProjectPreferences } from '@/api/preferences'
import { getTesterConfig, saveTesterConfig } from '@/api/tester'

const props = defineProps<{ dir: string | null }>()
// 关闭时把 dir 清回 null：否则同一目录再点一次「项目偏好」时 prop 没变化，
// 监听不触发，抽屉看着"打不开"。
const emit = defineEmits<{ 'update:dir': [string | null] }>()
const visible = ref(false)
const dir = ref<string | null>(null)

watch(visible, (v) => {
  if (!v) emit('update:dir', null)
})

const prefContent = ref('')
const prefPath = ref('')
const prefLoading = ref(false)
const prefSaving = ref(false)
const prefDirty = ref(false)

const testerMode = ref<'off' | 'auto' | 'on'>('off')
const testerPrompt = ref('')
const testerRounds = ref(2)
const testerLoading = ref(false)
const testerSaving = ref(false)

const dirLabel = () => dir.value || '默认目录'

// 打开即加载当前目录的配置；关闭时复位脏标记，避免下次打开误显示"未保存"。
watch(
  () => props.dir,
  async (d) => {
    dir.value = d
    visible.value = d !== null
    if (d === null) return
    prefDirty.value = false
    await Promise.all([loadPrefs(), loadTester()])
  },
)

async function loadPrefs() {
  prefLoading.value = true
  try {
    const res = await getProjectPreferences(dir.value || undefined)
    prefContent.value = res.content || ''
    prefPath.value = res.path || ''
    prefDirty.value = false
  } catch (e) {
    ElMessage.error('项目偏好加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    prefLoading.value = false
  }
}

async function savePrefs() {
  prefSaving.value = true
  try {
    await saveProjectPreferences(prefContent.value, dir.value || undefined)
    prefDirty.value = false
    ElMessage.success('项目偏好已保存')
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    prefSaving.value = false
  }
}

async function loadTester() {
  testerLoading.value = true
  try {
    const res = await getTesterConfig(dir.value || undefined)
    testerMode.value = res.mode || 'off'
    testerPrompt.value = res.auto_prompt || ''
    testerRounds.value = res.max_rounds || 2
  } catch (e) {
    ElMessage.error('测试助手配置加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    testerLoading.value = false
  }
}

async function saveTester() {
  testerSaving.value = true
  try {
    await saveTesterConfig(
      { mode: testerMode.value, auto_prompt: testerPrompt.value, max_rounds: testerRounds.value },
      dir.value || undefined,
    )
    ElMessage.success('测试助手配置已保存')
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    testerSaving.value = false
  }
}
</script>

<template>
  <el-drawer v-model="visible" :title="'工作目录设置'" size="520px" append-to-body>
    <div class="h-full flex flex-col gap-4 text-ink">
      <div class="text-xs text-ink-3 font-mono truncate" :title="dir || '默认目录'">{{ dirLabel() }}</div>

      <!-- 项目偏好 -->
      <div class="bg-card border border-line rounded-card flex flex-col overflow-hidden flex-1 min-h-[260px]">
        <div class="px-4 py-3 border-b border-line flex items-center justify-between gap-2">
          <div class="min-w-0">
            <div class="font-bold text-sm">项目偏好</div>
            <div class="text-[11px] text-ink-3 font-mono truncate">{{ prefPath || dirLabel() }}</div>
          </div>
          <div class="flex items-center gap-2 shrink-0">
            <el-tag v-if="prefDirty" size="small" type="warning" effect="plain">未保存</el-tag>
            <el-button size="small" plain :loading="prefLoading" @click="loadPrefs"><el-icon><Refresh /></el-icon></el-button>
            <el-button size="small" type="primary" :loading="prefSaving" :disabled="!prefDirty" @click="savePrefs">保存</el-button>
          </div>
        </div>
        <div class="flex-1 min-h-0 p-3">
          <el-input
            v-model="prefContent"
            type="textarea"
            spellcheck="false"
            class="prefs-editor h-full"
            placeholder="「项目约定」人工维护；「项目经验」会话结束自动沉淀，注入每个 Agent 上下文。"
            @input="prefDirty = true"
          />
        </div>
        <div class="px-4 py-2 border-t border-line text-[11px] text-ink-3">
          改完即时生效（下次派发即注入）；自动沉淀的行带时间戳，人工行程序永不改写。
        </div>
      </div>

      <!-- 测试助手（验收）：工作目录级三态开关 -->
      <div v-loading="testerLoading" class="bg-card border border-line rounded-card p-4 shrink-0">
        <div class="font-bold text-sm mb-1">测试助手</div>
        <p class="text-xs text-ink-2">
          任务完成后的验收策略，按当前工作目录生效：关=不执行；智能=按描述由模型判断命中才执行；总是=每次完成后都验收。
        </p>
        <el-radio-group v-model="testerMode" class="mt-3">
          <el-radio value="off">关</el-radio>
          <el-radio value="auto">智能</el-radio>
          <el-radio value="on">总是</el-radio>
        </el-radio-group>
        <el-input
          v-if="testerMode === 'auto'"
          v-model="testerPrompt"
          type="textarea"
          :rows="3"
          spellcheck="false"
          class="mt-2"
          placeholder="描述什么样的任务需要验收，例如「涉及前端页面交付、需要真机点击验证的任务」"
        />
        <div class="mt-3 flex items-center gap-2">
          <span class="text-xs text-ink-2 shrink-0">最大复验轮次</span>
          <el-input-number v-model="testerRounds" :min="1" :max="5" size="small" />
          <el-button size="small" type="primary" :loading="testerSaving" class="ml-auto" @click="saveTester">保存</el-button>
        </div>
      </div>
    </div>
  </el-drawer>
</template>

<style scoped>
.prefs-editor :deep(.el-textarea__inner) {
  height: 100%;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.7;
  background: var(--bma-page);
  color: var(--bma-text);
}
</style>
