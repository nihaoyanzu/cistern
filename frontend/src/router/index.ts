import { createRouter, createWebHistory } from 'vue-router'
import { getToken } from '@/api/client'
import { i18n } from '@/i18n'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/',
      name: 'guide',
      component: () => import('@/views/Guide.vue'),
      meta: { public: true, titleKey: 'guide.title' },
    },
    // 兼容旧链接；说明页始终在 /
    { path: '/guide', redirect: '/' },
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/Login.vue'),
      meta: { public: true, titleKey: 'login.loginBtn' },
    },
    {
      path: '/dashboard',
      name: 'dashboard',
      component: () => import('@/views/Dashboard.vue'),
      meta: { titleKey: 'dashboard.title' },
    },
    {
      path: '/access',
      name: 'access',
      component: () => import('@/views/RecentAccess.vue'),
      meta: { titleKey: 'access.title' },
    },
    {
      path: '/platform',
      name: 'platform',
      component: () => import('@/views/Platform.vue'),
      meta: { titleKey: 'platform.title', module: 'pypi' },
    },
    {
      path: '/settings',
      name: 'settings',
      component: () => import('@/views/SystemSettings.vue'),
      meta: { titleKey: 'system.title' },
    },
    { path: '/rate-limit', redirect: { path: '/settings', query: { tab: 'rate' } } },
    { path: '/cache', redirect: { path: '/settings', query: { tab: 'cache' } } },
    {
      path: '/queue',
      name: 'queue',
      component: () => import('@/views/Queue.vue'),
      meta: { titleKey: 'queue.title', module: 'pypi' },
    },
    {
      path: '/packages',
      name: 'packages',
      component: () => import('@/views/Packages.vue'),
      meta: { titleKey: 'packages.title', module: 'pypi' },
    },
    {
      path: '/prefetch',
      name: 'prefetch',
      component: () => import('@/views/Prefetch.vue'),
      meta: { titleKey: 'prefetch.title', module: 'pypi' },
    },
  ],
})

router.beforeEach((to) => {
  if (to.meta.public) return true
  if (!getToken()) return { name: 'login', query: { redirect: to.fullPath } }
  return true
})

router.afterEach((to) => {
  const key = typeof to.meta.titleKey === 'string' ? to.meta.titleKey : ''
  if (key) {
    document.title = `${String(i18n.global.t(key))} · Cistern`
  } else {
    document.title = 'Cistern'
  }
})

export default router
