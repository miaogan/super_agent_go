import { createRouter, createWebHashHistory } from 'vue-router'
import { useAuthStore } from '../stores/auth'

// hash 模式：静态托管（gateway/nginx）下刷新不丢路由
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('../views/LoginView.vue') },
    {
      path: '/',
      component: () => import('../views/LayoutView.vue'),
      redirect: '/agents',
      children: [
        { path: 'agents', name: 'agents', component: () => import('../views/agents/AgentListView.vue') },
        { path: 'agents/:id/chat', name: 'chat', component: () => import('../views/agents/AgentChatView.vue') },
        { path: 'kbs', name: 'kbs', component: () => import('../views/kb/KbListView.vue') },
        { path: 'kbs/:id', name: 'kb-detail', component: () => import('../views/kb/KbDetailView.vue') },
        { path: 'system/models', name: 'models', component: () => import('../views/system/ModelInfoView.vue') },
        { path: 'system/users', name: 'users', component: () => import('../views/system/UserManageView.vue') },
      ],
    },
    { path: '/:pathMatch(.*)*', redirect: '/agents' },
  ],
})

// 路由守卫：未登录一律进登录页
router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.name !== 'login' && !auth.isLoggedIn) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && auth.isLoggedIn) {
    return { name: 'agents' }
  }
  return true
})

export default router
