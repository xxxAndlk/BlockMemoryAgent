# Web 端 UI 重设计 — 实现计划

> **REQUIRED SUB-SKILL: superpowers:subagent-driven-development** — 本计划按任务逐个派发子代理执行。
> 设计依据：`docs/superpowers/specs/2026-09-03-web-ui-redesign-design.md`（精简 spec）。
> 视觉基准：`doc/image/redesign/01–16*.png`（用户已逐页确认；HTML 源稿在 `doc/image/redesign/src/`）。

## Goal

把 BMA Web 端从「深色单主题 + 11 个平铺页面」重构为「浅色优先双主题 + 分组导航 + 会话双视图单页」，共 12 条有效路由 + 1 个 Beta 占位，与预览图逐页对齐。

## Architecture

- 主题：CSS 变量语义 token（`--bma-*`）挂在 `:root`（浅色默认）与 `html.dark`（深色）；tailwind 颜色映射到这些变量；element-plus 用 `--el-*` 覆写跟随。旧 tailwind 色名（`dark-bg` 等）保留为变量别名，保证重构中期不断裂。
- 布局：`web/src/layout/index.vue` 全量重写——顶栏（品牌 + 健康点 + 主题切换 + 设置入口），侧栏 5 个静态分组 + 底部「工作流编排 Beta」虚线预留项。菜单不再从 router 派生，改静态声明。
- 会话页合并：`views/session/index.vue` 为唯一容器，持有全部会话状态（会话列表、SSE、提交/取消/软停止/中断、面板数据）；中栏两个展示组件 `ChatView.vue` / `MonitorView.vue` 按 `?view=` 切换；右栏统一 5 Tab 面板。
- 文件迁移：`web/src/views/chat/` 整目录移入 `web/src/views/session/chat/`；`@/views/chat` 绝对引用全部改写。
- 数据源约束：只用现有后端 API（见 spec）。`/api/memory/search|levels|eval` 是 ReAct 重构后的空壳桩，**禁止接入**。

## Tech Stack

Vue 3.4 `<script setup>`、element-plus 2.7（`html.dark` 暗黑 css-vars 已在 main.ts 引入）、tailwind 3.4（`darkMode: 'class'`）、vue-router 4、@vueuse。构建即类型检查：`cd web && npm run build`（`vue-tsc -b && vite build`）。**前端无测试框架，不写单测**。

## Global Constraints（所有任务必须遵守）

1. **禁 git 任何变异操作**（commit/mv/rebase 等一律不做；文件搬运用普通 `mv`）。
2. 设计 token 精确值（写进 `:root` / `html.dark`）：
   - `--bma-primary: #4F5DFF`（dark: `#6B77FF`），`--bma-primary-hover: #3D4BE0`（dark: `#8A94FF`），`--bma-primary-soft: #EEF0FF`（dark: `#232947`）
   - `--bma-page: #F6F7F9`（dark: `#0F1115`），`--bma-card: #FFFFFF`（dark: `#1A1D24`），`--bma-border: #E5E7EB`（dark: `#2A2D35`）
   - `--bma-text: #1F2329`（dark: `#E5E7EB`），`--bma-text-2: #6B7280`（dark: `#9CA3AF`），`--bma-text-3: #9CA3AF`（dark: `#6B7280`）
   - 语义色：`--bma-success: #16A34A`、`--bma-danger: #DC2626`、`--bma-warning: #D97706`（两主题同值）
   - 卡片圆角 10px、阴影 `0 1px 2px rgba(16,24,40,.05)`（dark 用 `0 1px 2px rgba(0,0,0,.4)`）
3. 类名替换映射（restyle 任务的确定性变换表，逐字替换，禁止自由发挥）：
   - `bg-[#0f1115]` / `bg-dark-bg` → `bg-page`；`bg-[#1a1d24]` / `!bg-[#1a1d24]` / `bg-dark-panel` → `bg-card`（el-card 上用 `!bg-card`）
   - `bg-[#14161a]` → `bg-card`；`border-[#2a2d35]` / `!border-[#2a2d35]` / `border-dark-border` → `border-line` / `!border-line`
   - `text-gray-200` / `text-gray-300` → `text-ink`；`text-gray-400` → `text-ink-2`；`text-gray-500` → `text-ink-2`；`text-gray-600` → `text-ink-3`
   - `bg-[#1e3a8a]/30` / `bg-[#1e3a8a]/20` → `bg-primary-soft`；`#1e3a8a` 其余 → `var(--bma-primary)`
   - `hover:bg-[#2a2d35]` → `hover:bg-page`；内嵌深色面板 `bg-[#0f1115]` 且带边框的统计小块 → `bg-page border border-line`
4. 每个任务收尾必须 `cd web && npm run build` 通过（vue-tsc 零报错），再按任务指定的预览图核对结构。
5. 新图标必须在 `web/src/main.ts` 两个数组（import 与 icons 列表）同时注册后才可在模板使用。
6. 后端零改动；发现 API 缺失一律走占位/静态说明，不得发明接口。

---

## Task 1: 主题基础设施（token / tailwind / useTheme / 图标注册）

**Files**:
- Modify: `web/src/style.css`（全量重写）
- Modify: `web/tailwind.config.js`（全量重写）
- Create: `web/src/composables/useTheme.ts`
- Modify: `web/src/main.ts`（注册新图标）

**Interfaces**:
- Produces: `useTheme(): { theme: Ref<'light'|'dark'>, toggleTheme: () => void }`
- Produces: tailwind 语义色类 `bg-page/bg-card/border-line/text-ink/text-ink-2/text-ink-3/bg-primary/bg-primary-soft/text-primary`

- [ ] **Step 1.1: 全量重写 `web/src/style.css`**

```css
@tailwind base;
@tailwind components;
@tailwind utilities;

/* ===== BMA 设计 Token：浅色默认，html.dark 深色（localStorage bma:theme 持久化）===== */
:root {
  --bma-primary: #4F5DFF;
  --bma-primary-hover: #3D4BE0;
  --bma-primary-soft: #EEF0FF;
  --bma-page: #F6F7F9;
  --bma-card: #FFFFFF;
  --bma-border: #E5E7EB;
  --bma-text: #1F2329;
  --bma-text-2: #6B7280;
  --bma-text-3: #9CA3AF;
  --bma-success: #16A34A;
  --bma-danger: #DC2626;
  --bma-warning: #D97706;
  --bma-card-shadow: 0 1px 2px rgba(16, 24, 40, .05);

  /* Element Plus 浅色覆写 */
  --el-color-primary: var(--bma-primary);
  --el-color-primary-light-3: #7C86FF;
  --el-color-primary-light-5: #A4ABFF;
  --el-color-primary-light-7: #CCD0FF;
  --el-color-primary-light-8: #E0E3FF;
  --el-color-primary-light-9: var(--bma-primary-soft);
  --el-color-primary-dark-2: var(--bma-primary-hover);
  --el-bg-color: #FFFFFF;
  --el-bg-color-overlay: #FFFFFF;
  --el-bg-color-page: var(--bma-page);
  --el-text-color-primary: var(--bma-text);
  --el-text-color-regular: var(--bma-text-2);
  --el-text-color-secondary: var(--bma-text-3);
  --el-border-color: var(--bma-border);
  --el-border-color-light: var(--bma-border);
  --el-border-color-lighter: #EEF0F3;
  --el-fill-color-blank: #FFFFFF;
  --el-fill-color-light: #F3F4F6;
  --el-menu-bg-color: transparent;
  --el-menu-hover-bg-color: var(--bma-primary-soft);
  --el-menu-text-color: var(--bma-text-2);
  --el-menu-active-color: var(--bma-primary);
}

html.dark {
  --bma-primary: #6B77FF;
  --bma-primary-hover: #8A94FF;
  --bma-primary-soft: #232947;
  --bma-page: #0F1115;
  --bma-card: #1A1D24;
  --bma-border: #2A2D35;
  --bma-text: #E5E7EB;
  --bma-text-2: #9CA3AF;
  --bma-text-3: #6B7280;
  --bma-card-shadow: 0 1px 2px rgba(0, 0, 0, .4);

  /* Element Plus 深色覆写（theme-chalk/dark/css-vars.css 已提供基底，这里对齐品牌色） */
  --el-color-primary: var(--bma-primary);
  --el-bg-color: #1A1D24;
  --el-bg-color-overlay: #1A1D24;
  --el-bg-color-page: var(--bma-page);
  --el-text-color-primary: #E5E7EB;
  --el-text-color-regular: #9CA3AF;
  --el-border-color: #2A2D35;
  --el-border-color-light: #2A2D35;
  --el-border-color-lighter: #33373F;
  --el-fill-color-blank: #1A1D24;
  --el-fill-color-light: #23262E;
  --el-menu-bg-color: transparent;
  --el-menu-hover-bg-color: #2A2D35;
  --el-menu-text-color: #9CA3AF;
  --el-menu-active-color: #FFFFFF;
}

body {
  margin: 0;
  background-color: var(--bma-page);
  color: var(--bma-text);
}

/* 等宽工具类：路径 / 日志 / 代码 */
.font-mono,
code,
pre {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}

.el-menu {
  border-right: none !important;
}

.el-card {
  background-color: var(--bma-card);
  border-color: var(--bma-border);
  border-radius: 10px;
  box-shadow: var(--bma-card-shadow);
  color: var(--bma-text);
}

.el-table {
  background-color: var(--bma-card);
  --el-table-border-color: var(--bma-border);
  --el-table-header-bg-color: var(--bma-card);
  --el-table-row-hover-bg-color: var(--bma-primary-soft);
}

.el-table th.el-table__cell {
  background-color: var(--bma-card);
}

.el-table tr {
  background-color: var(--bma-card);
}
```

- [ ] **Step 1.2: 全量重写 `web/tailwind.config.js`**

```js
/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: [
    "./index.html",
    "./src/**/*.{vue,js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        // 语义 token（跟随 CSS 变量，自动双主题）
        'primary': 'var(--bma-primary)',
        'primary-soft': 'var(--bma-primary-soft)',
        'page': 'var(--bma-page)',
        'card': 'var(--bma-card)',
        'line': 'var(--bma-border)',
        'ink': 'var(--bma-text)',
        'ink-2': 'var(--bma-text-2)',
        'ink-3': 'var(--bma-text-3)',
        // 旧色名别名：重构过渡期保持可编译，全部映射到新变量
        'dark-bg': 'var(--bma-page)',
        'dark-panel': 'var(--bma-card)',
        'dark-border': 'var(--bma-border)',
      },
      borderRadius: {
        card: '10px',
      },
      boxShadow: {
        card: 'var(--bma-card-shadow)',
      },
    },
  },
  plugins: [],
}
```

- [ ] **Step 1.3: 新建 `web/src/composables/useTheme.ts`**

```ts
import { ref } from 'vue'

const KEY = 'bma:theme'
type Theme = 'light' | 'dark'

const theme = ref<Theme>(localStorage.getItem(KEY) === 'dark' ? 'dark' : 'light')

function apply(t: Theme) {
  document.documentElement.classList.toggle('dark', t === 'dark')
}

// 模块加载即应用，保证首屏不出现错误主题闪烁。
apply(theme.value)

export function useTheme() {
  const toggleTheme = () => {
    theme.value = theme.value === 'dark' ? 'light' : 'dark'
    localStorage.setItem(KEY, theme.value)
    apply(theme.value)
  }
  return { theme, toggleTheme }
}
```

- [ ] **Step 1.4: `web/src/main.ts` 注册新图标**

在 import 块追加（保持字母序插入对应位置）：`DataAnalysis`、`FolderAdd`、`Moon`、`SetUp`、`Sunny`；并把这 5 个组件名同样追加到 `icons` 数组。其余不动。

- [ ] **Step 1.5: 验证**

```bash
cd web && npm run build
```

预期：编译通过；旧页面此时仍是旧深色类名但经由别名变量呈现（浅色默认）。打开页面确认底色变浅即 token 生效。

---

## Task 2: Layout 重构（顶栏 + 分组侧栏 + Beta 预留项）

**Depends on**: Task 1
**Files**:
- Modify: `web/src/layout/index.vue`（全量重写）

**Interfaces**:
- Consumes: `useTheme()`（Task 1）、`getHealth/getStatus`（`@/api/health`，现状不变）
- Produces: 全局框架；侧栏静态分组菜单；底部 Beta 预留项链至 `/workflow`

- [ ] **Step 2.1: 全量重写 `web/src/layout/index.vue`**

```vue
<template>
  <div class="flex flex-col h-screen w-full overflow-hidden bg-page text-ink">
    <!-- Header -->
    <header class="h-14 bg-card border-b border-line flex items-center px-4 shrink-0">
      <div class="flex items-center w-52 shrink-0">
        <el-icon class="text-primary text-2xl mr-2"><Grid /></el-icon>
        <span class="font-bold text-lg">BlockMemoryAgent</span>
      </div>

      <div class="flex items-center space-x-6 text-sm ml-auto mr-4 shrink-0">
        <span class="flex items-center text-ink-2" title="Postgres">
          <span class="w-2 h-2 rounded-full mr-2 inline-block" :class="healthDot(health?.postgres)"></span>Postgres
        </span>
        <span class="flex items-center text-ink-2" title="Redis">
          <span class="w-2 h-2 rounded-full mr-2 inline-block" :class="healthDot(health?.redis)"></span>Redis
        </span>
        <span class="flex items-center text-ink-2" title="LLM API">
          <span class="w-2 h-2 rounded-full mr-2 inline-block" :class="healthDot(health?.llm)"></span>LLM API
        </span>
      </div>

      <div class="flex items-center space-x-3 shrink-0">
        <el-icon class="text-xl cursor-pointer text-ink-2 hover:text-primary" @click="toggleTheme">
          <Moon v-if="theme === 'light'" /><Sunny v-else />
        </el-icon>
        <router-link to="/settings">
          <el-icon class="text-xl cursor-pointer text-ink-2 hover:text-primary"><Setting /></el-icon>
        </router-link>
      </div>
    </header>

    <!-- Main Body -->
    <div class="flex flex-1 min-h-0">
      <!-- Sidebar -->
      <div class="w-56 bg-card border-r border-line flex flex-col shrink-0">
        <el-menu
          :default-active="route.path"
          class="flex-1 overflow-y-auto !border-r-0 py-3"
          background-color="transparent"
          router
        >
          <el-menu-item-group v-for="g in menuGroups" :key="g.title">
            <template #title>
              <span class="text-xs text-ink-3">{{ g.title }}</span>
            </template>
            <el-menu-item v-for="item in g.items" :key="item.path" :index="item.path"
                          :class="{ 'is-active-custom': route.path === item.path }">
              <el-icon><component :is="item.icon" /></el-icon>
              <span>{{ item.title }}</span>
            </el-menu-item>
          </el-menu-item-group>
        </el-menu>

        <!-- Beta 预留：工作流编排 -->
        <div class="px-3 pb-3">
          <router-link to="/workflow"
                       class="flex items-center justify-between px-3 py-2.5 rounded-card border border-dashed border-line text-ink-3 hover:text-primary hover:border-primary transition-colors">
            <span class="flex items-center gap-2 text-sm">
              <el-icon><SetUp /></el-icon>工作流编排
            </span>
            <el-tag size="small" effect="plain" type="warning">Beta</el-tag>
          </router-link>
        </div>

        <div class="p-4 text-xs text-ink-3 border-t border-line flex justify-between items-center">
          <div>
            <div>当前人格</div>
            <div class="text-ink-2">{{ soulName }}</div>
          </div>
          <el-icon class="cursor-pointer hover:text-primary" @click="loadHealth"><Refresh /></el-icon>
        </div>
      </div>

      <!-- Content -->
      <main class="flex-1 overflow-y-auto p-6 bg-page">
        <router-view v-slot="{ Component }">
          <transition name="fade" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { getHealth, getStatus, type HealthResponse } from '@/api/health'
import { useTheme } from '@/composables/useTheme'

const route = useRoute()
const { theme, toggleTheme } = useTheme()

const health = ref<HealthResponse | null>(null)
const soulName = ref('default')

let healthTimer: ReturnType<typeof setInterval> | null = null

async function loadStatus() {
  try {
    const s = await getStatus()
    if (s?.soul) soulName.value = s.soul
  } catch {
    // ignore
  }
}

async function loadHealth() {
  try {
    health.value = await getHealth()
  } catch {
    health.value = null
  }
}

onMounted(() => {
  loadStatus()
  loadHealth()
  healthTimer = setInterval(loadHealth, 10000)
})

onUnmounted(() => {
  if (healthTimer) clearInterval(healthTimer)
})

function healthDot(service?: { online?: boolean }) {
  return service?.online ? 'bg-green-500' : 'bg-red-500'
}

// 静态分组菜单（IA 定稿：4 组 + 底部 Beta 预留项，见 spec §信息架构）
const menuGroups = [
  {
    title: '工作台',
    items: [
      { path: '/dashboard', title: '首页', icon: 'House' },
      { path: '/session', title: '会话', icon: 'ChatDotRound' },
    ],
  },
  {
    title: '项目',
    items: [{ path: '/projects', title: '工作目录', icon: 'FolderOpened' }],
  },
  {
    title: '资源库',
    items: [
      { path: '/skills', title: '技能库', icon: 'Connection' },
      { path: '/plugins', title: '插件', icon: 'MagicStick' },
      { path: '/knowledge', title: '知识库', icon: 'Document' },
    ],
  },
  {
    title: '记忆',
    items: [
      { path: '/memory', title: '记忆中心', icon: 'Coin' },
      { path: '/profile', title: '用户画像', icon: 'UserFilled' },
      { path: '/soul', title: '人格配置', icon: 'User' },
      { path: '/history', title: '会话历史', icon: 'Clock' },
    ],
  },
  {
    title: '系统',
    items: [{ path: '/settings', title: '系统设置', icon: 'Setting' }],
  },
]
</script>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.2s ease;
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

:deep(.el-menu-item) {
  border-radius: 8px;
  margin: 0 8px;
  height: 40px;
  line-height: 40px;
}

:deep(.el-menu-item-group__title) {
  padding: 8px 16px 4px;
}

.is-active-custom {
  background-color: var(--bma-primary-soft) !important;
  color: var(--bma-primary) !important;
  font-weight: 600;
}
</style>
```

注意：原顶栏的「人格」下拉为硬编码假选项，删除；人格名移到侧栏底部只读展示（来自 `/api/status`）。

- [ ] **Step 2.2: 验证**

```bash
cd web && npm run build
```

预期：通过。此时 `/projects`、`/memory`、`/workflow` 尚无路由，点击会 404 空白——Task 3 补齐，本任务不处理。

---

## Task 3: 路由重排与旧路由重定向

**Depends on**: Task 2
**Files**:
- Modify: `web/src/router/index.ts`（全量重写）
- Create: `web/src/views/workflow/index.vue`（占位，避免路由 404；正式视觉在 Task 14 打磨）

**Interfaces**:
- Produces: 12 条业务路由 + 2 条重定向（`/chat`→`/session` 保留 query；`/project-prefs`→`/projects`）

- [ ] **Step 3.1: 全量重写 `web/src/router/index.ts`**

```ts
import { createRouter, createWebHistory, RouteRecordRaw } from 'vue-router'
import Layout from '@/layout/index.vue'

const routes: Array<RouteRecordRaw> = [
  {
    path: '/',
    component: Layout,
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'Dashboard',
        component: () => import('@/views/dashboard/index.vue'),
        meta: { title: '首页', icon: 'House' }
      },
      {
        // 会话页：对话 / 监控同一页两视图，?view=chat|monitor 切换（缺省 chat）
        path: 'session',
        name: 'Session',
        component: () => import('@/views/session/index.vue'),
        meta: { title: '会话', icon: 'ChatDotRound' }
      },
      {
        path: 'projects',
        name: 'Projects',
        component: () => import('@/views/projects/index.vue'),
        meta: { title: '工作目录', icon: 'FolderOpened' }
      },
      {
        path: 'skills',
        name: 'Skills',
        component: () => import('@/views/skills/index.vue'),
        meta: { title: '技能库', icon: 'Connection' }
      },
      {
        path: 'plugins',
        name: 'Plugins',
        component: () => import('@/views/plugins/index.vue'),
        meta: { title: '插件', icon: 'MagicStick' }
      },
      {
        path: 'knowledge',
        name: 'Knowledge',
        component: () => import('@/views/knowledge/index.vue'),
        meta: { title: '知识库', icon: 'Document' }
      },
      {
        path: 'memory',
        name: 'Memory',
        component: () => import('@/views/memory/index.vue'),
        meta: { title: '记忆中心', icon: 'Coin' }
      },
      {
        path: 'profile',
        name: 'Profile',
        component: () => import('@/views/profile/index.vue'),
        meta: { title: '用户画像', icon: 'UserFilled' }
      },
      {
        path: 'soul',
        name: 'Soul',
        component: () => import('@/views/soul/index.vue'),
        meta: { title: '人格配置', icon: 'User' }
      },
      {
        path: 'history',
        name: 'History',
        component: () => import('@/views/history/index.vue'),
        meta: { title: '会话历史', icon: 'Clock' }
      },
      {
        path: 'settings',
        name: 'Settings',
        component: () => import('@/views/settings/index.vue'),
        meta: { title: '系统设置', icon: 'Setting' }
      },
      {
        path: 'workflow',
        name: 'Workflow',
        component: () => import('@/views/workflow/index.vue'),
        meta: { title: '工作流编排', icon: 'SetUp' }
      }
    ]
  },
  // 旧路由兼容：/chat 并入 /session（保留 ?id=）；/project-prefs 并入 /projects
  { path: '/chat', redirect: (to) => ({ path: '/session', query: to.query }) },
  { path: '/project-prefs', redirect: '/projects' }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

export default router
```

- [ ] **Step 3.2: 新建占位 `web/src/views/workflow/index.vue`**（Task 14 会替换为正式版）

```vue
<template>
  <div class="h-full">
    <FeaturePlaceholder
      icon="SetUp"
      title="工作流编排（Beta）"
      description="可视化编排多 Agent 工作流：拖拽节点、声明依赖、运行观测。该能力为 Beta 预留，入口先行开放。"
    />
  </div>
</template>

<script setup lang="ts">
import FeaturePlaceholder from '@/components/FeaturePlaceholder.vue'
</script>
```

- [ ] **Step 3.3: 临时空页兜底**

`/projects` 与 `/memory` 的视图文件在 Task 9 / Task 12 才创建。为保证本任务构建不断，先各建一个临时文件（内容同 Step 3.2 结构，标题分别「工作目录」「记忆中心」，icon 分别 `FolderOpened` / `Coin`），后续任务整体覆盖。**vue-tsc 对未创建的懒加载文件不报错，但 vite dev 运行会 404，故必须建。**

- [ ] **Step 3.4: 验证**

```bash
cd web && npm run build
```

预期：通过；侧栏全部条目可跳转，无空白页。

---

## Task 4: 首页改造（dashboard 换 token，对照 01-dashboard.png）

**Depends on**: Task 3
**Files**:
- Modify: `web/src/views/dashboard/index.vue`

**Interfaces**:
- Consumes（不变）: `listSessions/createSession`、`getTimeline/getActivity`、`useWorkDir`、`WorkDirPicker`

- [ ] **Step 4.1: 应用类名映射**

对全文件应用 Global Constraints §3 映射表逐字替换。完成后文件内不得再出现 `#0f1115`、`#1a1d24`、`#14161a`、`#2a2d35`、`#1e3a8a`、`gray-*` 以外的 tailwind 色（`text-green-500` 等语义色保留）。

- [ ] **Step 4.2: 替换 `<style scoped>` 整块**

```css
<style scoped>
:deep(.body-flex-1 .el-card__body) {
  flex: 1;
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:deep(.el-textarea__inner) {
  background-color: transparent;
  box-shadow: none !important;
  color: var(--bma-text);
}
:deep(.el-textarea__inner:focus) {
  box-shadow: none !important;
}
:deep(.el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
}
:deep(.el-pagination.is-background .el-pager li:not(.is-disabled).is-active) {
  background-color: var(--bma-primary);
}
</style>
```

- [ ] **Step 4.3: 三处定点修改**

1. Mock 提示条：`bg-yellow-900/30 border border-yellow-700/50 text-yellow-300` → `bg-amber-50 border border-amber-300 text-amber-700 dark:bg-yellow-900/30 dark:border-yellow-700/50 dark:text-yellow-300`。
2. Token 趋势 SVG 容器 `class="h-40 bg-[#0f1115] rounded border border-[#2a2d35] p-3"` → `class="h-40 bg-page rounded-card border border-line p-3"`；SVG 的 `class="w-full h-full text-blue-500"` → `class="w-full h-full text-primary"`。
3. 「查看更多」按钮旁的六边形插画 SVG 三个 gradient 色保持不动（品牌插画，双主题通用）。

- [ ] **Step 4.4: 验证**

```bash
cd web && npm run build
```

对照 `doc/image/redesign/01-dashboard.png`：左列创建卡 + 会话列表、右列统计 + 活动的结构与浅色观感一致。

---

## Task 5: 会话页容器（文件搬迁 + views/session/index.vue 全量重写）

**Depends on**: Task 3
**Files**:
- Move: `web/src/views/chat/components/*` → `web/src/views/session/chat/components/*`
- Move: `web/src/views/chat/utils/*` → `web/src/views/session/chat/utils/*`
- Delete: `web/src/views/chat/index.vue`（容器取代它；不删则 vue-tsc 报悬空引用）
- Modify: `web/src/views/dashboard/index.vue`（改 eventStyles 引用路径）
- Modify: `web/src/views/session/index.vue`（全量重写为容器）

**Interfaces**:
- Consumes: `@/api/session` 全集（create/send/clarify/cancel/stop/enqueue/interrupt/get/getAgents/getBoard/getMailbox/getLogs）、`getSessionMetrics/getSessionTokenMetrics`、`getHealth`、`useSessionStream/usePanelRefresh/useSessionList/useSessionStatus/useWorkDir`
- Produces: 单页双视图会话页；`?id=` 会话定位、`?view=chat|monitor` 视图切换、`?work_dir=` 预填工作目录

- [ ] **Step 5.1: 文件搬迁**

```bash
cd web/src/views
mkdir -p session/chat
mv chat/components session/chat/components
mv chat/utils session/chat/utils
rm chat/index.vue && rmdir chat
```

然后：
- `web/src/views/dashboard/index.vue` 第 10 行 `from '@/views/chat/utils/eventStyles'` → `from '@/views/session/chat/utils/eventStyles'`。
- 全局搜索确认无残留：`grep -rn "@/views/chat" web/src` 必须零命中（组件间相对引用 `./Xxx.vue`、`../utils/eventStyles` 随目录整体搬迁，不受影响）。

- [ ] **Step 5.2: 全量重写 `web/src/views/session/index.vue`**

```vue
<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import type { Session, SessionEvent, AgentNode, TaskBoardData, ClarifyOption, WireImage } from '@/types'
import {
  createSession,
  sendMessage,
  clarifySession,
  cancelSession,
  stopSession,
  enqueueSession,
  interruptSession,
  getSession,
  getSessionAgents,
  getSessionBoard,
  getSessionMailbox,
  getSessionLogs,
  type MailboxMessage,
  type SessionLog,
} from '@/api/session'
import { getHealth, type HealthResponse } from '@/api/health'
import {
  getSessionMetrics,
  getSessionTokenMetrics,
  type SessionMetrics,
  type SessionTokenMetricsResponse,
} from '@/api/metrics'
import { useSessionStream } from '@/composables/useSessionStream'
import { usePanelRefresh } from '@/composables/usePanelRefresh'
import { useSessionList } from '@/composables/useSessionList'
import { useSessionStatus } from '@/composables/useSessionStatus'
import { useWorkDir } from '@/composables/useWorkDir'
import ChatView from './chat/ChatView.vue'
import MonitorView from './monitor/MonitorView.vue'
import TaskBoardPanel from './components/panels/TaskBoardPanel.vue'
import ToolPanel from './components/panels/ToolPanel.vue'
import SessionMemoryPanel from './components/panels/SessionMemoryPanel.vue'
import FilePreview from './components/FilePreview.vue'
import MetricsCard from './components/MetricsCard.vue'
import TokenMetricsCard from './components/TokenMetricsCard.vue'
import MailboxCard from './components/MailboxCard.vue'
import HealthCard from './components/HealthCard.vue'

const route = useRoute()
const router = useRouter()

const { sessions, loadSessions } = useSessionList()
const stream = useSessionStream()
const panel = usePanelRefresh()
const { statusDotClass, statusText } = useSessionStatus()
const { workDir, setWorkDir } = useWorkDir()

const activeSession = ref<Session | null>(null)
const events = ref<SessionEvent[]>([])
const agents = ref<AgentNode[]>([])
const metrics = ref<SessionMetrics | null>(null)
const tokenMetrics = ref<SessionTokenMetricsResponse | null>(null)
const mailboxMessages = ref<MailboxMessage[]>([])
const health = ref<HealthResponse | null>(null)
const board = ref<TaskBoardData | null>(null)

// 结构化会话日志（监控视图「日志分析」Tab）
const sessionLogs = ref<SessionLog[]>([])
const logFilterAgent = ref('')
const logFilterLevel = ref('')
const logLimit = ref(100)
const expandedLogId = ref<number | null>(null)

// 待澄清选项区：由 SSE awaiting_clarify 帧驱动（不 push 进 events），答复后复位
const clarifyPending = ref<{ options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null>(null)

const loading = ref(false)
const sending = ref(false)
const sessionFilter = ref('')

// 视图切换：?view=chat|monitor，缺省 chat；chat 为缺省视图不污染 URL
const view = ref<'chat' | 'monitor'>((route.query.view as string) === 'monitor' ? 'monitor' : 'chat')
watch(view, (v) => {
  router.replace({ query: { ...route.query, view: v === 'chat' ? undefined : v } })
})

// 右栏统一 5 Tab
const rightTab = ref<'board' | 'tools' | 'files' | 'memory' | 'metrics'>('board')

onMounted(async () => {
  // 工作目录页「发起新会话」跳入：?work_dir= 预填并固化（与提交时 setWorkDir 同策略）
  const qwd = route.query.work_dir as string
  if (qwd) setWorkDir(qwd)

  await loadSessions()
  const id = route.query.id as string
  if (id) {
    await openSession(id)
  } else {
    // 无 URL id 时优先恢复上次活跃会话（localStorage），否则打开最近一个
    const last = localStorage.getItem('lastSessionID')
    if (last && sessions.value.some((s) => s.id === last)) {
      await openSession(last)
    } else if (sessions.value.length) {
      await openSession(sessions.value[0].id)
    }
  }
})

watch(() => route.query.id, (id) => {
  if (id && typeof id === 'string' && id !== activeSession.value?.id) {
    openSession(id)
  }
})

async function openSession(id: string) {
  loading.value = true
  stream.close()
  panel.stopPanelTimer()
  panel.invalidate()
  events.value = []
  clarifyPending.value = null // 切换会话时复位待澄清选项，避免串会话残留
  try {
    const s = await getSession(id)
    activeSession.value = s
    events.value = [...(s.events || [])]
    localStorage.setItem('lastSessionID', id)
    startStream(s)
    await refreshPanels(id)
  } catch (e) {
    localStorage.removeItem('lastSessionID')
    activeSession.value = null
    ElMessage.error('会话不存在或加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function refreshPanels(id: string) {
  const [agentsRes, boardRes, metricsRes, mbRes, healthRes] = await Promise.allSettled([
    panel.run(() => getSessionAgents(id)),
    panel.run(() => getSessionBoard(id)),
    panel.run(() => getSessionMetrics(id)),
    panel.run(() => getSessionMailbox(id)),
    panel.run(() => getHealth()),
  ])
  agents.value = agentsRes.status === 'fulfilled' && agentsRes.value ? agentsRes.value.agents || [] : []
  board.value = boardRes.status === 'fulfilled' && boardRes.value ? boardRes.value.board || null : null
  metrics.value = metricsRes.status === 'fulfilled' && metricsRes.value ? metricsRes.value : null
  mailboxMessages.value = mbRes.status === 'fulfilled' && mbRes.value ? mbRes.value.messages || [] : []
  health.value = healthRes.status === 'fulfilled' && healthRes.value ? healthRes.value : null
  await loadSessionLogs(id)
}

async function loadSessionLogs(sessionID: string) {
  const logsRes = await panel.run(() =>
    getSessionLogs(sessionID, {
      agent: logFilterAgent.value || undefined,
      level: logFilterLevel.value || undefined,
      limit: logLimit.value,
    })
  )
  sessionLogs.value = logsRes?.logs || []
  const tokenRes = await panel.run(() => getSessionTokenMetrics(sessionID))
  tokenMetrics.value = tokenRes || null
}

function applyLogFilters() {
  if (activeSession.value) loadSessionLogs(activeSession.value.id)
}

function startStream(s: Session) {
  panel.startAutoRefresh(s.id, refreshPanels)
  stream.startStream(s.id, {
    onSnapshot(snap) {
      activeSession.value = snap
      events.value = [...(snap.events || [])]
      if (snap.status !== 'awaiting_clarify' && clarifyPending.value) {
        clarifyPending.value = null
      }
    },
    onEvent(ev) {
      if ((ev as any).type === 'awaiting_clarify') {
        clarifyPending.value = {
          options: (ev as any).options || [],
          multiSelect: !!(ev as any).multi_select,
          questionId: (ev as any).question_id || '',
        }
        return
      }
      events.value.push(ev)
    },
    onDone(finalStatus?: string) {
      panel.stopPanelTimer()
      refreshPanels(s.id)
      loadSessions()
      if (activeSession.value) {
        const status = (finalStatus as Session['status']) || 'completed'
        activeSession.value = { ...activeSession.value, status }
      }
      sending.value = false
    },
    onError(err) {
      panel.stopPanelTimer()
      console.error('SSE error:', err)
      sending.value = false
      ElMessage.error('实时连接异常，请检查网络或刷新页面')
    },
  })
}

async function handleSubmit(content: string, images: WireImage[] = []) {
  if (!content.trim() && !images.length) return
  sending.value = true
  try {
    // 1) 待澄清会话 → 调 /clarify 提交答复
    if (activeSession.value && activeSession.value.status === 'awaiting_clarify') {
      if (images.length) ElMessage.warning('澄清答复不支持携带图片，已忽略')
      await clarifySession(activeSession.value.id, content)
      activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
      await openSession(activeSession.value.id)
      return
    }
    // 2) 已有会话 → 追加消息 / 入队 / 续跑
    if (activeSession.value) {
      const wasRunning = activeSession.value.status === 'running'
      if (wasRunning && !activeSession.value.destroy_at) {
        if (images.length) ElMessage.warning('运行中入队不支持携带图片，已忽略')
        await enqueueSession(activeSession.value.id, content)
        sending.value = false
        return
      }
      await sendMessage(activeSession.value.id, content, images)
      if (!wasRunning) {
        activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
        await openSession(activeSession.value.id)
      }
      return
    }
    // 3) 无选中会话 → 创建新会话
    const s = await createSession(content, images, workDir.value || undefined)
    setWorkDir(workDir.value)
    sessions.value.unshift(s)
    router.replace({ path: '/session', query: { id: s.id } })
    await openSession(s.id)
  } catch (e) {
    console.error('submit failed:', e)
    sending.value = false
    ElMessage.error('发送失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleClarifySubmitted() {
  if (!activeSession.value) return
  activeSession.value = { ...activeSession.value, status: 'running' as Session['status'] }
  await openSession(activeSession.value.id)
}

async function handleCancel() {
  if (!activeSession.value) return
  try {
    await ElMessageBox.confirm(
      '硬终止会立即取消全部子 Agent 与当前任务，且不可续跑。建议优先使用软停止。',
      '确认硬终止？',
      { confirmButtonText: '硬终止', cancelButtonText: '取消', type: 'warning' }
    )
  } catch {
    return
  }
  try {
    await cancelSession(activeSession.value.id)
  } catch (e) {
    ElMessage.error('取消会话失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleInterrupt() {
  if (!activeSession.value) return
  try {
    const { value } = await ElMessageBox.prompt(
      '将打断当前执行，并把输入内容作为新指令注入继续运行',
      '抢占中断',
      { confirmButtonText: '中断', cancelButtonText: '取消', inputPlaceholder: '新指令（必填）', type: 'warning' }
    )
    if (!value || !value.trim()) return
    await interruptSession(activeSession.value.id, value.trim())
    ElMessage.success('已中断并注入新指令')
  } catch (e) {
    if (e === 'cancel' || (e instanceof Error && e.message === 'cancel')) return
    ElMessage.error('中断失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleStop() {
  if (!activeSession.value) return
  try {
    await stopSession(activeSession.value.id)
    ElMessage.success('已软停止，倒计时内发送新消息可续跑')
    const s = await getSession(activeSession.value.id)
    activeSession.value = s
  } catch (e) {
    ElMessage.error('软停止失败：' + (e instanceof Error ? e.message : String(e)))
  }
}

async function handleNewSession() {
  stream.close()
  panel.stopPanelTimer()
  events.value = []
  activeSession.value = null
  agents.value = []
  metrics.value = null
  tokenMetrics.value = null
  board.value = null
  clarifyPending.value = null
  router.replace({ path: '/session' })
}

const filteredSessions = computed(() => {
  const q = sessionFilter.value.trim().toLowerCase()
  if (!q) return sessions.value
  return sessions.value.filter(s =>
    s.id.toLowerCase().includes(q) ||
    (s.goal || '').toLowerCase().includes(q))
})

const tokenUsage = computed(() => {
  let input = 0
  let output = 0
  for (const ev of events.value) {
    if (ev.kind !== 'token_usage') continue
    input += ev.input_tokens || 0
    output += ev.output_tokens || 0
  }
  return { input, output }
})

function fmtDateTime(iso: string) {
  if (!iso) return ''
  return new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}
</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-ink">
    <!-- 左侧会话列表 -->
    <aside class="w-[280px] flex flex-col shrink-0 bg-card border border-line rounded-card overflow-hidden">
      <div class="px-3 py-3 border-b border-line flex items-center justify-between gap-2">
        <span class="text-sm font-bold text-ink">会话列表</span>
        <el-button size="small" plain @click="handleNewSession">
          <el-icon><Plus /></el-icon>
        </el-button>
      </div>
      <div class="px-3 py-2 border-b border-line">
        <el-input v-model="sessionFilter" size="small" placeholder="搜索会话…" class="chat-search">
          <template #prefix><el-icon class="text-ink-3"><Search /></el-icon></template>
        </el-input>
      </div>

      <div class="flex-1 overflow-y-auto">
        <button v-for="s in filteredSessions" :key="s.id"
                @click="openSession(s.id)"
                class="w-full text-left px-3 py-2.5 border-b border-line hover:bg-page transition-colors"
                :class="{ 'bg-primary-soft': activeSession?.id === s.id }">
          <div class="flex items-center gap-2 mb-1">
            <span class="w-1.5 h-1.5 rounded-full shrink-0" :class="statusDotClass(s.status)"></span>
            <span class="text-xs text-ink-3 font-mono shrink-0">{{ s.id }}</span>
            <span class="text-[10px] text-ink-3 ml-auto shrink-0">{{ statusText(s.status) }}</span>
          </div>
          <div class="text-sm text-ink line-clamp-2 break-words">{{ s.goal || '(无目标)' }}</div>
          <div class="text-[10px] text-ink-3 mt-1">{{ fmtDateTime(s.started_at) }}</div>
        </button>

        <div v-if="!filteredSessions.length" class="text-center text-xs text-ink-3 py-10">
          暂无会话，在下方输入框直接下达命令即可创建。
        </div>
      </div>
    </aside>

    <!-- 中栏：标题 + 视图切换 + 双视图 -->
    <main class="flex-1 flex flex-col min-w-0 gap-3">
      <div class="h-12 bg-card border border-line rounded-card flex items-center px-4 gap-3 shrink-0">
        <span class="font-bold text-sm text-ink truncate">{{ activeSession?.goal || '新会话' }}</span>
        <span v-if="activeSession" class="text-xs text-ink-3 font-mono shrink-0">{{ activeSession.id }}</span>
        <div class="ml-auto flex bg-page border border-line rounded-lg p-0.5 shrink-0">
          <button class="px-3 py-1 text-xs rounded-md transition-colors"
                  :class="view === 'chat' ? 'bg-card text-primary font-bold shadow-sm' : 'text-ink-2 hover:text-ink'"
                  @click="view = 'chat'">💬 对话</button>
          <button class="px-3 py-1 text-xs rounded-md transition-colors"
                  :class="view === 'monitor' ? 'bg-card text-primary font-bold shadow-sm' : 'text-ink-2 hover:text-ink'"
                  @click="view = 'monitor'">📊 监控</button>
        </div>
      </div>

      <ChatView
        v-if="view === 'chat'"
        :session="activeSession"
        :events="events"
        :agents="agents"
        :clarify="clarifyPending"
        :sending="sending"
        :input-tokens="tokenUsage.input"
        :output-tokens="tokenUsage.output"
        @submit="handleSubmit"
        @cancel="handleCancel"
        @stop="handleStop"
        @interrupt="handleInterrupt"
        @new-session="handleNewSession"
        @clarify-submitted="handleClarifySubmitted"
      />
      <MonitorView
        v-else
        :session="activeSession"
        :events="events"
        :agents="agents"
        :session-logs="sessionLogs"
        v-model:log-agent="logFilterAgent"
        v-model:log-level="logFilterLevel"
        v-model:expanded-log-id="expandedLogId"
        @query-logs="applyLogFilters"
      />
    </main>

    <!-- 右栏：统一 5 Tab -->
    <aside class="w-[320px] shrink-0 bg-card border border-line rounded-card overflow-hidden flex flex-col">
      <el-tabs v-model="rightTab" class="session-right-tabs flex-1 flex flex-col min-h-0">
        <el-tab-pane label="任务看板" name="board" class="flex-1 overflow-y-auto p-3">
          <TaskBoardPanel :agents="agents" :board="board" />
        </el-tab-pane>
        <el-tab-pane label="工具" name="tools" class="flex-1 overflow-y-auto p-3">
          <ToolPanel :events="events" />
        </el-tab-pane>
        <el-tab-pane label="文件" name="files" class="flex-1 overflow-hidden p-0">
          <FilePreview :session-id="activeSession?.id || ''" :agents="agents" />
        </el-tab-pane>
        <el-tab-pane label="记忆" name="memory" class="flex-1 overflow-y-auto p-3">
          <SessionMemoryPanel :session-id="activeSession?.id || ''" />
        </el-tab-pane>
        <el-tab-pane label="指标" name="metrics" class="flex-1 overflow-y-auto p-3">
          <div class="space-y-3">
            <MetricsCard :metrics="metrics" />
            <TokenMetricsCard :token-metrics="tokenMetrics" />
            <MailboxCard :messages="mailboxMessages" />
            <HealthCard :health="health" />
          </div>
        </el-tab-pane>
      </el-tabs>
    </aside>
  </div>
</template>

<style scoped>
:deep(.chat-search .el-input__wrapper) {
  background-color: var(--bma-page);
  box-shadow: 0 0 0 1px var(--bma-border) inset;
}
:deep(.chat-search .el-input__wrapper.is-focus) {
  box-shadow: 0 0 0 1px var(--bma-primary) inset;
}

.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

/* 右栏 Tab：标签栏固定在卡片顶部，内容区自适应滚动 */
.session-right-tabs :deep(.el-tabs__header) {
  margin: 0;
  padding: 0 12px;
  border-bottom: 1px solid var(--bma-border);
}
.session-right-tabs :deep(.el-tabs__content) {
  flex: 1;
  min-height: 0;
  display: flex;
  flex-direction: column;
}
.session-right-tabs :deep(.el-tab-pane) {
  height: 100%;
}
</style>
```

- [ ] **Step 5.3: 验证（构建允许暂时失败项除外）**

本任务的容器引用了 Task 6/7/8 才创建的 4 个组件（`ChatView`、`MonitorView`、`panels/*`）。**执行顺序调整：Step 5.1 → Task 6 → Task 7 → Task 8 → 回本 Step 跑构建**：

```bash
cd web && npm run build
```

预期：通过；`/session` 打开默认对话视图，`?view=monitor` 切监控；旧 `/chat?id=xxx` 自动跳转。

---

## Task 6: 会话页对话视图（ChatView + chat 组件换 token）

**Depends on**: Task 5 Step 5.1（文件已搬迁）
**Files**:
- Create: `web/src/views/session/chat/ChatView.vue`
- Modify: `web/src/views/session/chat/components/*.vue`（7 个，仅换 token）

**Interfaces**:
- Consumes: `ChatHeader`（props `session/agents`，emits `cancel/stop/interrupt`）、`MessageList`（props `events/verbose/clarify/session-id`，emits `submit-clarify`）、`ChatInput`（props `loading/session-active/input-tokens/output-tokens`，emits `submit(content, images)`、`new-session`）——签名均已核实，保持不变
- Produces: `ChatView`（props `session/events/agents/clarify/sending/inputTokens/outputTokens`；emits `submit/cancel/stop/interrupt/new-session/clarify-submitted`）

- [ ] **Step 6.1: 新建 `web/src/views/session/chat/ChatView.vue`**

```vue
<script setup lang="ts">
import type { Session, SessionEvent, AgentNode, ClarifyOption, WireImage } from '@/types'
import ChatHeader from './components/ChatHeader.vue'
import MessageList from './components/MessageList.vue'
import ChatInput from './components/ChatInput.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  clarify: { options: ClarifyOption[]; multiSelect: boolean; questionId: string } | null
  sending: boolean
  inputTokens: number
  outputTokens: number
}>()

const emit = defineEmits<{
  submit: [content: string, images: WireImage[]]
  cancel: []
  stop: []
  interrupt: []
  'new-session': []
  'clarify-submitted': []
}>()
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <ChatHeader :session="session" :agents="agents"
                @cancel="emit('cancel')" @stop="emit('stop')" @interrupt="emit('interrupt')" />
    <MessageList :events="events" :verbose="false" :clarify="clarify"
                 :session-id="session?.id || ''" @submit-clarify="emit('clarify-submitted')" />
    <ChatInput :loading="sending"
               :session-active="session?.status === 'running'"
               :input-tokens="inputTokens"
               :output-tokens="outputTokens"
               @submit="(c: string, i: WireImage[]) => emit('submit', c, i)"
               @new-session="emit('new-session')" />
  </div>
</template>
```

- [ ] **Step 6.2: chat 组件换 token**

对 `web/src/views/session/chat/components/` 下 7 个文件（`AssistantTurn/ChatHeader/ChatInput/MessageList/ThinkChain/ToolCallCard/UserBubble`）应用 Global Constraints §3 映射表。补充定点：

- `ChatHeader.vue`：根节点 `border-b border-[#2a2d35] bg-[#14161a]` → `border-b border-line bg-card`；软停止按钮 `!bg-orange-900/30 !border-orange-700/40 !text-orange-400 hover:!bg-orange-800/50` → `!bg-amber-50 !border-amber-300 !text-amber-700 dark:!bg-orange-900/30 dark:!border-orange-700/40 dark:!text-orange-400`；终止按钮同法用 `red`/`dark:red-900` 系；中断按钮 `!bg-transparent !border-[#2a2d35] !text-gray-400 hover:!text-white` → `!bg-transparent !border-line !text-ink-2 hover:!text-primary`。
- 各文件 scoped style 里的硬编码 hex（`#0f1115` 等）同步替换为对应 `var(--bma-*)`。

**禁止改动任何 props/emits/逻辑代码**——本步纯样式。

---

## Task 7: 会话页监控视图（MonitorView）

**Depends on**: Task 5 Step 5.1
**Files**:
- Create: `web/src/views/session/monitor/MonitorView.vue`
- Modify: `web/src/views/session/components/*.vue`（8 个，仅换 token）

**Interfaces**:
- Consumes: `ExecutionLog`（props `events`）、`SkillSet`（props `agents`）、`SessionLogsPanel`（models `agent/level/expandedLogId`，props `logs`，emits `query`）——均沿用现状
- Produces: `MonitorView`（props `session/events/agents/sessionLogs`；models `logAgent/logLevel/expandedLogId`；emits `query-logs`）
- 说明：原监控中栏的「文件预览」Tab 移除（已由右栏统一「文件」Tab 承担），监控中栏保留 执行日志 / Skill 装配 / 日志分析 三个。

- [ ] **Step 7.1: 新建 `web/src/views/session/monitor/MonitorView.vue`**

```vue
<script setup lang="ts">
import { ref } from 'vue'
import type { Session, SessionEvent, AgentNode } from '@/types'
import type { SessionLog } from '@/api/session'
import ExecutionLog from '../components/ExecutionLog.vue'
import SkillSet from '../components/SkillSet.vue'
import SessionLogsPanel from '../components/SessionLogsPanel.vue'

defineProps<{
  session: Session | null
  events: SessionEvent[]
  agents: AgentNode[]
  sessionLogs: SessionLog[]
}>()

const logAgent = defineModel<string>('logAgent', { default: '' })
const logLevel = defineModel<string>('logLevel', { default: '' })
const expandedLogId = defineModel<number | null>('expandedLogId', { default: null })

const emit = defineEmits<{ 'query-logs': [] }>()

const activeTab = ref<'log' | 'skill' | 'logs'>('log')
</script>

<template>
  <div class="flex-1 flex flex-col bg-card border border-line rounded-card overflow-hidden min-w-0 min-h-0">
    <div class="h-12 border-b border-line flex items-center px-4 gap-6 text-sm shrink-0">
      <span @click="activeTab = 'log'"
            :class="activeTab === 'log' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Document /></el-icon> 执行日志
      </span>
      <span @click="activeTab = 'skill'"
            :class="activeTab === 'skill' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Connection /></el-icon> Skill 装配
      </span>
      <span @click="activeTab = 'logs'"
            :class="activeTab === 'logs' ? 'text-primary font-bold border-b-2 border-primary' : 'text-ink-2 hover:text-ink'"
            class="flex items-center h-full cursor-pointer">
        <el-icon class="mr-1"><Tickets /></el-icon> 日志分析
      </span>
    </div>

    <div class="flex-1 overflow-hidden relative flex flex-col">
      <ExecutionLog v-if="activeTab === 'log'" :events="events" />
      <SkillSet v-if="activeTab === 'skill'" :agents="agents" />
      <SessionLogsPanel
        v-if="activeTab === 'logs'"
        :agent="logAgent"
        @update:agent="(v: string) => (logAgent = v)"
        :level="logLevel"
        @update:level="(v: string) => (logLevel = v)"
        :expanded-log-id="expandedLogId"
        @update:expanded-log-id="(v: number | null) => (expandedLogId = v)"
        :logs="sessionLogs"
        @query="emit('query-logs')"
      />
    </div>
  </div>
</template>
```

若 `SessionLogsPanel` 的 model 修饰符名与上述 `:agent/@update:agent` 桥接不匹配（如它声明的是 `modelValue` 以外的自定义名），以其组件内 `defineModel`/`defineEmits` 实际声明为准调整桥接，语义不变。

- [ ] **Step 7.2: 监控组件换 token**

对 `web/src/views/session/components/` 下 8 个文件（`ExecutionLog/FilePreview/HealthCard/MailboxCard/MetricsCard/SessionLogsPanel/SkillSet/TokenMetricsCard`）应用 Global Constraints §3 映射表；scoped style 内硬编码 hex 同步换 `var(--bma-*)`。纯样式，不动逻辑。

---

## Task 8: 会话页右栏面板（TaskBoardPanel / ToolPanel / SessionMemoryPanel）

**Depends on**: Task 5 Step 5.1
**Files**:
- Create: `web/src/views/session/components/panels/TaskBoardPanel.vue`
- Create: `web/src/views/session/components/panels/ToolPanel.vue`
- Create: `web/src/views/session/components/panels/SessionMemoryPanel.vue`

**Interfaces**:
- `TaskBoardPanel`: props `{ agents: AgentNode[]; board: TaskBoardData | null }`；内部 `useRoleTree/useTaskBoard`（接受 `MaybeRef`，用 `toRef(props, ...)` 喂入）
- `ToolPanel`: props `{ events: SessionEvent[] }`
- `SessionMemoryPanel`: props `{ sessionId: string }`；Consumes `listEvolutionLog/listLearnedSkills`

- [ ] **Step 8.1: 新建 `TaskBoardPanel.vue`**

```vue
<script setup lang="ts">
import { toRef } from 'vue'
import type { AgentNode, TaskBoardData } from '@/types'
import { useRoleTree } from '@/composables/useRoleTree'
import { useTaskBoard } from '@/composables/useTaskBoard'

const props = defineProps<{
  agents: AgentNode[]
  board: TaskBoardData | null
}>()

const { roleTree, defaultProps } = useRoleTree(toRef(props, 'agents'))
const { tasks, constraints, taskProgress } = useTaskBoard(toRef(props, 'board'))
</script>

<template>
  <div class="space-y-4 text-xs">
    <!-- 角色层级 -->
    <div>
      <div class="font-bold text-sm text-ink mb-2">角色层级</div>
      <div v-if="!roleTree.length" class="text-ink-3 py-3 text-center">暂无角色实例，会话启动后自动创建</div>
      <el-tree v-else :data="roleTree" :props="defaultProps" default-expand-all
               class="!bg-transparent custom-tree" :expand-on-click-node="false">
        <template #default="{ node, data }">
          <div class="flex items-center justify-between w-full pr-1 py-0.5">
            <span class="flex items-center gap-1.5">
              <el-icon :class="data.iconColor" class="text-sm">
                <UserFilled v-if="data.isUser" /><User v-else />
              </el-icon>
              <span :class="data.active ? 'text-ink' : 'text-ink-3'" class="text-xs">{{ node.label }}</span>
            </span>
            <el-tag v-if="data.status" :type="data.statusType" size="small" effect="plain"
                    class="scale-75 origin-right">
              {{ data.status === 'delivered-unverified' ? '已交付未验证' : data.status }}
            </el-tag>
          </div>
        </template>
      </el-tree>
    </div>

    <!-- 任务看板 -->
    <div>
      <div class="flex justify-between items-center mb-2">
        <span class="font-bold text-sm text-ink">任务看板</span>
        <el-progress v-if="tasks.length" :percentage="taskProgress" :show-text="false" class="w-20 custom-progress" />
      </div>
      <div v-if="!tasks.length" class="text-ink-3 py-3 text-center">暂无子任务，等待 DomainAgent 拆解</div>
      <div v-else class="space-y-1.5">
        <div v-for="(t, i) in tasks" :key="i"
             class="flex items-center gap-2 p-1.5 bg-page rounded border border-line">
          <el-icon v-if="t.status === 'done'" class="text-green-500 text-sm"><CircleCheck /></el-icon>
          <el-icon v-else-if="t.status === 'running' || t.status === 'in_progress'" class="text-yellow-500 text-sm"><Loading /></el-icon>
          <el-icon v-else-if="t.status === 'delivered-unverified'" class="text-yellow-500 text-sm"><WarningFilled /></el-icon>
          <el-icon v-else-if="t.status === 'failed'" class="text-red-500 text-sm"><CircleClose /></el-icon>
          <el-icon v-else-if="t.status === 'blocked'" class="text-ink-2 text-sm"><Lock /></el-icon>
          <el-icon v-else class="text-ink-3 text-sm"><CirclePlus /></el-icon>
          <span class="text-ink truncate flex-1">{{ t.title || t.name }}</span>
          <span v-if="t.assignee" class="text-[10px] text-ink-3 shrink-0 font-mono">{{ t.assignee }}</span>
        </div>
      </div>
    </div>

    <!-- 约束条件 -->
    <div v-if="constraints.length">
      <div class="font-bold text-sm text-ink mb-2">约束条件</div>
      <div class="space-y-1.5">
        <div v-for="(c, i) in constraints" :key="i" class="flex justify-between p-2 bg-page rounded border border-line">
          <span class="text-ink-3">{{ c[0] }}</span>
          <span class="text-ink">{{ c[1] }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
:deep(.custom-tree .el-tree-node__content) {
  background-color: transparent !important;
  height: 28px;
}
:deep(.custom-tree .el-tree-node__content:hover) {
  background-color: var(--bma-primary-soft) !important;
}
:deep(.custom-progress .el-progress-bar__outer) {
  background-color: var(--bma-border);
}
:deep(.custom-progress .el-progress-bar__inner) {
  background-color: var(--bma-primary);
}
</style>
```

- [ ] **Step 8.2: 新建 `ToolPanel.vue`**

```vue
<script setup lang="ts">
import { computed } from 'vue'
import type { SessionEvent } from '@/types'
import { isToolCallEvent, isToolExecEvent } from '@/types'

const props = defineProps<{ events: SessionEvent[] }>()

interface ToolRow {
  key: number
  tool: string
  path: string
  args: string
  output: string
  error: string
  success: boolean | null // null = 尚无结果帧
  time: string
  agent: string
}

function fmtTime(iso?: string) {
  return iso ? new Date(iso).toLocaleTimeString('zh-CN', { hour12: false }) : ''
}

const rows = computed<ToolRow[]>(() => {
  const calls = props.events.filter(isToolCallEvent)
  return calls.map((ev, i) => {
    // 结果配对：本次调用之后、下一次任意 tool_call 之前的首个同名 tool_exec 帧
    const next = calls[i + 1]
    const res = props.events.find((e) =>
      isToolExecEvent(e) &&
      e.tool === ev.tool &&
      e.timestamp >= ev.timestamp &&
      (!next || e.timestamp < next.timestamp)
    )
    return {
      key: i,
      tool: ev.tool || 'unknown',
      path: ev.tool_path || res?.tool_path || '',
      args: ev.tool_args || '',
      output: res?.tool_output || ev.tool_output || '',
      error: res?.tool_error || ev.tool_error || '',
      success: res ? res.success ?? !res.tool_error : (ev.success ?? null),
      time: fmtTime(ev.timestamp),
      agent: ev.agent || '',
    }
  })
})
</script>

<template>
  <div class="text-xs">
    <div v-if="!rows.length" class="text-ink-3 text-center py-8">暂无工具调用</div>
    <el-collapse v-else class="tool-panel">
      <el-collapse-item v-for="r in rows" :key="r.key">
        <template #title>
          <div class="flex items-center gap-2 w-full pr-2 min-w-0">
            <el-icon v-if="r.success === true" class="text-green-500 shrink-0"><CircleCheck /></el-icon>
            <el-icon v-else-if="r.success === false" class="text-red-500 shrink-0"><CircleClose /></el-icon>
            <el-icon v-else class="text-ink-3 shrink-0"><Clock /></el-icon>
            <span class="font-mono font-bold text-ink shrink-0">{{ r.tool }}</span>
            <span class="font-mono text-ink-3 truncate">{{ r.path }}</span>
            <span class="text-ink-3 ml-auto shrink-0">{{ r.time }}</span>
          </div>
        </template>
        <div class="space-y-2 pb-2">
          <div v-if="r.agent" class="text-ink-3">调用方：<span class="text-ink-2">{{ r.agent }}</span></div>
          <div v-if="r.args">
            <div class="text-ink-3 mb-1">参数</div>
            <pre class="bg-page border border-line rounded p-2 overflow-x-auto text-ink-2 whitespace-pre-wrap break-all">{{ r.args }}</pre>
          </div>
          <div v-if="r.output">
            <div class="text-ink-3 mb-1">输出</div>
            <pre class="bg-page border border-line rounded p-2 overflow-x-auto text-ink-2 whitespace-pre-wrap break-all max-h-64 overflow-y-auto">{{ r.output }}</pre>
          </div>
          <div v-if="r.error" class="text-red-500 break-all">错误：{{ r.error }}</div>
        </div>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<style scoped>
.tool-panel :deep(.el-collapse-item__header) {
  background-color: transparent;
  border-bottom: 1px solid var(--bma-border);
  height: 36px;
}
.tool-panel :deep(.el-collapse-item__wrap) {
  background-color: transparent;
  border-bottom: 1px solid var(--bma-border);
}
.tool-panel {
  border-top: none;
  border-bottom: none;
}
</style>
```

- [ ] **Step 8.3: 新建 `SessionMemoryPanel.vue`**

```vue
<script setup lang="ts">
import { ref, watch } from 'vue'
import {
  listEvolutionLog,
  listLearnedSkills,
  type EvolutionLogEntry,
  type LearnedSkill,
} from '@/api/learned'

const props = defineProps<{ sessionId: string }>()

const entries = ref<EvolutionLogEntry[]>([])
const skills = ref<LearnedSkill[]>([])
const loading = ref(false)

// 本会话沉淀：evolution_log 与 learned_skills 均带 source_session 字段，前端按之过滤。
async function load() {
  if (!props.sessionId) {
    entries.value = []
    skills.value = []
    return
  }
  loading.value = true
  try {
    const [logRes, skillRes] = await Promise.all([listEvolutionLog(200), listLearnedSkills()])
    entries.value = (logRes.entries || []).filter((e) => e.source_session === props.sessionId)
    skills.value = (skillRes.skills || []).filter((s) => s.source_session === props.sessionId)
  } catch {
    entries.value = []
    skills.value = []
  } finally {
    loading.value = false
  }
}

watch(() => props.sessionId, load, { immediate: true })

function kindLabel(kind: string) {
  switch (kind) {
    case 'user_pref': return '用户偏好'
    case 'project_lesson': return '项目经验'
    case 'skill_create': return '技能新建'
    case 'skill_update': return '技能更新'
    default: return kind
  }
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}
</script>

<template>
  <div v-loading="loading" class="space-y-4 text-xs">
    <div>
      <div class="font-bold text-sm text-ink mb-2">沉淀技能</div>
      <div v-if="!skills.length" class="text-ink-3 py-2">本会话未沉淀技能</div>
      <div v-for="s in skills" :key="s.name" class="p-2 bg-page rounded border border-line mb-1.5">
        <div class="flex items-center gap-2">
          <span class="font-bold text-ink">{{ s.title }}</span>
          <el-tag size="small" effect="plain" :type="s.outcome === 'success' ? 'success' : 'danger'">
            源自{{ s.outcome === 'success' ? '成功' : '失败' }}会话
          </el-tag>
        </div>
        <p class="text-ink-2 mt-1 line-clamp-2">{{ s.when_to_use }}</p>
      </div>
    </div>

    <div>
      <div class="font-bold text-sm text-ink mb-2">沉淀记录</div>
      <div v-if="!entries.length" class="text-ink-3 py-2">本会话暂无沉淀记录（会话结束后自动沉淀）</div>
      <div v-for="e in entries" :key="e.id" class="p-2 bg-page rounded border border-line mb-1.5">
        <div class="flex items-center gap-2">
          <el-tag size="small" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
          <span class="font-bold text-ink">{{ e.target }}</span>
          <span class="text-ink-3 ml-auto">{{ fmtTime(e.created_at) }}</span>
        </div>
        <p class="text-ink-2 mt-1">{{ e.summary }}</p>
      </div>
    </div>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
```

- [ ] **Step 8.4: 验证（回到 Task 5 Step 5.3 跑构建）**

对照 `doc/image/redesign/02-session.png`（对话 + 右栏 5 Tab）、`03-monitor.png`（监控）、`11-session-tab-tools.png`、`13-session-tab-memory.png`、`14-session-tab-metrics.png`、`15-monitor-logs.png` 逐张核对结构。

---

## Task 9: 工作目录页 `/projects`（吞并项目偏好，对照 04-projects.png）

**Depends on**: Task 3
**Files**:
- Create: `web/src/views/projects/index.vue`（覆盖 Task 3 的临时占位）
- Modify: `web/src/api/preferences.ts`（仅补类型导出，若无）

**Interfaces**:
- Consumes: `listSessions()`、`getProjectPreferences(workDir?)`、`saveProjectPreferences(content, workDir?)`、`useWorkDir`、`WorkDirPicker`
- Produces: `/projects` 页；`?work_dir=` 跳出到 `/session`；localStorage `bma:workdirs`（手工添加的目录清单）

- [ ] **Step 9.1: 读 `web/src/api/preferences.ts` 核对两个函数签名**（应为 `getProjectPreferences(workDir?: string): Promise<{ content: string; path: string }>` 与 `saveProjectPreferences(content: string, workDir?: string): Promise<...>`），不一致以实际为准调整下方调用。

- [ ] **Step 9.2: 新建 `web/src/views/projects/index.vue`**

```vue
<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'
import { getProjectPreferences, saveProjectPreferences } from '@/api/preferences'
import { useWorkDir } from '@/composables/useWorkDir'
import WorkDirPicker from '@/components/WorkDirPicker.vue'

const DEFAULT_DIR = '' // 空 work_dir = 后端默认目录
const DIRS_KEY = 'bma:workdirs'

const router = useRouter()
const { setWorkDir } = useWorkDir()

const sessions = ref<Session[]>([])
const loading = ref(false)

// 手工添加的目录（无会话也能出现在列表）
const customDirs = ref<string[]>(JSON.parse(localStorage.getItem(DIRS_KEY) || '[]'))
const newDir = ref('')

const selected = ref<string>(DEFAULT_DIR)

// 项目偏好编辑器状态
const prefContent = ref('')
const prefPath = ref('')
const prefLoading = ref(false)
const prefSaving = ref(false)
const prefDirty = ref(false)

interface DirGroup {
  dir: string
  total: number
  running: number
  lastActive: string
}

const groups = computed<DirGroup[]>(() => {
  const map = new Map<string, DirGroup>()
  for (const s of sessions.value) {
    const dir = s.work_dir || DEFAULT_DIR
    const g = map.get(dir) || { dir, total: 0, running: 0, lastActive: '' }
    g.total++
    if (s.status === 'running') g.running++
    if (s.started_at && s.started_at > g.lastActive) g.lastActive = s.started_at
    map.set(dir, g)
  }
  for (const d of customDirs.value) {
    if (!map.has(d)) map.set(d, { dir: d, total: 0, running: 0, lastActive: '' })
  }
  return [...map.values()].sort((a, b) => b.lastActive.localeCompare(a.lastActive))
})

function dirLabel(dir: string) {
  return dir || '默认目录'
}

async function load() {
  loading.value = true
  try {
    sessions.value = await listSessions()
  } catch (e) {
    ElMessage.error('会话列表加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

function addDir() {
  const d = newDir.value.trim()
  if (!d) return
  if (!customDirs.value.includes(d)) {
    customDirs.value = [...customDirs.value, d]
    localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
  }
  selected.value = d
  newDir.value = ''
}

function removeCustomDir(dir: string) {
  customDirs.value = customDirs.value.filter((d) => d !== dir)
  localStorage.setItem(DIRS_KEY, JSON.stringify(customDirs.value))
  if (selected.value === dir) selected.value = DEFAULT_DIR
}

// 发起新会话：固化工作目录并跳入会话页（容器读取 ?work_dir= 预填）
function startSession(dir: string) {
  setWorkDir(dir)
  router.push({ path: '/session', query: dir ? { work_dir: dir } : {} })
}

function viewSessions(dir: string) {
  router.push({ path: '/history', query: dir ? { work_dir: dir } : {} })
}

async function loadPrefs() {
  prefLoading.value = true
  try {
    const res = await getProjectPreferences(selected.value || undefined)
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
    await saveProjectPreferences(prefContent.value, selected.value || undefined)
    prefDirty.value = false
    ElMessage.success('项目偏好已保存')
  } catch (e) {
    ElMessage.error('保存失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    prefSaving.value = false
  }
}

watch(selected, loadPrefs)
onMounted(async () => {
  await load()
  if (groups.value.length && !groups.value.some((g) => g.dir === selected.value)) {
    selected.value = groups.value[0].dir
  }
  await loadPrefs()
})

function fmtTime(iso: string) {
  return iso
    ? new Date(iso).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
    : '—'
}
</script>

<template>
  <div class="h-full flex gap-4 overflow-hidden text-ink">
    <!-- 左：目录卡片列表 -->
    <div class="flex-1 flex flex-col min-w-0 gap-4 overflow-y-auto">
      <div class="bg-card border border-line rounded-card p-4 shrink-0">
        <div class="font-bold text-sm mb-1">工作目录</div>
        <p class="text-xs text-ink-2">按目录组织会话与项目偏好；新会话将以此目录作为 Agent 的工作根目录。</p>
        <div class="flex gap-2 mt-3">
          <div class="flex-1"><WorkDirPicker v-model="newDir" /></div>
          <el-button type="primary" :disabled="!newDir.trim()" @click="addDir">
            <el-icon class="mr-1"><FolderAdd /></el-icon>添加目录
          </el-button>
        </div>
      </div>

      <div v-loading="loading" class="grid gap-3 xl:grid-cols-2">
        <div
          v-for="g in groups"
          :key="g.dir"
          class="bg-card border rounded-card p-4 cursor-pointer transition-colors"
          :class="selected === g.dir ? 'border-primary shadow-card' : 'border-line hover:border-primary'"
          @click="selected = g.dir"
        >
          <div class="flex items-center gap-2 min-w-0">
            <el-icon class="text-primary shrink-0"><FolderOpened /></el-icon>
            <span class="font-mono text-sm font-bold truncate">{{ dirLabel(g.dir) }}</span>
            <el-tag v-if="g.running" size="small" type="warning" effect="plain" class="shrink-0">
              {{ g.running }} 运行中
            </el-tag>
          </div>
          <div class="flex gap-4 mt-2 text-xs text-ink-2">
            <span>会话 {{ g.total }}</span>
            <span>最近活跃 {{ fmtTime(g.lastActive) }}</span>
          </div>
          <div class="flex gap-2 mt-3">
            <el-button size="small" type="primary" plain @click.stop="startSession(g.dir)">
              <el-icon class="mr-1"><Promotion /></el-icon>发起新会话
            </el-button>
            <el-button size="small" plain @click.stop="viewSessions(g.dir)">查看会话</el-button>
            <el-button
              v-if="customDirs.includes(g.dir) && !g.total"
              size="small" plain type="danger"
              @click.stop="removeCustomDir(g.dir)"
            >移除</el-button>
          </div>
        </div>
        <div v-if="!loading && !groups.length" class="text-center text-sm text-ink-3 py-16 col-span-2">
          暂无工作目录，用上方选择器添加一个。
        </div>
      </div>
    </div>

    <!-- 右：选中目录的项目偏好（原 project-prefs 页逻辑） -->
    <div class="w-[420px] shrink-0 bg-card border border-line rounded-card flex flex-col overflow-hidden">
      <div class="px-4 py-3 border-b border-line flex items-center justify-between gap-2">
        <div class="min-w-0">
          <div class="font-bold text-sm">项目偏好</div>
          <div class="text-[11px] text-ink-3 font-mono truncate">{{ prefPath || dirLabel(selected) }}</div>
        </div>
        <div class="flex items-center gap-2 shrink-0">
          <el-tag v-if="prefDirty" size="small" type="warning" effect="plain">未保存</el-tag>
          <el-button size="small" plain :loading="prefLoading" @click="loadPrefs">
            <el-icon><Refresh /></el-icon>
          </el-button>
          <el-button size="small" type="primary" :loading="prefSaving" :disabled="!prefDirty" @click="savePrefs">
            保存
          </el-button>
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
  </div>
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
```

- [ ] **Step 9.3: 验证**

```bash
cd web && npm run build
```

对照 `04-projects.png`：左卡片网格 + 右偏好编辑器。旧 `/project-prefs` 链接应重定向到此页。

---

## Task 10: 技能库页（新增「内置技能」Tab，对照 05-skills.png）

**Depends on**: Task 3
**Files**:
- Modify: `web/src/views/skills/index.vue`

**Interfaces**:
- Consumes 新增: `listSkills()`（`@/api/skills`，返回 `{ skills: Skill[] }`；`Skill = { skill_id, name, description, domain, tool_ref, cost, tags }`）

- [ ] **Step 10.1: 应用映射表** 对全文件应用 Global Constraints §3；`skill-editor` scoped 样式里 `#0f1115`/`#d1d5db` → `var(--bma-page)`/`var(--bma-text)`。

- [ ] **Step 10.2: 新增内置技能数据加载**（script 追加）

```ts
import { listSkills } from '@/api/skills'
import type { Skill } from '@/types'

const builtinSkills = ref<Skill[]>([])
const builtinLoading = ref(false)

async function loadBuiltin() {
  builtinLoading.value = true
  try {
    const res = await listSkills()
    builtinSkills.value = res.skills || []
  } catch (e) {
    ElMessage.error('内置技能加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    builtinLoading.value = false
  }
}
```

`onMounted` 中追加 `loadBuiltin()`。

- [ ] **Step 10.3: 新增 Tab 页**（插在「经验技能」与「进化日志」之间）

```vue
<el-tab-pane label="内置技能" name="builtin">
  <div v-loading="builtinLoading" class="grid gap-3 md:grid-cols-2">
    <el-card v-for="s in builtinSkills" :key="s.skill_id" shadow="never">
      <div class="flex items-center gap-2 flex-wrap">
        <span class="font-bold text-ink">{{ s.name }}</span>
        <el-tag size="small" effect="plain">{{ s.domain || '通用' }}</el-tag>
        <el-tag v-if="s.cost" size="small" type="warning" effect="plain">cost {{ s.cost }}</el-tag>
      </div>
      <p class="text-xs text-ink-2 mt-2 line-clamp-2">{{ s.description }}</p>
      <div v-if="s.tool_ref" class="mt-2 text-[11px] text-ink-3 font-mono break-all">工具：{{ s.tool_ref }}</div>
      <div v-if="s.tags?.length" class="mt-2 flex flex-wrap gap-1">
        <el-tag v-for="t in s.tags" :key="t" size="small" effect="plain" class="!text-ink-2">{{ t }}</el-tag>
      </div>
    </el-card>
  </div>
  <div v-if="!builtinLoading && !builtinSkills.length" class="text-center text-sm text-ink-3 py-16">
    暂无内置技能（config/skills.yaml 与插件包注入）。
  </div>
</el-tab-pane>
```

- [ ] **Step 10.4: 验证** `cd web && npm run build`；对照 `05-skills.png`（顶部说明 + Tab 条 + 卡片网格）。

---

## Task 11: 插件页（形态徽标 + 换 token，对照 06-plugins.png）

**Depends on**: Task 3
**Files**:
- Modify: `web/src/views/plugins/index.vue`

- [ ] **Step 11.1: 应用映射表**（全文件 + scoped）。

- [ ] **Step 11.2: 新增形态徽标**（script 追加函数）

```ts
// 形态徽标约定：PluginInfo 无 transport 字段，按 id/kind 判定（spec §06）。
function transportBadge(p: PluginInfo): { label: string; type: 'warning' | 'info' | '' } | null {
  if (p.id === 'host_computer_use') return { label: '宿主直控', type: 'warning' }
  if (p.id === 'computer_use') return { label: 'Docker 沙箱', type: 'info' }
  if (p.kind === 'service') return { label: 'HTTP 服务', type: '' }
  return null
}
```

模板中在 `{{ p.kind }}` 标签后插入：

```vue
<el-tag v-if="transportBadge(p)" size="small" :type="transportBadge(p)!.type" effect="plain">
  {{ transportBadge(p)!.label }}
</el-tag>
```

`host_computer_use` 卡片描述下方追加一行提示（仅该 id 渲染）：

```vue
<div v-if="p.id === 'host_computer_use'" class="text-[11px] text-amber-600 dark:text-amber-400">
  该插件直接操作宿主机 GUI（鼠标/键盘/截屏），与沙箱版 computer_use 不要同时启用。
</div>
```

- [ ] **Step 11.3: 验证** `cd web && npm run build`；对照 `06-plugins.png`。

---

## Task 12: 记忆中心 `/memory`（对照 07-memory.png / 16-memory-project.png）

**Depends on**: Task 3
**Files**:
- Create: `web/src/views/memory/index.vue`（覆盖 Task 3 的临时占位）

**Interfaces**:
- Consumes: `listLearnedSkills()`、`listEvolutionLog(200)`、`getProfile()`、`getProjectPreferences(workDir?)`、`listSessions()`
- **禁用**: `/api/memory/search|levels|eval`（后端空壳桩，见 spec §07）

- [ ] **Step 12.1: 新建 `web/src/views/memory/index.vue`**

```vue
<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'
import { getProfile, getProjectPreferences } from '@/api/preferences'
import {
  listLearnedSkills,
  listEvolutionLog,
  type LearnedSkill,
  type EvolutionLogEntry,
} from '@/api/learned'

const router = useRouter()

const sessions = ref<Session[]>([])
const skills = ref<LearnedSkill[]>([])
const entries = ref<EvolutionLogEntry[]>([])
const profile = ref('')
const projectPref = ref('')
const loading = ref(false)

// 项目筛选：'' = 全部项目；否则为某 work_dir
const project = ref('')
// 类型筛选：'' = 全部
const kind = ref('')

const kindOptions = [
  { value: '', label: '全部' },
  { value: 'user_pref', label: '用户偏好' },
  { value: 'project_lesson', label: '项目经验' },
  { value: 'skill_create', label: '技能新建' },
  { value: 'skill_update', label: '技能更新' },
]

// 会话 id → work_dir 映射：记忆条目的项目归属经 source_session 反查
const sessionDir = computed(() => {
  const m = new Map<string, string>()
  for (const s of sessions.value) m.set(s.id, s.work_dir || '')
  return m
})

const projectOptions = computed(() => {
  const dirs = Array.from(new Set(sessions.value.map((s) => s.work_dir || ''))).sort()
  return [{ value: '', label: '全部项目' }, ...dirs.map((d) => ({ value: d, label: d || '默认目录' }))]
})

// 无 source_session / 反查不到会话 = 全局条目，仅「全部项目」可见
function inProject(sourceSession?: string) {
  if (!project.value) return true
  if (!sourceSession) return false
  return sessionDir.value.get(sourceSession) === project.value
}

const filteredEntries = computed(() =>
  entries.value.filter((e) => inProject(e.source_session) && (!kind.value || e.kind === kind.value))
)

const filteredSkills = computed(() => skills.value.filter((s) => inProject(s.source_session)))

const stats = computed(() => ({
  skillTotal: filteredSkills.value.length,
  skillEnabled: filteredSkills.value.filter((s) => s.enabled).length,
  entryTotal: filteredEntries.value.length,
  sessionTotal: new Set(
    filteredEntries.value.map((e) => e.source_session).filter(Boolean)
  ).size,
}))

async function load() {
  loading.value = true
  try {
    const [sRes, skRes, logRes, pfRes] = await Promise.allSettled([
      listSessions(),
      listLearnedSkills(),
      listEvolutionLog(200),
      getProfile(),
    ])
    sessions.value = sRes.status === 'fulfilled' ? sRes.value || [] : []
    skills.value = skRes.status === 'fulfilled' ? skRes.value.skills || [] : []
    entries.value = logRes.status === 'fulfilled' ? logRes.value.entries || [] : []
    profile.value = pfRes.status === 'fulfilled' ? pfRes.value.content || '' : ''
  } catch (e) {
    ElMessage.error('记忆数据加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
}

async function loadProjectPref() {
  if (!project.value) {
    projectPref.value = ''
    return
  }
  try {
    const res = await getProjectPreferences(project.value)
    projectPref.value = res.content || ''
  } catch {
    projectPref.value = ''
  }
}

watch(project, loadProjectPref)
onMounted(load)

function kindLabel(k: string) {
  return kindOptions.find((o) => o.value === k)?.label || k
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// 摘要：取前 8 行非空行
function summarize(text: string) {
  return text.split('\n').filter((l) => l.trim()).slice(0, 8).join('\n')
}
</script>

<template>
  <div class="h-full flex flex-col gap-4 overflow-y-auto text-ink" v-loading="loading">
    <!-- 顶部：说明 + 筛选 -->
    <div class="bg-card border border-line rounded-card p-4 shrink-0">
      <div class="flex items-center justify-between gap-4 flex-wrap">
        <div>
          <div class="font-bold text-sm">记忆中心</div>
          <p class="text-xs text-ink-2 mt-1">
            全局记忆聚合：会话结束后自动沉淀的用户偏好 / 项目经验 / 经验技能，在此统一浏览与按项目筛选。
          </p>
        </div>
        <div class="flex items-center gap-3 flex-wrap">
          <el-select v-model="project" class="w-64" size="small">
            <el-option v-for="o in projectOptions" :key="o.value" :value="o.value" :label="o.label" />
          </el-select>
          <div class="flex gap-1">
            <el-button
              v-for="o in kindOptions"
              :key="o.value"
              size="small"
              :type="kind === o.value ? 'primary' : ''"
              :plain="kind !== o.value"
              @click="kind = o.value"
            >{{ o.label }}</el-button>
          </div>
        </div>
      </div>
    </div>

    <!-- 统计卡 -->
    <div class="grid grid-cols-2 lg:grid-cols-4 gap-3 shrink-0">
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">经验技能</div>
        <div class="text-2xl font-bold">{{ stats.skillTotal }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">启用中</div>
        <div class="text-2xl font-bold text-green-600">{{ stats.skillEnabled }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">沉淀条目</div>
        <div class="text-2xl font-bold text-primary">{{ stats.entryTotal }}</div>
      </div>
      <div class="bg-card border border-line rounded-card p-4">
        <div class="text-xs text-ink-2 mb-1">涉及会话</div>
        <div class="text-2xl font-bold">{{ stats.sessionTotal }}</div>
      </div>
    </div>

    <!-- 主体：左时间线，右技能 + 画像/偏好摘要 -->
    <div class="flex gap-4 flex-1 min-h-0">
      <div class="flex-1 min-w-0 bg-card border border-line rounded-card p-4 overflow-y-auto">
        <div class="font-bold text-sm mb-3">沉淀时间线</div>
        <el-timeline v-if="filteredEntries.length">
          <el-timeline-item v-for="e in filteredEntries" :key="e.id" :timestamp="fmtTime(e.created_at)" placement="top">
            <div class="flex items-center gap-2 flex-wrap">
              <el-tag size="small" effect="plain">{{ kindLabel(e.kind) }}</el-tag>
              <span class="text-sm font-bold">{{ e.target }}</span>
              <span v-if="e.source_session" class="text-[10px] text-ink-3 font-mono">{{ e.source_session }}</span>
            </div>
            <p class="text-xs text-ink-2 mt-1">{{ e.summary }}</p>
          </el-timeline-item>
        </el-timeline>
        <div v-else class="text-center text-sm text-ink-3 py-16">当前筛选下暂无沉淀条目。</div>
      </div>

      <div class="w-[380px] shrink-0 flex flex-col gap-4 overflow-y-auto">
        <div class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">经验技能</span>
            <router-link to="/skills" class="text-xs text-primary hover:underline">管理 →</router-link>
          </div>
          <div v-if="!filteredSkills.length" class="text-xs text-ink-3 py-2">暂无经验技能</div>
          <div v-for="s in filteredSkills" :key="s.name" class="p-2 bg-page rounded border border-line mb-1.5 text-xs">
            <div class="flex items-center gap-2">
              <span class="font-bold">{{ s.title }}</span>
              <el-tag size="small" effect="plain" :type="s.enabled ? 'success' : 'info'">
                {{ s.enabled ? '启用' : '禁用' }}
              </el-tag>
            </div>
            <p class="text-ink-2 mt-1 line-clamp-2">{{ s.when_to_use }}</p>
          </div>
        </div>

        <div class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">用户画像摘要</span>
            <router-link to="/profile" class="text-xs text-primary hover:underline">编辑 →</router-link>
          </div>
          <pre v-if="profile" class="text-xs text-ink-2 whitespace-pre-wrap break-all font-mono bg-page border border-line rounded p-2">{{ summarize(profile) }}</pre>
          <div v-else class="text-xs text-ink-3 py-2">暂无画像内容</div>
        </div>

        <div v-if="project" class="bg-card border border-line rounded-card p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-bold text-sm">项目偏好摘要</span>
            <router-link to="/projects" class="text-xs text-primary hover:underline">编辑 →</router-link>
          </div>
          <pre v-if="projectPref" class="text-xs text-ink-2 whitespace-pre-wrap break-all font-mono bg-page border border-line rounded p-2">{{ summarize(projectPref) }}</pre>
          <div v-else class="text-xs text-ink-3 py-2">该项目暂无偏好内容</div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.line-clamp-2 {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
</style>
```

- [ ] **Step 12.2: 验证** `cd web && npm run build`；对照 `07-memory.png`（全部项目态）与 `16-memory-project.png`（项目筛选态）。

---

## Task 13: 记忆分组其余页面（profile / soul / history / knowledge）

**Depends on**: Task 3
**Files**:
- Modify: `web/src/views/profile/index.vue`（仅映射表换 token）
- Modify: `web/src/views/soul/index.vue`（全量重写）
- Modify: `web/src/views/history/index.vue`（全量重写为表格页）
- Modify: `web/src/views/knowledge/index.vue`（仅映射表换 token）
- Modify: `web/src/components/FeaturePlaceholder.vue`、`web/src/components/AppCard.vue`（映射表换 token，全局复用）

- [ ] **Step 13.1: profile / knowledge / FeaturePlaceholder / AppCard 应用映射表**。FeaturePlaceholder 中 `bg-[#1e3a8a]/20` → `bg-primary-soft`，`text-gray-200` → `text-ink`，`text-gray-500` → `text-ink-3`。

- [ ] **Step 13.2: 全量重写 `web/src/views/soul/index.vue`**

```vue
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { getStatus } from '@/api/health'

const soul = ref('default')
const llm = ref('')

onMounted(async () => {
  try {
    const s = await getStatus()
    soul.value = s.soul || 'default'
    llm.value = `${s.llm_provider || ''} / ${s.llm_model || ''}`
  } catch {
    // ignore
  }
})
</script>

<template>
  <div class="h-full overflow-y-auto text-ink">
    <div class="max-w-2xl mx-auto flex flex-col items-center text-center pt-16">
      <div class="rounded-full bg-primary-soft p-5 mb-4">
        <el-icon class="text-5xl text-primary"><User /></el-icon>
      </div>
      <div class="text-lg font-bold mb-1">人格配置</div>
      <div class="text-sm text-ink-2 mb-6">当前人格决定了 Agent 的语气、价值取向与决策风格。</div>

      <div class="w-full bg-card border border-line rounded-card p-4 text-left text-sm space-y-2">
        <div class="flex justify-between">
          <span class="text-ink-2">当前人格</span>
          <span class="font-bold">{{ soul }}</span>
        </div>
        <div class="flex justify-between">
          <span class="text-ink-2">LLM</span>
          <span class="font-mono text-xs">{{ llm || '-' }}</span>
        </div>
        <div class="flex justify-between">
          <span class="text-ink-2">定义文件</span>
          <span class="font-mono text-xs">config/soul.md</span>
        </div>
      </div>

      <div class="w-full mt-4 bg-primary-soft border border-line rounded-card p-4 text-xs text-ink-2 text-left">
        人格的热切换与在线编辑能力规划中。当前修改方式：编辑安装目录下的
        <span class="font-mono">config/soul.md</span> 后重启后端生效。
      </div>
    </div>
  </div>
</template>
```

（先打开 `web/src/api/health.ts` 核对 `StatusResponse` 含 `soul/llm_provider/llm_model` 字段；后端 `/api/status` 返回键已确认含此三者。）

- [ ] **Step 13.3: 全量重写 `web/src/views/history/index.vue`**

```vue
<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { Session } from '@/types'
import { listSessions } from '@/api/session'
import { statusTagType, statusText } from '@/utils/sessionStatus'

const route = useRoute()
const router = useRouter()

const sessions = ref<Session[]>([])
const loading = ref(false)
const search = ref('')
const statusFilter = ref('')
// 工作目录页「查看会话」跳入：?work_dir= 预过滤
const workDirFilter = ref((route.query.work_dir as string) || '')

const statusOptions = [
  { value: '', label: '全部状态' },
  { value: 'running', label: '运行中' },
  { value: 'completed', label: '已完成' },
  { value: 'error', label: '失败' },
  { value: 'awaiting_clarify', label: '待澄清' },
  { value: 'paused_on_child', label: '子Agent暂停' },
]

const workDirOptions = computed(() => {
  const dirs = Array.from(new Set(sessions.value.map((s) => s.work_dir || ''))).sort()
  return [{ value: '', label: '全部目录' }, ...dirs.map((d) => ({ value: d, label: d || '默认目录' }))]
})

const rows = computed(() =>
  sessions.value.filter((s) => {
    if (statusFilter.value && s.status !== statusFilter.value) return false
    if (workDirFilter.value && (s.work_dir || '') !== workDirFilter.value) return false
    const q = search.value.trim().toLowerCase()
    if (q && !s.id.toLowerCase().includes(q) && !(s.goal || '').toLowerCase().includes(q)) return false
    return true
  })
)

onMounted(async () => {
  loading.value = true
  try {
    sessions.value = await listSessions()
  } catch (e) {
    ElMessage.error('会话历史加载失败：' + (e instanceof Error ? e.message : String(e)))
  } finally {
    loading.value = false
  }
})

function open(s: Session) {
  router.push({ path: '/session', query: { id: s.id, view: 'monitor' } })
}

function fmt(t?: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : '—'
}

function duration(s: Session) {
  if (!s.started_at || !s.ended_at) return '—'
  const ms = new Date(s.ended_at).getTime() - new Date(s.started_at).getTime()
  if (ms < 0) return '—'
  const mm = Math.floor(ms / 60000)
  const ss = Math.floor((ms % 60000) / 1000)
  return mm ? `${mm}m ${ss}s` : `${ss}s`
}
</script>

<template>
  <div class="h-full flex flex-col gap-4 overflow-hidden text-ink">
    <div class="bg-card border border-line rounded-card p-4 shrink-0 flex items-center gap-3 flex-wrap">
      <div class="font-bold text-sm mr-auto">会话历史</div>
      <el-input v-model="search" size="small" placeholder="搜索目标 / ID…" class="w-56">
        <template #prefix><el-icon class="text-ink-3"><Search /></el-icon></template>
      </el-input>
      <el-select v-model="statusFilter" size="small" class="w-32">
        <el-option v-for="o in statusOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
      <el-select v-model="workDirFilter" size="small" class="w-56">
        <el-option v-for="o in workDirOptions" :key="o.value" :value="o.value" :label="o.label" />
      </el-select>
    </div>

    <div class="flex-1 min-h-0 bg-card border border-line rounded-card overflow-hidden">
      <el-table v-loading="loading" :data="rows" class="w-full" height="100%">
        <el-table-column label="目标" min-width="280">
          <template #default="{ row }">
            <div class="font-bold text-sm truncate">{{ row.goal || '(无目标)' }}</div>
            <div class="text-[11px] text-ink-3 font-mono">{{ row.id }}</div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag size="small" :type="statusTagType(row.status)" effect="plain">{{ statusText(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="工作目录" min-width="220">
          <template #default="{ row }">
            <span class="font-mono text-xs text-ink-2">{{ row.work_dir || '默认目录' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="开始时间" width="160">
          <template #default="{ row }"><span class="text-xs text-ink-2">{{ fmt(row.started_at) }}</span></template>
        </el-table-column>
        <el-table-column label="耗时" width="100">
          <template #default="{ row }"><span class="text-xs text-ink-2">{{ duration(row) }}</span></template>
        </el-table-column>
        <el-table-column width="80" align="right">
          <template #default="{ row }">
            <el-button size="small" link type="primary" @click="open(row)">打开</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="text-sm text-ink-3 py-10">暂无会话历史</div>
        </template>
      </el-table>
    </div>
  </div>
</template>
```

（先打开 `web/src/utils/sessionStatus.ts` 核对 `statusTagType/statusText` 导出签名。）

- [ ] **Step 13.4: 验证** `cd web && npm run build`；history 行点击应跳入 `/session?id=&view=monitor`。

---

## Task 14: 系统设置页 + 工作流 Beta 占位页（对照 08/09）

**Depends on**: Task 3
**Files**:
- Modify: `web/src/views/settings/index.vue`（全量重写）
- Modify: `web/src/views/workflow/index.vue`（替换 Task 3 临时版）

**Interfaces**:
- Consumes: `useTheme()`（Task 1）、`getStatus/getHealth`

- [ ] **Step 14.1: 全量重写 `web/src/views/settings/index.vue`**

```vue
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
```

- [ ] **Step 14.2: 全量重写 `web/src/views/workflow/index.vue`**

```vue
<template>
  <div class="h-full overflow-y-auto text-ink">
    <div class="max-w-2xl mx-auto flex flex-col items-center text-center pt-16">
      <div class="rounded-full bg-primary-soft p-5 mb-4">
        <el-icon class="text-5xl text-primary"><SetUp /></el-icon>
      </div>
      <div class="flex items-center gap-2 mb-1">
        <span class="text-lg font-bold">工作流编排</span>
        <el-tag size="small" type="warning" effect="plain">Beta</el-tag>
      </div>
      <div class="text-sm text-ink-2 mb-6">可视化编排多 Agent 工作流，该能力为 Beta 预留，入口先行开放。</div>

      <div class="w-full bg-card border border-line rounded-card p-5 text-left text-sm space-y-3">
        <div class="font-bold text-sm">规划能力</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Connection /></el-icon>拖拽节点编排：Meta / Domain / Assistant 角色自由组合</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Share /></el-icon>依赖连线与条件分支，声明式 DAG</div>
        <div class="flex items-start gap-2 text-ink-2"><el-icon class="mt-0.5 text-primary"><Monitor /></el-icon>运行观测：节点级状态、日志与指标联动会话监控</div>
      </div>
    </div>
  </div>
</template>
```

注意：用到 `Share` 图标，需在 `main.ts` 补注册（两个数组都加）。

- [ ] **Step 14.3: 验证** `cd web && npm run build`；对照 `08-settings.png`、`09-workflow.png`。

---

## Task 15: 收尾（旧文件清理 + 全量视觉走查）

**Depends on**: Task 4–14 全部
**Files**:
- Delete: `web/src/views/project-prefs/`（整目录）

- [ ] **Step 15.1: 清理与残留扫描**

```bash
rm -rf web/src/views/project-prefs
cd web && grep -rn "@/views/chat\|project-prefs" src || echo "OK: 无残留引用"
grep -rn "#0f1115\|#1a1d24\|#14161a\|#2a2d35\|#1e3a8a" src || echo "OK: 无残留硬编码深色"
```

允许的例外：`dashboard/index.vue` 品牌插画 SVG 的渐变色（`#3b82f6`/`#1d4ed8` 等，双主题通用）。其余命中一律按映射表清除。

- [ ] **Step 15.2: 全量构建**

```bash
cd web && npm run build
```

vue-tsc 必须零报错。

- [ ] **Step 15.3: 视觉走查清单**（起后端 + `npm run dev`，逐页对照 `doc/image/redesign/`）

| 页面 | 预览图 | 检查点 |
|---|---|---|
| `/dashboard` | 01 | 浅色卡片、创建卡、列表过滤 chip、右侧统计+趋势+活动 |
| `/session?view=chat` | 02 | 三栏、标题旁 💬/📊 切换器、右栏 5 Tab |
| `/session?view=monitor` | 03/15 | 中栏三 Tab（执行日志/Skill装配/日志分析）、右栏同 02 |
| 右栏 Tab | 11/12/13/14 | 工具/文件/记忆/指标 四态 |
| `/projects` | 04 | 目录卡片网格 + 右侧项目偏好编辑器 |
| `/skills` | 05 | 三 Tab（经验技能/内置技能/进化日志） |
| `/plugins` | 06 | 形态徽标（宿主直控/Docker 沙箱/HTTP 服务） |
| `/memory` | 07/16 | 项目下拉 + 类型 chip + 统计卡 + 时间线 |
| `/settings` | 08 | 主题切换即时生效并持久化 |
| `/workflow` | 09 | Beta 占位 |
| 全局 | 10 | 侧栏分组 + 底部 Beta 虚线项；顶栏主题切换 |

每页同时切深色主题复查一遍（token 双主题）。

- [ ] **Step 15.4: 功能冒烟**

1. `/chat?id=<旧会话>` 重定向到 `/session?id=<旧会话>` 且对话可见。
2. `/project-prefs` 重定向到 `/projects`。
3. `/projects` 添加目录 → 发起新会话 → 会话页 `?work_dir=` 生效（创建会话请求体带 `work_dir`）。
4. 会话页发送消息 → SSE 正常、软停止/中断/终止按钮可用、右栏工具 Tab 出现调用记录。
5. 刷新页面主题保持。

---

## Self-Review 记录（计划落稿时已完成）

- **Spec 覆盖**：12 路由 + 2 重定向 + Beta 占位全覆盖；07/16 记忆中心按"真实 API 聚合"实现，未接入空壳 `/api/memory/*`。
- **占位符扫描**：无 TBD / "类似 Task N" / "适当错误处理"；全部新增文件给出完整代码，restyle 任务给出确定性映射表。
- **类型一致性**：`Skill`（skill_id/name/description/domain/tool_ref/cost/tags）、`LearnedSkill.source_session`、`Session.work_dir/ended_at`、`SessionLogsPanel` 的 model 名等均已对照源码核实；`defineModel` 需 Vue 3.4+（项目 3.4 ✓）。
- **执行顺序**：Task 5 的构建验证显式推迟到 Task 8 完成后（容器引用后续任务的组件），已在 Step 5.3 注明。

## 执行方式

- **Subagent-Driven（推荐）**：每个 Task 派一个子代理执行 + 完成后我审阅，按依赖序推进（1 → 2 → 3 → 4∥9∥10∥11∥12∥13∥14 → 5 → 6 → 7 → 8 → 15）。
- **Inline Execution**：我在本会话内顺序执行全部 Task。
