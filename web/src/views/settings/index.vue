<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { getHealth, getStatus, type HealthResponse, type StatusResponse } from '@/api/health'
import { useTheme } from '@/composables/useTheme'

const { theme, toggleTheme } = useTheme()

// 单选绑定：值变化才触发 toggle，避免初始化误切换
const themeModel = computed<'light' | 'dark'>({
  get: () => theme.value,
  set: (v) => { if (v !== theme.value) toggleTheme() },
})

const status = ref<StatusResponse | null>(null)
const health = ref<HealthResponse | null>(null)

onMounted(async () => {
  try { status.value = await getStatus() } catch { status.value = null }
  try { health.value = await getHealth() } catch { health.value = null }
})

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
