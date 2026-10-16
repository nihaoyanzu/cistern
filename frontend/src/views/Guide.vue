<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import copy from 'clipboard-copy'
import ModuleIcon from '@/components/ModuleIcon.vue'
import { api, getToken, type PublicGuide } from '@/api/client'
import { setLocale, type SupportedLocale } from '@/i18n'
import {
  guideFallbackModules,
  MODULE_BY_ID,
  moduleColor,
  orderModulesByNav,
} from '@/modules/registry'
import { GUIDE_SECTIONS } from '@/modules/guide/sections'
import { useThemeStore } from '@/stores/theme'

const { t, locale } = useI18n()
const theme = useThemeStore()
const loading = ref(true)
const guide = ref<PublicGuide | null>(null)
const copied = ref('')
const activeTab = ref('')

const loggedIn = computed(() => !!getToken())

/** 与侧栏菜单同一顺序（MODULES），不跟后端注册序 */
const enabledModules = computed(() =>
  orderModulesByNav((guide.value?.modules || []).filter((m) => m.enabled)),
)
const disabledModules = computed(() =>
  orderModulesByNav((guide.value?.modules || []).filter((m) => !m.enabled)),
)

function moduleIcon(id: string) {
  return MODULE_BY_ID[id]?.navIcon || ''
}

const activeSection = computed(() => GUIDE_SECTIONS[activeTab.value] || null)

/** 用当前打开说明页的 Host + 下载端口（适配 Docker 端口映射）。 */
function accessDownloadURL(proxyPort: string): string {
  const port = String(proxyPort || '18081').trim() || '18081'
  if (typeof location === 'undefined') {
    return `http://127.0.0.1:${port}`
  }
  const scheme = location.protocol === 'https:' ? 'https' : 'http'
  let host = location.hostname || '127.0.0.1'
  if (host === '::1' || host === '[::1]') host = '127.0.0.1'
  const hostPart = host.includes(':') ? `[${host}]` : host
  return `${scheme}://${hostPart}:${port}`
}

const baseURL = computed(() => accessDownloadURL(guide.value?.proxy_port || '18081'))
const indexURL = computed(() => `${baseURL.value}/simple/`)
const registryURL = computed(() => `${baseURL.value}/`)
const displayHost = computed(() => baseURL.value.replace(/^https?:\/\//i, ''))

const trustedHost = computed(() => {
  try {
    return new URL(baseURL.value).hostname
  } catch {
    return 'localhost'
  }
})

function syncTab() {
  const ids = enabledModules.value.map((m) => m.id)
  if (!ids.length) {
    activeTab.value = ''
    return
  }
  if (!ids.includes(activeTab.value)) activeTab.value = ids[0]
}

watch(enabledModules, syncTab, { immediate: true })

function toggleLang() {
  const next: SupportedLocale = locale.value === 'zh-CN' ? 'en' : 'zh-CN'
  setLocale(next)
}

async function copyText(key: string, text: string) {
  try {
    await copy(text)
    copied.value = key
    window.setTimeout(() => {
      if (copied.value === key) copied.value = ''
    }, 1500)
  } catch {
    /* ignore */
  }
}

function moduleTitle(id: string) {
  return MODULE_BY_ID[id]?.guideTitle || id
}

onMounted(async () => {
  loading.value = true
  try {
    guide.value = await api.getPublicGuide()
  } catch {
    guide.value = {
      proxy_port: '18081',
      modules: guideFallbackModules(),
    }
  } finally {
    syncTab()
    loading.value = false
  }
})
</script>

<template>
  <div class="relative min-h-screen overflow-hidden">
    <div class="pointer-events-none absolute inset-0">
      <div class="absolute -left-24 top-10 h-72 w-72 rounded-full bg-accent/15 blur-3xl" />
      <div
        class="absolute -right-20 bottom-0 h-80 w-80 rounded-full blur-3xl"
        style="background: var(--bg-glow-b)"
      />
      <div
        class="absolute left-1/3 top-1/2 h-64 w-64 -translate-x-1/2 rounded-full blur-3xl"
        style="background: var(--bg-glow-c)"
      />
    </div>

    <div class="relative mx-auto max-w-3xl px-4 py-8 sm:px-6 sm:py-10">
      <header class="mb-8 flex flex-wrap items-end justify-between gap-4 border-b border-line pb-6">
        <div class="flex items-center gap-3">
          <img src="/favicon.svg" alt="MirrorHub" class="h-10 w-10" width="40" height="40" />
          <div>
            <div class="text-2xl font-semibold tracking-wide">
              mirror<span class="text-accent">hub</span>
            </div>
            <p class="mt-0.5 text-sm text-muted">{{ t('guide.tagline') }}</p>
          </div>
        </div>
        <div class="flex items-center gap-2">
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
          <button type="button" class="ui-btn-ghost !py-1.5 text-xs" @click="toggleLang">
            {{ locale === 'zh-CN' ? 'EN' : '中文' }}
          </button>
          <RouterLink v-if="loggedIn" class="ui-btn" to="/dashboard">{{ t('nav.dashboard') }}</RouterLink>
          <RouterLink v-else class="ui-btn-primary" to="/login">{{ t('login.loginBtn') }}</RouterLink>
        </div>
      </header>

      <div v-if="loading" class="ui-panel p-6 text-sm text-muted">{{ t('common.loading') }}</div>

      <template v-else>
        <div class="mb-4">
          <div class="ui-label">{{ t('guide.addresses') }}</div>
          <div class="ui-input font-mono text-sm text-muted">{{ displayHost }}</div>
          <p class="mt-2 text-xs text-muted">{{ t('guide.addressesHint') }}</p>
        </div>

        <div v-if="!enabledModules.length" class="ui-panel p-5 text-sm text-muted">
          {{ t('guide.noModule') }}
        </div>

        <template v-else>
          <div class="mb-4 flex flex-wrap gap-1 rounded-xl border border-line bg-panel/60 p-1">
            <button
              v-for="m in enabledModules"
              :key="m.id"
              type="button"
              class="ui-tab inline-flex items-center gap-1.5"
              :class="{ 'ui-tab-active': activeTab === m.id }"
              @click="activeTab = m.id"
            >
              <ModuleIcon
                v-if="moduleIcon(m.id)"
                :name="moduleIcon(m.id)"
                class="h-4 w-4 shrink-0"
                :style="{ color: moduleColor(m.id) }"
              />
              {{ moduleTitle(m.id) }}
            </button>
          </div>

          <component
            :is="activeSection"
            v-if="activeSection"
            :baseURL="baseURL"
            :indexURL="indexURL"
            :registryURL="registryURL"
            :display-host="displayHost"
            :trusted-host="trustedHost"
            :copied="copied"
            @copy="copyText"
          />
        </template>

        <p
          v-if="disabledModules.length"
          class="mt-4 flex flex-wrap items-center gap-x-2 gap-y-1.5 text-xs text-muted"
        >
          <span>{{ t('guide.disabled') }}：</span>
          <span
            v-for="m in disabledModules"
            :key="m.id"
            class="inline-flex items-center gap-1 opacity-70"
          >
            <ModuleIcon
              v-if="moduleIcon(m.id)"
              :name="moduleIcon(m.id)"
              class="h-3.5 w-3.5 shrink-0"
              :style="{ color: moduleColor(m.id) }"
            />
            {{ moduleTitle(m.id) }}
          </span>
        </p>
      </template>
    </div>
  </div>
</template>
