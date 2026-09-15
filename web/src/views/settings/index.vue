<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { getCapabilities, getHealth, getStatus, type CapabilitiesResponse, type HealthResponse, type StatusResponse } from '@/api/health'
import { notificationPermission, requestNotificationPermission } from '@/utils/notifications'
import { useTheme } from '@/composables/useTheme'

const { theme, toggleTheme } = useTheme()

// 单选绑定：值变化才触发 toggle，避免初始化误切换
const themeModel = computed<'light' | 'dark'>({
  get: () => theme.value,
  set: (v) => { if (v !== theme.value) toggleTheme() },
})

const status = ref<StatusResponse | null>(null)
const health = ref<HealthResponse | null>(null)

// 能力自检（T28）：六类能力逐项 ok/缺失/修复提示，装机排障一屏看完
const caps = ref<CapabilitiesResponse | null>(null)
const capsLoading = ref(false)
async function loadCaps() {
  capsLoading.value = true
  try { caps.value = await getCapabilities() } catch { caps.value = null } finally { capsLoading.value = false }
}

onMounted(async () => {
  try { status.value = await getStatus() } catch { status.value = null }
  try { health.value = await getHealth() } catch { health.value = null }
  void loadCaps()
})

// 能力名 → 展示名（其余原样直出）
const capLabels: Record<string, string> = {
  llm: 'LLM 模型',
  postgres: 'Postgres',
  redis: 'Redis',
  embed: '向量嵌入',
  plugins: '插件',
  workdir: '工作目录',
}

// 浏览器通知权限（T32）：页面在后台时收会话终态提醒
const perm = ref(notificationPermission())
async function askPermission() {
  perm.value = await requestNotificationPermission()
}
const permLabel = computed(() => ({
  unsupported: '当前浏览器不支持',
  default: '未授权',
  granted: '已授权',
  denied: '已禁止（需在浏览器站点设置里恢复）',
}[perm.value] || perm.value))

function dot(service?: { online?: boolean }) {
  return service?.online ? 'bg-green-500' : 'bg-red-500'
}
</script>

<template>
  <div class="h-full overflow-y-auto text-ink">
    <div class="max-w-3xl space-y-4">
      <div class="bg-card border border-line rounded-card p-5">
        <div class="font-bold text-sm mb-3">外观</div>
        <el-radio-group v-model="themeModel">
          <el-radio-button value="light">浅色</el-radio-button>
          <el-radio-button value="dark">深色</el-radio-button>
        </el-radio-group>
        <p class="text-xs text-ink-3 mt-2">主题偏好保存在本机浏览器（localStorage bma:theme）。</p>
      </div>

      <div class="bg-card border border-line rounded-card p-5">
        <div class="font-bold text-sm mb-3">服务信息</div>
        <div class="text-sm space-y-2">
          <div class="flex justify-between"><span class="text-ink-2">程序</span><span>{{ status?.program || '-' }}</span></div>
          <div class="flex justify-between"><span class="text-ink-2">运行模式</span><span>{{ status?.mode || '-' }}</span></div>
          <div class="flex justify-between"><span class="text-ink-2">当前人格</span><span>{{ status?.soul || '-' }}</span></div>
          <div class="flex justify-between">
            <span class="text-ink-2">LLM</span>
            <span class="font-mono text-xs">{{ status?.llm_provider || '-' }} / {{ status?.llm_model || '-' }}</span>
          </div>
        </div>
      </div>

      <div class="bg-card border border-line rounded-card p-5">
        <div class="font-bold text-sm mb-3">连接健康</div>
        <div class="text-sm space-y-2">
          <div v-for="svc in [
            { key: 'postgres', label: 'Postgres' },
            { key: 'redis', label: 'Redis' },
            { key: 'llm', label: 'LLM API' },
          ]" :key="svc.key" class="flex items-center justify-between">
            <span class="text-ink-2">{{ svc.label }}</span>
            <span class="flex items-center gap-2">
              <span class="w-2 h-2 rounded-full inline-block" :class="dot((health as any)?.[svc.key])"></span>
              <span class="text-xs text-ink-3">{{ (health as any)?.[svc.key]?.detail || '未知' }}</span>
            </span>
          </div>
        </div>
      </div>

      <!-- 装机能力自检（T28）：ok 绿点 + 现状；失败红点 + 缺失项 + 可行动修复提示 -->
      <div class="bg-card border border-line rounded-card p-5">
        <div class="flex justify-between items-center mb-3">
          <div class="font-bold text-sm">能力自检</div>
          <el-button size="small" text :loading="capsLoading" @click="loadCaps">重新检测</el-button>
        </div>
        <template v-if="caps">
          <div class="text-sm space-y-2">
            <div v-for="it in caps.items" :key="it.name" class="flex items-start justify-between gap-3">
              <span class="text-ink-2 shrink-0">{{ capLabels[it.name] || it.name }}</span>
              <span class="flex items-start gap-2 text-right">
                <span class="flex flex-col items-end gap-0.5">
                  <span class="text-xs text-ink-3">{{ it.detail || (it.ok ? '就绪' : '未就绪') }}</span>
                  <span v-if="!it.ok && it.hint" class="text-[11px] text-amber-600 dark:text-amber-400 leading-5">{{ it.hint }}</span>
                </span>
                <span class="w-2 h-2 rounded-full inline-block mt-1.5 shrink-0" :class="it.ok ? 'bg-green-500' : 'bg-red-500'"></span>
              </span>
            </div>
          </div>
        </template>
        <p v-else class="text-xs text-ink-3">能力自检不可用（接口请求失败或服务未启动）。</p>
      </div>

      <!-- 浏览器通知（T32）：页面在后台时收会话终态提醒 -->
      <div class="bg-card border border-line rounded-card p-5">
        <div class="flex justify-between items-center">
          <div>
            <div class="font-bold text-sm mb-1">桌面通知</div>
            <p class="text-xs text-ink-3 leading-5">页面切到后台时，会话完成/失败弹出系统级提醒；前台使用不打扰。权限状态：{{ permLabel }}</p>
          </div>
          <el-button v-if="perm === 'default'" size="small" type="primary" plain @click="askPermission">申请权限</el-button>
          <el-tag v-else-if="perm === 'granted'" size="small" type="success" effect="plain">已开启</el-tag>
          <el-tag v-else size="small" type="info" effect="plain">不可用</el-tag>
        </div>
      </div>

      <div class="bg-card border border-line rounded-card p-5">
        <div class="font-bold text-sm mb-3">资源目录</div>
        <p class="text-xs text-ink-2 leading-6">
          配置文件、技能、插件与会话数据均位于安装目录（BMA_HOME）：
          <br /><span class="font-mono">config/</span>（config.yaml、plugins.yaml、roles.yaml、skills.yaml、soul.md、user_profile.md）
          <br /><span class="font-mono">config/plugins.d/</span>（外部插件包，经插件页「重载配置」扫描装载）
          <br /><span class="font-mono">config/skills_learned/</span>（自进化沉淀的技能包）
          <br /><span class="font-mono">workspace/</span>（默认工作目录）
        </p>
      </div>
    </div>
  </div>
</template>
