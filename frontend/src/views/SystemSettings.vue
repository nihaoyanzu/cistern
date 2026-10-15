<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import ModalDialog from '@/components/ModalDialog.vue'
import PageHeader from '@/components/PageHeader.vue'
import { api } from '@/api/client'
import { useSessionStore } from '@/stores/session'
import { useToastStore } from '@/stores/toast'
import type { AppConfig, PlatformConfig } from '@/types/api'
import { MODULES } from '@/modules/registry'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const session = useSessionStore()
const toast = useToastStore()
const loading = ref(false)
const saving = ref(false)
const dirty = ref(false)
const pwdSaving = ref(false)
const showClearModal = ref(false)
const showRestartModal = ref(false)
const clearing = ref(false)
const restarting = ref(false)
const tab = ref<'network' | 'rate' | 'concurrency' | 'download' | 'cache' | 'account'>('network')

/** 保存分片时回写全部平台；保留接入字段 */
const platformsSnapshot = ref<Record<string, PlatformConfig>>({})

const form = reactive({
  upstream_proxy: '',
  bandwidth_mbps: 0,
  max_concurrent: 20,
  max_connections: 80,
  windows: [] as { start: string; end: string; bandwidth_mbps: number }[],
  idle_quota_ratio: 0.3,
  on_interactive: 'pause',
  resume_on_idle: true,
  small_file_boost_enabled: true,
  small_file_boost_kb: 512,
  concurrency: 16,
  chunk_size: 5242880,
  min_size: 102400,
  max_size_gb: 100,
  index_ttl_seconds: 604800,
  package_ttl_seconds: 0,
})

type RateWindowRow = { start: string; end: string; bandwidth_mbps: number }

const pwd = reactive({
  old_password: '',
  new_password: '',
  confirm: '',
})

const tabs = computed(() => [
  { id: 'network' as const, label: t('system.tabNetwork') },
  { id: 'rate' as const, label: t('system.tabRate') },
  { id: 'concurrency' as const, label: t('system.tabConcurrency') },
  { id: 'download' as const, label: t('system.tabDownload') },
  { id: 'cache' as const, label: t('system.tabCache') },
  { id: 'account' as const, label: t('system.tabAccount') },
])

const configTabs = computed(() => tab.value !== 'account')

function syncTabFromRoute() {
  const q = String(route.query.tab || '')
  // 制品预取策略已迁至各模块「平台与上游」页
  if (q === 'prefetch') {
    router.replace({ path: '/platform', query: { module: 'pypi' } })
    return
  }
  if (
    q === 'network' ||
    q === 'rate' ||
    q === 'concurrency' ||
    q === 'download' ||
    q === 'cache' ||
    q === 'account'
  ) {
    tab.value = q
  }
}

function setTab(id: typeof tab.value) {
  tab.value = id
  router.replace({ query: { ...route.query, tab: id } })
}

watch(() => route.query.tab, syncTabFromRoute)

function markDirty() {
  dirty.value = true
}

function padHM(hm: string) {
  const m = String(hm || '').trim().match(/^(\d{1,2}):(\d{2})/)
  if (!m) return '00:00'
  return `${String(Number(m[1])).padStart(2, '0')}:${m[2]}`
}

function normalizeWindows(wins: RateWindowRow[]) {
  return wins
    .map((w) => ({
      start: padHM(w.start),
      end: padHM(w.end),
      bandwidth_mbps: Math.max(0, Number(w.bandwidth_mbps) || 0),
    }))
    .filter((w) => w.start && w.end)
}

function addWindow() {
  form.windows.push({ start: '09:00', end: '18:00', bandwidth_mbps: 20 })
  markDirty()
}

function removeWindow(i: number) {
  form.windows.splice(i, 1)
  markDirty()
}

function applyPreset(kind: 'off' | 'always' | 'work_limit' | 'offwork_limit') {
  if (kind === 'off') {
    form.bandwidth_mbps = 0
    form.windows = []
  } else if (kind === 'always') {
    form.bandwidth_mbps = form.bandwidth_mbps > 0 ? form.bandwidth_mbps : 20
    form.windows = []
  } else if (kind === 'work_limit') {
    form.bandwidth_mbps = 0
    form.windows = [{ start: '09:00', end: '18:00', bandwidth_mbps: 20 }]
  } else {
    form.bandwidth_mbps = form.bandwidth_mbps > 0 ? form.bandwidth_mbps : 20
    form.windows = [{ start: '09:00', end: '18:00', bandwidth_mbps: 0 }]
  }
  markDirty()
}

/** 仅更新调度相关字段，保留模块页编辑的制品策略 */
function schedulerPayload(existing: AppConfig['scheduler'] | undefined) {
  const prev = existing?.prefetch
  return {
    prefetch: {
      idle_quota_ratio: form.idle_quota_ratio,
      on_interactive: form.on_interactive,
      resume_on_idle: form.resume_on_idle,
      artifact_mode: prev?.artifact_mode || 'portable',
      extra_wheel_tags: prev?.extra_wheel_tags || [],
      target_python: prev?.target_python || ['3.10', '3.11', '3.12'],
      target_platforms: prev?.target_platforms?.length
        ? [...prev.target_platforms]
        : [prev?.target_platform || 'linux'],
      target_platform: prev?.target_platform || prev?.target_platforms?.[0] || 'linux',
    },
    small_file_boost: {
      enabled: form.small_file_boost_enabled,
      max_size_kb: form.small_file_boost_kb,
    },
  }
}

function rateLimitPayload() {
  return {
    bandwidth_mbps: form.bandwidth_mbps,
    max_concurrent: form.max_concurrent,
    max_connections: form.max_connections,
    windows: normalizeWindows(form.windows),
  }
}

async function load() {
  loading.value = true
  try {
    const cfg: AppConfig = await api.getConfig()
    platformsSnapshot.value = { ...(cfg.platforms || {}) }

    form.upstream_proxy = cfg.server?.upstream_proxy || ''
    const rl = cfg.rate_limit
    form.bandwidth_mbps = rl?.bandwidth_mbps ?? 0
    form.max_concurrent = rl?.max_concurrent ?? 20
    form.max_connections = rl?.max_connections ?? 80
    form.windows = normalizeWindows(
      (rl?.windows || []).map((w) => ({
        start: w.start,
        end: w.end,
        bandwidth_mbps: w.bandwidth_mbps ?? 0,
      })),
    )

    const dlSrc =
      cfg.platforms?.pypi?.download ||
      cfg.platforms?.[MODULES[0].id]?.download ||
      Object.values(cfg.platforms || {})[0]?.download
    form.concurrency = dlSrc?.concurrency ?? 16
    form.chunk_size = dlSrc?.chunk_size ?? 5242880
    form.min_size = dlSrc?.min_size ?? 102400

    if (cfg.cache) {
      form.max_size_gb = cfg.cache.max_size_gb ?? 100
      form.index_ttl_seconds = cfg.cache.index_ttl_seconds ?? 604800
      form.package_ttl_seconds = cfg.cache.package_ttl_seconds ?? 0
    }

    const sch = cfg.scheduler
    if (sch) {
      form.idle_quota_ratio = sch.prefetch?.idle_quota_ratio ?? 0.3
      form.on_interactive = sch.prefetch?.on_interactive || 'pause'
      form.resume_on_idle = sch.prefetch?.resume_on_idle !== false
      form.small_file_boost_enabled = sch.small_file_boost?.enabled !== false
      form.small_file_boost_kb = sch.small_file_boost?.max_size_kb ?? 512
    }
    dirty.value = false
  } catch (e: any) {
    toast.err(e.message || t('system.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    if (tab.value === 'network') {
      await api.putConfig({ upstream_proxy: form.upstream_proxy })
    } else if (tab.value === 'rate' || tab.value === 'concurrency') {
      const cfg = await api.getConfig()
      await api.putConfig({
        rate_limit: rateLimitPayload(),
        scheduler: schedulerPayload(cfg.scheduler),
      })
    } else if (tab.value === 'download') {
      const download = {
        concurrency: form.concurrency,
        chunk_size: form.chunk_size,
        min_size: form.min_size,
      }
      const platforms: Record<string, PlatformConfig> = { ...platformsSnapshot.value }
      for (const desc of MODULES) {
        const prev = platforms[desc.id]
        platforms[desc.id] = {
          enabled: prev?.enabled ?? true,
          upstream: prev?.upstream || desc.defaults.upstream,
          file_upstream: prev?.file_upstream || prev?.upstream || desc.defaults.file_upstream,
          metadata_upstream: prev?.metadata_upstream ?? desc.defaults.metadata_upstream,
          upstream_token: prev?.upstream_token,
          download: { ...download },
        }
      }
      for (const id of Object.keys(platforms)) {
        if (MODULES.some((d) => d.id === id)) continue
        const prev = platforms[id]
        platforms[id] = {
          ...prev,
          download: { ...download },
        }
      }
      await api.putConfig({ platforms })
      platformsSnapshot.value = platforms
    } else if (tab.value === 'cache') {
      await api.putConfig({
        cache: {
          max_size_gb: form.max_size_gb,
          index_ttl_seconds: form.index_ttl_seconds,
          package_ttl_seconds: form.package_ttl_seconds,
        },
      })
    }
    dirty.value = false
    toast.ok(t('system.saved'))
  } catch (e: any) {
    toast.err(e.message || t('system.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function clearCache() {
  clearing.value = true
  try {
    await api.clearCache()
    toast.ok(t('cache.cleared'))
  } catch (e: any) {
    toast.err(e.message || t('cache.clearFailed'))
  } finally {
    clearing.value = false
    showClearModal.value = false
  }
}

async function waitForHealth(timeoutMs = 120_000): Promise<boolean> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 1500))
    try {
      const res = await fetch('/health', { cache: 'no-store' })
      if (res.ok) return true
    } catch {
      /* 重启中 */
    }
  }
  return false
}

async function restartService() {
  restarting.value = true
  showRestartModal.value = false
  try {
    await api.restartService()
    toast.ok(t('platform.restarting'))
    const ok = await waitForHealth()
    if (ok) toast.ok(t('platform.restartOk'))
    else toast.err(t('platform.restartFailed'))
  } catch (e: any) {
    toast.err(e?.message || t('platform.restartFailed'))
  } finally {
    restarting.value = false
  }
}

async function changePassword() {
  if (!pwd.old_password || !pwd.new_password) {
    toast.err(t('system.pwdRequired'))
    return
  }
  if (pwd.new_password !== pwd.confirm) {
    toast.err(t('system.pwdMismatch'))
    return
  }
  pwdSaving.value = true
  try {
    await api.changePassword(pwd.old_password, pwd.new_password)
    pwd.old_password = ''
    pwd.new_password = ''
    pwd.confirm = ''
    toast.ok(t('system.pwdChanged'))
  } catch (e: any) {
    toast.err(e.message || t('system.pwdFailed'))
  } finally {
    pwdSaving.value = false
  }
}

async function logout() {
  try {
    await api.logout()
  } catch {
    /* ignore */
  }
  session.reset()
  toast.info(t('toast.loggedOut'))
  await router.replace('/login')
}

onMounted(() => {
  syncTabFromRoute()
  load()
})
</script>

<template>
  <div>
    <PageHeader :title="t('system.title')">
      <template #actions>
        <template v-if="configTabs">
          <span v-if="dirty" class="ui-badge-warn">{{ t('common.unsaved') }}</span>
          <button class="ui-btn" :disabled="loading || saving" @click="load">{{ t('system.reload') }}</button>
          <button class="ui-btn-primary" :disabled="loading || saving" @click="save">
            {{ saving ? t('system.saving') : t('common.save') }}
          </button>
        </template>
        <template v-else>
          <button type="button" class="ui-btn" @click="logout">{{ t('common.logout') }}</button>
          <button
            type="button"
            class="ui-btn-primary"
            :disabled="pwdSaving"
            @click="changePassword"
          >
            {{ pwdSaving ? t('system.pwdSaving') : t('system.changePassword') }}
          </button>
        </template>
      </template>
    </PageHeader>

    <div class="mb-5 flex flex-wrap gap-1 rounded-xl border border-line bg-panel/70 p-1">
      <button
        v-for="item in tabs"
        :key="item.id"
        type="button"
        class="ui-tab"
        :class="{ 'ui-tab-active': tab === item.id }"
        @click="setTab(item.id)"
      >
        {{ item.label }}
      </button>
    </div>

    <section
      v-if="tab === 'network'"
      class="ui-panel space-y-5 p-5"
      @input="markDirty"
      @change="markDirty"
    >
      <form class="grid gap-4 sm:grid-cols-2" autocomplete="off" @submit.prevent>
        <div class="sm:col-span-2">
          <label class="ui-label">{{ t('system.upstreamProxy') }}</label>
          <input
            v-model="form.upstream_proxy"
            class="ui-input"
            name="mirrorhub-upstream-proxy"
            type="text"
            inputmode="url"
            autocomplete="off"
            data-1p-ignore
            data-lpignore="true"
            placeholder="socks5://127.0.0.1:1080"
          />
        </div>
      </form>
    </section>

    <section
      v-else-if="tab === 'rate'"
      class="ui-panel space-y-6 p-5"
      @input="markDirty"
      @change="markDirty"
    >
      <div class="flex flex-wrap items-center gap-2">
        <span class="shrink-0 text-xs font-medium text-muted">{{ t('system.presets') }}</span>
        <button type="button" class="ui-btn-ghost !py-1 text-xs" @click="applyPreset('off')">
          {{ t('system.presetOff') }}
        </button>
        <button type="button" class="ui-btn-ghost !py-1 text-xs" @click="applyPreset('always')">
          {{ t('system.presetAlways') }}
        </button>
        <button type="button" class="ui-btn-ghost !py-1 text-xs" @click="applyPreset('work_limit')">
          {{ t('system.presetWorkLimit') }}
        </button>
        <button type="button" class="ui-btn-ghost !py-1 text-xs" @click="applyPreset('offwork_limit')">
          {{ t('system.presetOffworkLimit') }}
        </button>
      </div>

      <div>
        <h3 class="ui-section-title">{{ t('system.sectionDefaultBandwidth') }}</h3>
        <div class="grid gap-4 sm:grid-cols-2">
          <div>
            <label class="ui-label">{{ t('system.defaultMbps') }}</label>
            <input
              v-model.number="form.bandwidth_mbps"
              type="number"
              min="0"
              step="0.1"
              class="ui-input"
              placeholder="0"
            />
            <p class="mt-1 text-xs text-muted">{{ t('system.defaultMbpsHint') }}</p>
          </div>
        </div>
      </div>

      <div>
        <div class="mb-3 flex items-end justify-between gap-3">
          <h3 class="ui-section-title !mb-0">{{ t('system.sectionRateWindows') }}</h3>
          <button type="button" class="ui-btn !py-1.5 text-xs" @click="addWindow">
            {{ t('system.addWindow') }}
          </button>
        </div>

        <div v-if="!form.windows.length" class="rounded-md border border-dashed border-line px-4 py-6 text-center text-sm text-muted">
          {{ t('system.noWindows') }}
        </div>

        <div v-else class="space-y-2">
          <div
            v-for="(w, i) in form.windows"
            :key="i"
            class="grid grid-cols-1 items-end gap-3 rounded-md border border-line px-3 py-3 sm:grid-cols-[1fr_auto_1fr_1fr_auto]"
          >
            <div>
              <label class="ui-label">{{ t('system.windowStart') }}</label>
              <input v-model="w.start" type="time" class="ui-input" />
            </div>
            <span class="hidden pb-2.5 text-muted sm:block">→</span>
            <div>
              <label class="ui-label">{{ t('system.windowEnd') }}</label>
              <input v-model="w.end" type="time" class="ui-input" />
            </div>
            <div>
              <label class="ui-label">{{ t('system.windowMbps') }}</label>
              <input
                v-model.number="w.bandwidth_mbps"
                type="number"
                min="0"
                step="0.1"
                class="ui-input"
              />
            </div>
            <button
              type="button"
              class="ui-btn-ghost !px-2 !py-2 text-xs text-muted hover:text-danger"
              :title="t('common.delete')"
              @click="removeWindow(i)"
            >
              {{ t('common.delete') }}
            </button>
          </div>
        </div>
      </div>
    </section>

    <section
      v-else-if="tab === 'concurrency'"
      class="ui-panel space-y-6 p-5"
      @input="markDirty"
      @change="markDirty"
    >
      <div>
        <h3 class="ui-section-title">{{ t('system.sectionConcurrency') }}</h3>
        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label class="ui-label">{{ t('platform.concurrency') }}</label>
            <input v-model.number="form.max_concurrent" type="number" min="1" class="ui-input" />
            <p class="mt-1 text-xs text-muted">{{ t('platform.concurrencyHint') }}</p>
          </div>
          <div>
            <label class="ui-label">{{ t('platform.rangeConnections') }}</label>
            <input v-model.number="form.max_connections" type="number" min="1" class="ui-input" />
          </div>
          <div>
            <label class="ui-label">{{ t('platform.idleQuota') }}</label>
            <input
              v-model.number="form.idle_quota_ratio"
              type="number"
              min="0.05"
              max="1"
              step="0.05"
              class="ui-input"
            />
          </div>
        </div>
      </div>

      <div>
        <h3 class="ui-section-title">{{ t('platform.sectionInteractive') }}</h3>
        <div class="grid gap-4 sm:grid-cols-3">
          <div>
            <label class="ui-label">{{ t('platform.onInteractive') }}</label>
            <select v-model="form.on_interactive" class="ui-input">
              <option value="pause">{{ t('platform.pause') }}</option>
              <option value="continue">{{ t('platform.continueAction') }}</option>
            </select>
          </div>
          <div>
            <label class="ui-label">{{ t('platform.smallFileKB') }}</label>
            <input v-model.number="form.small_file_boost_kb" type="number" min="1" class="ui-input" />
          </div>
          <div class="flex flex-col justify-end gap-2 pb-1">
            <label class="flex items-center gap-2 text-sm">
              <input v-model="form.resume_on_idle" type="checkbox" class="accent-accent" />
              {{ t('platform.resumeOnIdle') }}
            </label>
            <label class="flex items-center gap-2 text-sm">
              <input v-model="form.small_file_boost_enabled" type="checkbox" class="accent-accent" />
              {{ t('platform.smallFileBoost') }}
            </label>
          </div>
        </div>
      </div>
    </section>

    <section
      v-else-if="tab === 'download'"
      class="ui-panel space-y-5 p-5"
      @input="markDirty"
      @change="markDirty"
    >
      <p class="text-xs text-muted">{{ t('system.downloadHint') }}</p>
      <h3 class="ui-section-title">{{ t('platform.sectionDownload') }}</h3>
      <div class="grid gap-4 sm:grid-cols-3">
        <div>
          <label class="ui-label">{{ t('platform.chunkConcurrency') }}</label>
          <input v-model.number="form.concurrency" type="number" min="1" class="ui-input" />
          <p class="mt-1 text-xs text-muted">{{ t('platform.chunkConcurrencyHint') }}</p>
        </div>
        <div>
          <label class="ui-label">{{ t('platform.chunkSize') }}</label>
          <input v-model.number="form.chunk_size" type="number" min="1" class="ui-input" />
        </div>
        <div>
          <label class="ui-label">{{ t('platform.parallelMinSize') }}</label>
          <input v-model.number="form.min_size" type="number" min="0" class="ui-input" />
        </div>
      </div>
    </section>

    <section
      v-else-if="tab === 'cache'"
      class="ui-panel space-y-5 p-5"
      @input="markDirty"
      @change="markDirty"
    >
      <p class="text-xs text-muted">{{ t('system.cacheHint') }}</p>
      <div class="grid gap-4 sm:grid-cols-3">
        <div>
          <label class="ui-label">{{ t('platform.cacheGB') }}</label>
          <input v-model.number="form.max_size_gb" type="number" min="1" class="ui-input" />
        </div>
        <div>
          <label class="ui-label">{{ t('platform.indexTTL') }}</label>
          <input v-model.number="form.index_ttl_seconds" type="number" min="1" class="ui-input" />
          <p class="mt-1 text-xs text-muted">{{ t('platform.indexTTLHint') }}</p>
        </div>
        <div>
          <label class="ui-label">{{ t('platform.packageTTL') }}</label>
          <input v-model.number="form.package_ttl_seconds" type="number" min="0" class="ui-input" />
          <p class="mt-1 text-xs text-muted">{{ t('platform.packageTTLHint') }}</p>
        </div>
      </div>
      <div class="border-t border-danger/20 pt-5">
        <h3 class="mb-4 text-sm font-medium text-danger">{{ t('platform.dangerZone') }}</h3>
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="rounded-md border border-line/80 p-4">
            <p class="mb-3 text-xs text-muted">{{ t('platform.clearCacheDesc') }}</p>
            <button class="ui-btn-danger" :disabled="clearing || restarting" @click="showClearModal = true">
              {{ t('platform.clearCache') }}
            </button>
          </div>
          <div class="rounded-md border border-line/80 p-4">
            <p class="mb-3 text-xs text-muted">{{ t('platform.restartServiceDesc') }}</p>
            <button class="ui-btn-danger" :disabled="clearing || restarting" @click="showRestartModal = true">
              {{ restarting ? t('platform.restarting') : t('platform.restartService') }}
            </button>
          </div>
        </div>
      </div>
    </section>

    <section v-else-if="tab === 'account'" class="ui-panel space-y-5 p-5">
      <p class="text-xs text-muted">
        {{ t('system.accountHint', { name: session.username || '—' }) }}
      </p>
      <form class="grid gap-4 sm:grid-cols-3" autocomplete="off" @submit.prevent="changePassword">
        <div>
          <label class="ui-label">{{ t('system.oldPassword') }}</label>
          <input
            v-model="pwd.old_password"
            type="password"
            class="ui-input"
            name="mirrorhub-current-password"
            autocomplete="current-password"
          />
        </div>
        <div>
          <label class="ui-label">{{ t('system.newPassword') }}</label>
          <input
            v-model="pwd.new_password"
            type="password"
            class="ui-input"
            name="mirrorhub-new-password"
            autocomplete="new-password"
          />
        </div>
        <div>
          <label class="ui-label">{{ t('system.confirmPassword') }}</label>
          <input
            v-model="pwd.confirm"
            type="password"
            class="ui-input"
            name="mirrorhub-confirm-password"
            autocomplete="new-password"
          />
        </div>
      </form>
    </section>

    <ModalDialog
      v-model:visible="showClearModal"
      :title="t('platform.clearCache')"
      :description="t('platform.clearCacheConfirm')"
      danger
      @cancel="showClearModal = false"
      @confirm="clearCache"
    />
    <ModalDialog
      v-model:visible="showRestartModal"
      :title="t('platform.restartService')"
      :description="t('platform.restartServiceConfirm')"
      danger
      @cancel="showRestartModal = false"
      @confirm="restartService"
    />
  </div>
</template>
