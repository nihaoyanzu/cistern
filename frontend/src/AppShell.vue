<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { useSessionStore } from '@/stores/session'
import { useToastStore } from '@/stores/toast'
import { useThemeStore } from '@/stores/theme'
import { useStatsStore } from '@/stores/stats'
import { useModulesStore } from '@/stores/modules'
import { setLocale, type SupportedLocale } from '@/i18n'
import { api } from '@/api/client'
import type { UpdateProgress, UpdateStatus } from '@/types/api'
import { fmtBytes, fmtRate } from '@/lib/format'
import { MODULES, moduleColor } from '@/modules/registry'
import ModuleIcon from '@/components/ModuleIcon.vue'
import ModalDialog from '@/components/ModalDialog.vue'

const { t, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const session = useSessionStore()
const toast = useToastStore()
const theme = useThemeStore()
const statsStore = useStatsStore()
const modulesStore = useModulesStore()

const sidebarOpen = ref(false)
const userMenuOpen = ref(false)

type NavLink = { to: string; icon: string; text: string; module?: string }
/** item=顶层单链；section=可折叠分组；module=已启用镜像模块 */
type NavGroup = {
  id: string
  label: string
  kind: 'item' | 'section' | 'module'
  links: NavLink[]
  /** 模块分组专用图标（与 ModuleDescriptor.navIcon 一致） */
  navIcon?: string
}

const navGroups = computed((): NavGroup[] => {
  const groups: NavGroup[] = [
    {
      id: 'dashboard',
      label: t('nav.dashboard'),
      kind: 'item',
      links: [{ to: '/dashboard', icon: 'dash', text: t('nav.dashboard') }],
    },
    {
      id: 'access',
      label: t('nav.access'),
      kind: 'item',
      links: [{ to: '/access', icon: 'access', text: t('nav.access') }],
    },
    {
      id: 'queue',
      label: t('nav.queue'),
      kind: 'item',
      links: [{ to: '/queue', icon: 'queue', text: t('nav.queue') }],
    },
  ]

  for (const d of MODULES) {
    if (!modulesStore.isEnabled(d.id)) continue
    const links: NavLink[] = []
    if (d.nav.catalog) {
      links.push({
        to: `/packages?module=${d.id}`,
        icon: 'pkg',
        text: t(d.nav.catalogLabelKey),
        module: d.id,
      })
    }
    if (d.nav.prefetch) {
      links.push({
        to: `/prefetch?module=${d.id}`,
        icon: 'pf',
        text: t(d.nav.prefetchLabelKey),
        module: d.id,
      })
    }
    if (!links.length) continue
    groups.push({
      id: `mod-${d.id}`,
      label: t(d.labelKey),
      kind: 'module',
      navIcon: d.navIcon,
      links,
    })
  }

  groups.push({
    id: 'system',
    label: t('nav.system'),
    kind: 'section',
    links: [
      { to: '/platform', icon: 'plat', text: t('nav.platform') },
      { to: '/settings', icon: 'sys', text: t('nav.settings') },
      { to: '/', icon: 'guide', text: t('nav.guide') },
    ],
  })
  return groups
})

const expanded = reactive<Record<string, boolean>>({})

function ensureExpandedDefaults() {
  for (const g of navGroups.value) {
    if (g.kind === 'item') continue
    if (expanded[g.id] === undefined) {
      expanded[g.id] = g.id === 'system' || groupActive(g)
    }
  }
}

function groupActive(g: NavGroup): boolean {
  return g.links.some((l) => linkActive(l))
}

function linkActive(link: NavLink): boolean {
  const path = route.path
  if (link.to.startsWith('/packages')) {
    return path === '/packages' && String(route.query.module || '') === (link.module || '')
  }
  if (link.to.startsWith('/prefetch')) {
    return path === '/prefetch' && String(route.query.module || '') === (link.module || '')
  }
  if (link.to === '/') return path === '/'
  return path === link.to || path.startsWith(link.to + '/')
}

function collapseOtherModules(exceptId: string) {
  for (const g of navGroups.value) {
    if (g.kind === 'module' && g.id !== exceptId) {
      expanded[g.id] = false
    }
  }
}

function toggleGroup(id: string) {
  const next = !expanded[id]
  expanded[id] = next
  if (next && id.startsWith('mod-')) {
    collapseOtherModules(id)
  }
}

watch(
  navGroups,
  () => {
    ensureExpandedDefaults()
    for (const g of navGroups.value) {
      if (groupActive(g)) {
        expanded[g.id] = true
        if (g.kind === 'module') collapseOtherModules(g.id)
      }
    }
  },
  { immediate: true },
)

watch(
  () => [route.path, route.query.module],
  () => {
    for (const g of navGroups.value) {
      if (groupActive(g)) {
        expanded[g.id] = true
        if (g.kind === 'module') collapseOtherModules(g.id)
      }
    }
  },
)

const stats = computed(() => statsStore.data)
const pullBps = computed(() => Number(stats.value?.traffic?.upstream_bps ?? 0))
const shareBps = computed(() => Number(stats.value?.traffic?.downstream_bps ?? 0))
const pullTotal = computed(() => Number(stats.value?.traffic?.upstream_total ?? 0))
const shareTotal = computed(() => Number(stats.value?.traffic?.downstream_total ?? 0))
const p0Active = computed(() => Number(stats.value?.interactive_active ?? 0))
const p1Active = computed(() => Number(stats.value?.resume_count ?? 0))
const cacheSize = computed(() => Number(stats.value?.cache?.total_size ?? 0))

const updateStatus = ref<UpdateStatus | null>(null)
const updateLoading = ref(false)
const showResultModal = ref(false)
const showConfirmModal = ref(false)
const showProgressModal = ref(false)
const applying = ref(false)
const cancellingUpdate = ref(false)
const resultText = ref('')
const liveProgress = ref<UpdateProgress | null>(null)
let updateTimer: number | undefined
let progressTimer: number | undefined

const idleProgress = (): UpdateProgress => ({
  phase: 'idle',
  bytes_done: 0,
  bytes_total: 0,
  percent: 0,
  can_cancel: false,
})

const displayVersion = computed(() => {
  const v = updateStatus.value?.current || '…'
  return v.startsWith('v') ? v : `v${v}`
})

const progressPercent = computed(() => {
  const p = liveProgress.value
  if (!p) return 0
  if (p.percent > 0) return Math.min(100, Math.round(p.percent))
  if (p.bytes_total > 0) return Math.min(100, Math.round((p.bytes_done / p.bytes_total) * 100))
  return 0
})

const progressLabel = computed(() => {
  const p = liveProgress.value
  if (!p) return t('common.updateApplying')
  if (p.phase === 'restarting') return t('common.updateProgressRestart')
  if (p.phase === 'failed') return p.error || t('common.updateFailed')
  if (p.phase === 'cancelled') return t('common.updateCancelled')
  if (p.message) return p.message
  return t('common.updateApplying')
})

const canCancelUpdate = computed(
  () => !!liveProgress.value?.can_cancel && applying.value && !cancellingUpdate.value,
)

async function checkUpdate(force = false) {
  updateLoading.value = true
  try {
    updateStatus.value = await api.getUpdate(force)
    if (updateStatus.value?.progress) {
      liveProgress.value = updateStatus.value.progress
    }
  } catch (e: any) {
    updateStatus.value = {
      current: updateStatus.value?.current || 'dev',
      update_available: false,
      error: e?.message || String(e),
      checked_at: new Date().toISOString(),
      progress: updateStatus.value?.progress || idleProgress(),
    }
  } finally {
    updateLoading.value = false
  }
}

function onVersionClick() {
  if (updateLoading.value || applying.value) return
  const st = updateStatus.value
  if (st?.update_available) {
    showConfirmModal.value = true
    return
  }
  if (!st) {
    resultText.value = t('common.updateChecking')
    showResultModal.value = true
    void checkUpdate(true).then(() => {
      const next = updateStatus.value
      if (next?.update_available) {
        showResultModal.value = false
        showConfirmModal.value = true
        return
      }
      resultText.value = formatUpdateResult(next)
    })
    return
  }
  resultText.value = formatUpdateResult(st)
  showResultModal.value = true
  void checkUpdate(true).then(() => {
    if (!showResultModal.value) return
    const next = updateStatus.value
    if (next?.update_available) {
      showResultModal.value = false
      showConfirmModal.value = true
      return
    }
    resultText.value = formatUpdateResult(next)
  })
}

function formatUpdateResult(st: UpdateStatus | null | undefined): string {
  if (!st) return t('common.updateChecking')
  if (st.error) return t('common.updateError', { error: st.error })
  if (st.update_available) {
    return t('common.updateConfirmDesc', {
      current: st.current,
      latest: st.latest || '',
    })
  }
  return t('common.updateLatest', { current: st.current || 'dev' })
}

function stopProgressPoll() {
  if (progressTimer) {
    clearInterval(progressTimer)
    progressTimer = undefined
  }
}

async function pollUpdateProgressOnce(): Promise<string> {
  try {
    const st = await api.getUpdate(false)
    updateStatus.value = st
    liveProgress.value = st.progress || idleProgress()
    return st.progress?.phase || 'idle'
  } catch {
    return 'unreachable'
  }
}

async function waitForServiceBack() {
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 1500))
    try {
      const res = await fetch('/health')
      if (res.ok) {
        await checkUpdate(true)
        return true
      }
    } catch {
      /* 重启中 */
    }
  }
  return false
}

async function confirmUpgrade() {
  applying.value = true
  cancellingUpdate.value = false
  showProgressModal.value = true
  liveProgress.value = {
    phase: 'preparing',
    bytes_done: 0,
    bytes_total: 0,
    percent: 0,
    message: t('common.updateApplying'),
    can_cancel: true,
  }
  try {
    await api.applyUpdate()
    stopProgressPoll()
    progressTimer = window.setInterval(() => {
      void pollUpdateProgressOnce()
    }, 400)

    let phase = await pollUpdateProgressOnce()
    while (applying.value) {
      phase = await pollUpdateProgressOnce()
      if (phase === 'failed') {
        toast.err(liveProgress.value?.error || t('common.updateFailed'))
        break
      }
      if (phase === 'cancelled') {
        toast.ok(t('common.updateCancelled'))
        break
      }
      if (phase === 'restarting' || phase === 'unreachable') {
        liveProgress.value = {
          ...(liveProgress.value || idleProgress()),
          phase: 'restarting',
          percent: 100,
          message: t('common.updateProgressWaiting'),
          can_cancel: false,
        }
        stopProgressPoll()
        const ok = await waitForServiceBack()
        if (ok) toast.ok(t('common.updateApplied'))
        else toast.err(t('common.updateFailed'))
        break
      }
      await new Promise((r) => setTimeout(r, 400))
    }
  } catch (e: any) {
    toast.err(e?.message || t('common.updateFailed'))
  } finally {
    stopProgressPoll()
    applying.value = false
    cancellingUpdate.value = false
    showProgressModal.value = false
  }
}

async function cancelUpgrade() {
  if (!canCancelUpdate.value) return
  cancellingUpdate.value = true
  try {
    await api.cancelUpdate()
  } catch (e: any) {
    toast.err(e?.message || t('common.updateFailed'))
    cancellingUpdate.value = false
  }
}

/** 升级进行中禁止关掉进度窗；可取消阶段点关闭=请求取消 */
function onProgressVisible(v: boolean) {
  if (v) {
    showProgressModal.value = true
    return
  }
  if (applying.value) {
    if (canCancelUpdate.value) void cancelUpgrade()
    showProgressModal.value = true
    return
  }
  showProgressModal.value = false
}

onMounted(() => {
  session.refresh()
  modulesStore.refresh()
  statsStore.startPolling(3000)
  void checkUpdate(true)
  updateTimer = window.setInterval(() => void checkUpdate(false), 6 * 60 * 60 * 1000)
  document.addEventListener('click', onDocClick)
})

onUnmounted(() => {
  statsStore.stopPolling()
  if (updateTimer) clearInterval(updateTimer)
  stopProgressPoll()
  document.removeEventListener('click', onDocClick)
})

const userMenuRef = ref<HTMLElement | null>(null)

function toggleLang() {
  const next: SupportedLocale = locale.value === 'zh-CN' ? 'en' : 'zh-CN'
  setLocale(next)
}

function closeSidebar() {
  sidebarOpen.value = false
}

function toggleUserMenu() {
  userMenuOpen.value = !userMenuOpen.value
}

function closeUserMenu() {
  userMenuOpen.value = false
}

function onDocClick(e: MouseEvent) {
  const el = userMenuRef.value
  if (!el) return
  if (!el.contains(e.target as Node)) closeUserMenu()
}

async function logout() {
  closeUserMenu()
  try {
    await api.logout()
  } catch {
    /* 本地清会话即可 */
  }
  session.reset()
  toast.info(t('toast.loggedOut'))
  router.replace('/login')
}

function goAccount() {
  closeUserMenu()
  router.push({ path: '/settings', query: { tab: 'account' } })
}
</script>

<template>
  <Transition name="fade">
    <div
      v-if="sidebarOpen"
      class="fixed inset-0 z-40 bg-black/50 backdrop-blur-sm lg:hidden"
      @click="closeSidebar"
    />
  </Transition>

  <aside
    class="app-sidebar fixed inset-y-0 left-0 z-50 flex w-[260px] flex-col
      transition-transform duration-200 lg:translate-x-0"
    :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full'"
  >
    <div class="app-sidebar-brand flex h-14 shrink-0 items-center gap-2.5 px-5">
      <img src="/favicon.svg" alt="" class="h-7 w-7 shrink-0" width="28" height="28" />
      <div>
        <div class="text-[15px] font-semibold leading-tight tracking-tight text-fg">
          mirror<span class="text-accent">hub</span>
        </div>
        <div class="text-[10px] font-medium uppercase tracking-[0.12em] text-muted">mirror platform</div>
      </div>
    </div>

    <nav class="app-sidebar-nav flex-1 overflow-y-auto px-3 pb-4 pt-2">
      <template v-for="(g, idx) in navGroups" :key="g.id">
        <div
          v-if="g.kind === 'module' && (idx === 0 || navGroups[idx - 1]?.kind !== 'module')"
          class="nav-workspace-label"
        >
          {{ t('nav.workspace') }}
        </div>

        <!-- 一级单链 -->
        <router-link
          v-if="g.kind === 'item' && g.links[0]"
          :to="g.links[0].to"
          class="nav-item nav-item--top"
          :class="{ 'is-active': linkActive(g.links[0]) }"
          @click="closeSidebar"
        >
          <svg class="nav-item-icon" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
            <path v-if="g.links[0].icon === 'dash'" d="M10 2a8 8 0 100 16 8 8 0 000-16zm0 2c-.56 0-1.1.1-1.6.28a1 1 0 00.32 1.96A4 4 0 0110 6zm-4.32.68a1 1 0 00-1.4 0A6 6 0 006 12a1 1 0 001.96-.32A4 4 0 015.68 6.68zm8.64 0a4 4 0 01-2.32 5A1 1 0 0013.4 12a6 6 0 00.92-5.32z" />
            <path v-else-if="g.links[0].icon === 'access'" d="M3 4a1 1 0 011-1h12a1 1 0 011 1v2H3V4zm0 4h14v8a1 1 0 01-1 1H4a1 1 0 01-1-1V8zm3 2a1 1 0 100 2h4a1 1 0 100-2H6z" />
            <path v-else-if="g.links[0].icon === 'queue'" d="M3 4a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1V4zm0 5a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1V9zm0 5a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1v-2z" />
            <circle v-else cx="10" cy="10" r="3" />
          </svg>
          <span class="nav-item-text">{{ g.links[0].text }}</span>
        </router-link>

        <!-- 可折叠分组（模块 / 系统） -->
        <div
          v-else-if="g.kind === 'section' || g.kind === 'module'"
          class="nav-section"
          :class="'nav-section--' + g.kind"
        >
          <button
            type="button"
            class="nav-section-head"
            :class="{ 'is-open': expanded[g.id], 'is-active': groupActive(g) }"
            @click="toggleGroup(g.id)"
          >
            <span class="nav-section-title">
              <ModuleIcon
                v-if="g.kind === 'module' && g.navIcon"
                :name="g.navIcon"
                class="nav-item-icon nav-section-mod-icon"
                :style="{ color: moduleColor(g.id.replace(/^mod-/, '')) }"
              />
              <span
                v-else-if="g.kind === 'module'"
                class="ui-module-dot"
                :style="{ '--module-color': moduleColor(g.id.replace(/^mod-/, '')) }"
              />
              {{ g.label }}
            </span>
            <svg class="nav-section-chevron" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
              <path fill-rule="evenodd" d="M5.293 7.293a1 1 0 011.414 0L10 10.586l3.293-3.293a1 1 0 111.414 1.414l-4 4a1 1 0 01-1.414 0l-4-4a1 1 0 010-1.414z" clip-rule="evenodd" />
            </svg>
          </button>
          <div v-show="expanded[g.id]" class="nav-section-body">
            <router-link
              v-for="link in g.links"
              :key="link.to"
              :to="link.to"
              class="nav-item"
              :class="{ 'is-active': linkActive(link) }"
              @click="closeSidebar"
            >
              <svg class="nav-item-icon" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                <path v-if="link.icon === 'dash'" d="M10 2a8 8 0 100 16 8 8 0 000-16zm0 2c-.56 0-1.1.1-1.6.28a1 1 0 00.32 1.96A4 4 0 0110 6zm-4.32.68a1 1 0 00-1.4 0A6 6 0 006 12a1 1 0 001.96-.32A4 4 0 015.68 6.68zm8.64 0a4 4 0 01-2.32 5A1 1 0 0013.4 12a6 6 0 00.92-5.32z" />
                <path v-else-if="link.icon === 'access'" d="M3 4a1 1 0 011-1h12a1 1 0 011 1v2H3V4zm0 4h14v8a1 1 0 01-1 1H4a1 1 0 01-1-1V8zm3 2a1 1 0 100 2h4a1 1 0 100-2H6z" />
                <path v-else-if="link.icon === 'queue'" d="M3 4a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1V4zm0 5a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1V9zm0 5a1 1 0 011-1h12a1 1 0 011 1v2a1 1 0 01-1 1H4a1 1 0 01-1-1v-2z" />
                <path v-else-if="link.icon === 'pypi'" d="M10 1.5c-2.2 0-4 1.34-4 3v2.5h4.5v.75H4.4C2.5 7.75 1 9.4 1 11.5S2.5 15.25 4.4 15.25h2.1V13c0-2.2 1.7-4 3.9-4h4.5V6.5c0-1.66-1.8-3-4-3zm-2.25 2a1 1 0 110 2 1 1 0 010-2zM15.6 8.5h-2.1V11c0 2.2-1.7 4-3.9 4H5v2.5c0 1.66 1.8 3 4 3s4-1.34 4-3v-2.5H8.5V14h5.1c1.9 0 3.4-1.65 3.4-3.75S17.5 8.5 15.6 8.5zM12.25 15.5a1 1 0 110 2 1 1 0 010-2z" />
                <path v-else-if="link.icon === 'npm'" d="M3 4h14v12H3V4zm2.5 2.5v7h3.25v-5.25H10V13.5h4.5v-7H5.5z" />
                <path v-else-if="link.icon === 'docker'" d="M2 11.5h1.6V13H2v-1.5zm2 0h1.6V13H4v-1.5zm2 0h1.6V13H6v-1.5zm2 0h1.6V13H8v-1.5zm-4-2h1.6V11H4V9.5zm2 0h1.6V11H6V9.5zm2 0h1.6V11H8V9.5zm2 0h1.6V11h-1.6V9.5zm-4-2h1.6V9H6V7.5zm2 0h1.6V9H8V7.5zm6.2 2.2c-.2-.15-.5-.1-.7.05-.05.4-.2.75-.45 1.05-.55.7-1.45 1.1-2.55 1.1H2.4c-.35 0-.65.3-.65.65 0 2.1 1.55 4 4.05 4 1.55 0 2.85-.55 3.75-1.5.7.4 1.5.6 2.4.6 2.35 0 3.85-1.55 3.85-3.55 0-.1 0-.2-.02-.3.55-.35.9-.95.9-1.6 0-1.05-.85-1.9-1.88-1.9-.35 0-.7.1-.95.3z" />
                <path v-else-if="link.icon === 'goproxy'" d="M10 2a8 8 0 100 16 8 8 0 000-16zm3.2 5.4l-1.1 4.2c-.15.55-.55.7-1.05.45l-1.55-.9-.75.75c-.1.1-.15.15-.3.15l.1-1.7 3.15-2.8c.15-.1 0-.2-.2-.05l-3.9 2.45-1.65-.5c-.35-.1-.4-.35.1-.55l6.4-2.45c.3-.1.55.15.45.5z" />
                <path v-else-if="link.icon === 'huggingface'" d="M10 2a8 8 0 100 16 8 8 0 000-16zm-2.5 6.2a1.1 1.1 0 110 2.2 1.1 1.1 0 010-2.2zm5 0a1.1 1.1 0 110 2.2 1.1 1.1 0 010-2.2zM6.8 12.2c.55 1.35 1.7 2.2 3.2 2.2s2.65-.85 3.2-2.2c.1-.25-.05-.5-.3-.5H7.1c-.25 0-.4.25-.3.5z" />
                <path v-else-if="link.icon === 'maven'" d="M10 2L3.5 5v4.5c0 4.1 2.8 7.9 6.5 8.9 3.7-1 6.5-4.8 6.5-8.9V5L10 2zm0 2.2l4.5 2.1v3.2c0 2.9-1.8 5.6-4.5 6.5-2.7-.9-4.5-3.6-4.5-6.5V6.3L10 4.2zm-1.5 3.3h3v1.5H9.8L11.5 14h-1.6L8.5 9.5z" />
                <path v-else-if="link.icon === 'pkg'" d="M10 2l7 4v8l-7 4-7-4V6l7-4zm0 2.2L5 7.5v5l5 3 5-3v-5l-5-3.3z" />
                <path v-else-if="link.icon === 'pf'" d="M4 4a2 2 0 012-2h8a2 2 0 012 2v12l-6-3-6 3V4z" />
                <path v-else-if="link.icon === 'plat'" d="M10 2a8 8 0 100 16 8 8 0 000-16zm-1 4h2v4l3 2-1 1.5L9 11V6z" />
                <path v-else-if="link.icon === 'sys'" d="M5 4a2 2 0 012-2h6a2 2 0 012 2v1h1a1 1 0 011 1v2a1 1 0 01-1 1h-1v6a2 2 0 01-2 2H7a2 2 0 01-2-2v-6H4a1 1 0 01-1-1V6a1 1 0 011-1h1V4zm2 0v1h6V4H7zm0 10h6v-6H7v6z" />
                <path v-else-if="link.icon === 'guide'" d="M4 3a1 1 0 011-1h5a1 1 0 011 1v14H5a1 1 0 01-1-1V3zm8 0a1 1 0 011-1h2a2 2 0 012 2v11a1 1 0 01-1 1h-4V3zm-6 3h3v1.5H6V6zm0 3h3v1.5H6V9z" />
                <circle v-else cx="10" cy="10" r="3" />
              </svg>
              <span class="nav-item-text">{{ link.text }}</span>
            </router-link>
          </div>
        </div>
      </template>
    </nav>

    <div class="app-sidebar-foot shrink-0 px-4 py-3">
      <button
        type="button"
        class="flex w-full items-center gap-2 rounded-md px-1 py-0.5 text-left text-xs text-muted
          transition hover:bg-panel hover:text-fg"
        :disabled="applying"
        @click="onVersionClick"
      >
        <span
          class="inline-block h-1.5 w-1.5 shrink-0 rounded-full"
          :class="
            updateStatus?.update_available
              ? 'bg-warn animate-pulse'
              : updateStatus?.error
                ? 'bg-danger'
                : 'bg-ok'
          "
        />
        <span class="truncate font-mono">
          <template v-if="updateStatus?.update_available">
            {{ displayVersion }} · {{ t('common.updateAvailable') }}
          </template>
          <template v-else-if="updateLoading && !updateStatus">
            {{ t('common.updateChecking') }}
          </template>
          <template v-else>
            {{ displayVersion }}
          </template>
        </span>
      </button>
    </div>
  </aside>

  <ModalDialog
    v-model:visible="showResultModal"
    :title="t('common.updateResultTitle')"
    :description="resultText"
    hide-confirm
  />
  <ModalDialog
    v-model:visible="showConfirmModal"
    :title="t('common.updateConfirmTitle')"
    :description="
      t('common.updateConfirmDesc', {
        current: updateStatus?.current || '',
        latest: updateStatus?.latest || '',
      })
    "
    :confirm-text="t('common.updateApply')"
    @confirm="confirmUpgrade"
  />
  <ModalDialog
    :visible="showProgressModal"
    :title="t('common.updateProgressTitle')"
    :hide-confirm="true"
    :cancel-text="
      canCancelUpdate
        ? cancellingUpdate
          ? t('common.updateCancelling')
          : t('common.updateCancel')
        : applying
          ? t('common.updateProgressWaiting')
          : t('common.close')
    "
    @update:visible="onProgressVisible"
  >
    <div class="space-y-3">
      <p class="text-sm text-muted">{{ progressLabel }}</p>
      <div class="h-2 overflow-hidden rounded-full bg-line">
        <div
          class="h-full rounded-full bg-accent transition-[width] duration-300"
          :style="{ width: `${progressPercent}%` }"
        />
      </div>
      <div class="flex justify-between font-mono text-xs text-muted">
        <span>{{ progressPercent }}%</span>
        <span v-if="(liveProgress?.bytes_total || 0) > 0">
          {{ fmtBytes(liveProgress?.bytes_done || 0) }}
          /
          {{ fmtBytes(liveProgress?.bytes_total || 0) }}
        </span>
      </div>
    </div>
  </ModalDialog>

  <div class="flex min-h-screen flex-col lg:pl-[260px]">
    <header
      class="sticky top-0 z-30 flex h-16 shrink-0 items-center gap-3 border-b border-line bg-panel/80
        px-4 backdrop-blur-md sm:px-6"
    >
      <button
        type="button"
        class="ui-btn-ghost !p-2 lg:hidden"
        aria-label="Menu"
        @click="sidebarOpen = !sidebarOpen"
      >
        <svg class="h-6 w-6" viewBox="0 0 20 20" fill="currentColor">
          <path d="M3 5h14M3 10h14M3 15h14" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="round" />
        </svg>
      </button>

      <div class="hidden min-w-0 items-center gap-2 text-sm text-muted lg:flex">
        <span class="ui-metric">
          <svg class="h-3.5 w-3.5 text-accent" viewBox="0 0 20 20" fill="currentColor">
            <path d="M10 3a1 1 0 011 1v7.586l2.293-2.293a1 1 0 111.414 1.414l-4 4a1 1 0 01-1.414 0l-4-4a1 1 0 011.414-1.414L10 11.586V4a1 1 0 011-1z" />
            <path d="M4 15a1 1 0 011-1h10a1 1 0 110 2H5a1 1 0 01-1-1z" />
          </svg>
          <strong>{{ fmtRate(pullBps) }}</strong>
          <span class="opacity-60">({{ fmtBytes(pullTotal) }})</span>
        </span>
        <span class="ui-metric">
          <svg class="h-3.5 w-3.5 text-ok" viewBox="0 0 20 20" fill="currentColor">
            <path d="M10 17a1 1 0 01-1-1V9.414l-2.293 2.293a1 1 0 01-1.414-1.414l4-4a1 1 0 011.414 0l4 4a1 1 0 01-1.414 1.414L10 9.414V16a1 1 0 01-1 1z" />
            <path d="M4 5a1 1 0 011-1h10a1 1 0 110 2H5a1 1 0 01-1-1z" />
          </svg>
          <strong>{{ fmtRate(shareBps) }}</strong>
          <span class="opacity-60">({{ fmtBytes(shareTotal) }})</span>
        </span>
        <span class="ui-metric">
          <svg class="h-3.5 w-3.5 text-muted" viewBox="0 0 20 20" fill="currentColor">
            <path d="M4 4a2 2 0 012-2h8a2 2 0 012 2v12a2 2 0 01-2 2H6a2 2 0 01-2-2V4zm2 1v10h8V5H6zm2 2h4v1.5H8V7zm0 3h4V11.5H8V10z" />
          </svg>
          <strong>{{ fmtBytes(cacheSize) }}</strong>
        </span>
        <span class="ui-metric font-mono">
          <span class="text-accent">P0 {{ p0Active }}</span>
          <span class="opacity-40">·</span>
          <span :class="p1Active ? 'text-ok' : ''">P1 {{ p1Active }}</span>
        </span>
      </div>

      <div class="ml-auto flex items-center gap-1">
        <button
          type="button"
          class="ui-btn-ghost !px-2.5 !py-1.5 text-xs"
          :title="theme.isDark ? t('common.themeLight') : t('common.themeDark')"
          @click="theme.toggle()"
        >
          <svg v-if="theme.isDark" class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor">
            <path d="M10 2a1 1 0 011 1v1a1 1 0 01-2 0V3a1 1 0 011-1zm4.22 1.78a1 1 0 011.41 0l.71.71a1 1 0 01-1.41 1.41l-.71-.71a1 1 0 010-1.41zM18 9a1 1 0 010 2h-1a1 1 0 010-2h1zm-2.78 6.22a1 1 0 00-1.41 0l-.71.71a1 1 0 001.41 1.41l.71-.71a1 1 0 000-1.41zM10 16a1 1 0 011 1v1a1 1 0 01-2 0v-1a1 1 0 011-1zm-5.22-1.78a1 1 0 000-1.41l-.71-.71a1 1 0 00-1.41 1.41l.71.71a1 1 0 001.41 0zM4 11a1 1 0 010-2H3a1 1 0 010 2h1zm2.78-6.22a1 1 0 001.41 0l.71-.71A1 1 0 007.49 2.66l-.71.71a1 1 0 000 1.41zM10 7a3 3 0 100 6 3 3 0 000-6z" />
          </svg>
          <svg v-else class="h-4 w-4" viewBox="0 0 20 20" fill="currentColor">
            <path d="M17.293 13.293A8 8 0 016.707 2.707a8.001 8.001 0 1010.586 10.586z" />
          </svg>
        </button>

        <button type="button" class="ui-btn-ghost !px-2 !py-1 text-xs font-medium" @click="toggleLang">
          {{ locale === 'zh-CN' ? 'EN' : '中' }}
        </button>

        <div ref="userMenuRef" class="relative border-l border-line pl-2">
          <button
            type="button"
            class="ui-btn-ghost flex items-center gap-1.5 !px-2.5 !py-1.5 text-xs"
            :aria-expanded="userMenuOpen"
            @click.stop="toggleUserMenu"
          >
            <span class="max-w-[7rem] truncate font-medium text-fg">
              {{ session.username || t('common.notLoggedIn') }}
            </span>
            <svg
              class="h-3 w-3 shrink-0 text-muted transition-transform"
              :class="{ 'rotate-180': userMenuOpen }"
              viewBox="0 0 20 20"
              fill="currentColor"
            >
              <path fill-rule="evenodd" d="M5.293 7.293a1 1 0 011.414 0L10 10.586l3.293-3.293a1 1 0 111.414 1.414l-4 4a1 1 0 01-1.414 0l-4-4a1 1 0 010-1.414z" clip-rule="evenodd" />
            </svg>
          </button>
          <div
            v-show="userMenuOpen"
            class="user-menu absolute right-0 top-full z-50 mt-1.5 min-w-[10rem] overflow-hidden rounded-lg border border-line bg-panel py-1 shadow-lg"
          >
            <button type="button" class="user-menu-item" @click="goAccount">
              {{ t('nav.account') }}
            </button>
            <button type="button" class="user-menu-item" @click="logout">
              {{ t('common.logout') }}
            </button>
          </div>
        </div>
      </div>
    </header>

    <main class="mx-auto w-full max-w-screen-2xl flex-1 px-5 py-6 sm:px-8 sm:py-8">
      <router-view />
    </main>
  </div>
</template>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: opacity 0.15s ease;
}
.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}
</style>
