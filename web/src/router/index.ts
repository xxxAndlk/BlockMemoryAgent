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
        path: 'dag',
        name: 'Dag',
        component: () => import('@/views/dag/index.vue'),
        meta: { title: '定时任务', icon: 'AlarmClock' }
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
