import { createRouter, createWebHashHistory } from 'vue-router'
import Dashboard from '../views/Dashboard.vue'

const routes = [
  { path: '/', name: 'dashboard', component: Dashboard },
  { path: '/chat', name: 'chat', component: () => import('../views/Chat.vue') },
  { path: '/sessions', name: 'sessions', component: () => import('../views/Sessions.vue') },
  { path: '/agents', name: 'agents', component: () => import('../views/Agents.vue') },
  { path: '/board', name: 'board', component: () => import('../views/Board.vue') },
  { path: '/memory', name: 'memory', component: () => import('../views/Memory.vue') },
  { path: '/skills', name: 'skills', component: () => import('../views/Skills.vue') },
  { path: '/files', name: 'files', component: () => import('../views/Files.vue') },
  { path: '/knowledge', name: 'knowledge', component: () => import('../views/Knowledge.vue') },
  { path: '/history', name: 'history', component: () => import('../views/History.vue') },
  { path: '/health', name: 'health', component: () => import('../views/Health.vue') },
  { path: '/settings', name: 'settings', component: () => import('../views/Settings.vue') },
]

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
