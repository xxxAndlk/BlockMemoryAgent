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
        path: 'chat',
        name: 'Chat',
        component: () => import('@/views/chat/index.vue'),
        meta: { title: '对话', icon: 'ChatDotRound' }
      },
      {
        path: 'session',
        name: 'Session',
        component: () => import('@/views/session/index.vue'),
        meta: { title: '会话监控', icon: 'Monitor' }
      },
      {
        path: 'plugins',
        name: 'Plugins',
        component: () => import('@/views/plugins/index.vue'),
        meta: { title: '插件管理', icon: 'MagicStick' }
      },
      {
        path: 'knowledge',
        name: 'Knowledge',
        component: () => import('@/views/knowledge/index.vue'),
        meta: { title: '知识库', icon: 'Document' }
      },
      {
        path: 'history',
        name: 'History',
        component: () => import('@/views/history/index.vue'),
        meta: { title: '会话历史', icon: 'Clock' }
      },
      {
        path: 'profile',
        name: 'Profile',
        component: () => import('@/views/profile/index.vue'),
        meta: { title: '用户画像', icon: 'UserFilled' }
      },
      {
        path: 'project-prefs',
        name: 'ProjectPrefs',
        component: () => import('@/views/project-prefs/index.vue'),
        meta: { title: '项目偏好', icon: 'Document' }
      },
      {
        path: 'skills',
        name: 'Skills',
        component: () => import('@/views/skills/index.vue'),
        meta: { title: '技能库', icon: 'Connection' }
      },
      {
        path: 'soul',
        name: 'Soul',
        component: () => import('@/views/soul/index.vue'),
        meta: { title: '人格配置', icon: 'User' }
      },
      {
        path: 'settings',
        name: 'Settings',
        component: () => import('@/views/settings/index.vue'),
        meta: { title: '系统设置', icon: 'Setting' }
      }
    ]
  }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

export default router
