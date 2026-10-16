<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import ModalDialog from '@/components/ModalDialog.vue'
import ModuleIcon from '@/components/ModuleIcon.vue'
import { api } from '@/api/client'
import { useToastStore } from '@/stores/toast'
import { useModulesStore } from '@/stores/modules'
import type { AccessTestModule, AppConfig, PlatformConfig } from '@/types/api'
import { isKnownModule, MODULE_BY_ID, MODULES, moduleColor } from '@/modules/registry'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToastStore()
const modulesStore = useModulesStore()
const loading = ref(false)
const saving = ref(false)
const testing = ref(false)
const dirty = ref(false)
const moduleId = ref(MODULES[0].id)
const testModalVisible = ref(false)
const testModules = ref<AccessTestModule[] | null>(null)
const testPhase = ref<'idle' | 'running' | 'done'>('idle')

const form = reactive({
  enabled: true,
  upstream: MODULES[0].defaults.upstream,
  file_upstream: MODULES[0].defaults.file_upstream,
  metadata_upstream: MODULES[0].defaults.metadata_upstream || '',
  upstream_token: '',
})

/** 系统级字段：探测时只读带入，不在本页编辑 */
const systemNet = reactive({
  upstream_proxy: '',
})

/** 预取制品策略（scheduler.prefetch 中的平台相关字段，按模块页编辑） */
const prefetchForm = reactive({
  artifact_mode: 'portable',
  extra_wheel_tags: '',
  target_python: '3.10\n3.11\n3.12',
  target_platforms: ['linux'] as string[],
})

type PlatformDraft = {
  enabled: boolean
  upstream: string
  file_upstream: string
  metadata_upstream: string
  upstream_token: string
}

/** 切换模块时暂存各平台表单，避免丢失未保存编辑以外的已加载值 */
const platformDrafts = reactive<Record<string, PlatformDraft>>({})

const platformOptions = [
  { id: 'linux', labelKey: 'platform.platLinux' },
  { id: 'linux-arm', labelKey: 'platform.platLinuxArm' },
  { id: 'win32', labelKey: 'platform.platWin' },
  { id: 'win-arm', labelKey: 'platform.platWinArm' },
  { id: 'darwin', labelKey: 'platform.platDarwin' },
  { id: 'darwin-arm', labelKey: 'platform.platDarwinArm' },
] as const

const currentDesc = computed(() => MODULE_BY_ID[moduleId.value] || MODULES[0])
const moduleOptions = MODULES.map((d) => ({ id: d.id, labelKey: d.labelKey, navIcon: d.navIcon }))
const enableLabel = computed(() => t(currentDesc.value.enableLabelKey))
const thirdField = computed(() => currentDesc.value.fields.thirdField)
const prefetchUI = computed(() => currentDesc.value.prefetchUI)
const unifiedUpstream = computed(() => !!currentDesc.value.fields.unifiedUpstream)
/** 仅有可编辑控件时展示独立预取区；纯说明并入上方上游区 */
const showPrefetchSection = computed(
  () =>
    prefetchUI.value === 'pypiWheel' ||
    prefetchUI.value === 'arch' ||
    moduleId.value === 'maven',
)

/** 旧链接 /platform?tab=download|cache → 系统设置；tab=prefetch → PyPI 模块 */
function redirectLegacyTabs() {
  const q = String(route.query.tab || '')
  if (q === 'download' || q === 'cache') {
    router.replace({ path: '/settings', query: { tab: q } })
    return true
  }
  if (q === 'prefetch') {
    router.replace({ path: '/platform', query: { module: 'pypi' } })
    return true
  }
  if (q === 'rate') {
    router.replace({ path: '/settings', query: { tab: 'download' } })
    return true
  }
  return false
}

function syncModuleFromRoute() {
  if (redirectLegacyTabs()) return
  const raw = route.query.module
  const mod = String(Array.isArray(raw) ? raw[0] : raw || '')
  if (isKnownModule(mod) && mod !== moduleId.value) {
    snapshotCurrentPlatform()
    moduleId.value = mod
    applyPlatformDraft(moduleId.value)
  }
}

function ensureDraft(id: string) {
  if (platformDrafts[id]) return
  const desc = MODULE_BY_ID[id]
  if (!desc) return
  platformDrafts[id] = draftFromConfig(
    undefined,
    desc.defaults,
    desc.fields.persistUpstreamToken,
  )
}

function snapshotCurrentPlatform() {
  const desc = MODULE_BY_ID[moduleId.value]
  const fileUp = desc?.fields.unifiedUpstream ? form.upstream : form.file_upstream
  platformDrafts[moduleId.value] = {
    enabled: form.enabled,
    upstream: form.upstream,
    file_upstream: fileUp,
    metadata_upstream: form.metadata_upstream,
    upstream_token: form.upstream_token,
  }
}

function applyPlatformDraft(id: string) {
  ensureDraft(id)
  const d = platformDrafts[id]
  if (!d) return
  form.enabled = d.enabled
  form.upstream = d.upstream
  form.file_upstream = d.file_upstream
  form.metadata_upstream = d.metadata_upstream
  form.upstream_token = d.upstream_token || ''
}

function setModule(id: string) {
  if (id === moduleId.value || !isKnownModule(id)) return
  snapshotCurrentPlatform()
  moduleId.value = id
  applyPlatformDraft(id)
  // 只保留 module，避免残留 tab=prefetch 等把切页又重定向回 PyPI
  router.replace({ path: '/platform', query: { module: id } })
}

watch(() => route.query.tab, syncModuleFromRoute)
watch(() => route.query.module, syncModuleFromRoute)

function markDirty() {
  dirty.value = true
}

function normalizePlatforms(raw: unknown, legacy?: string): string[] {
  const allowed = new Set<string>(platformOptions.map((o) => o.id))
  const out: string[] = []
  const push = (v: string) => {
    const id = String(v || '').trim().toLowerCase()
    if (!id || !allowed.has(id)) return
    if (!out.includes(id)) out.push(id)
  }
  if (Array.isArray(raw)) {
    for (const v of raw) push(String(v))
  }
  if (out.length === 0 && legacy) push(legacy)
  return out.length ? out : ['linux']
}

function togglePlatform(id: string) {
  const i = prefetchForm.target_platforms.indexOf(id)
  if (i >= 0) {
    if (prefetchForm.target_platforms.length <= 1) return
    prefetchForm.target_platforms.splice(i, 1)
  } else {
    prefetchForm.target_platforms.push(id)
  }
  markDirty()
}

function draftFromConfig(
  cfg: PlatformConfig | undefined,
  defaults: (typeof MODULES)[0]['defaults'],
  persistToken: boolean,
): PlatformDraft {
  // 有配置时保留空串（表示走 Match 默认上游）；无配置时用模块 defaults 填表
  return {
    enabled: !!cfg?.enabled,
    upstream: cfg ? String(cfg.upstream ?? '') : defaults.upstream,
    file_upstream: cfg
      ? String(cfg.file_upstream ?? cfg.upstream ?? '')
      : defaults.file_upstream,
    metadata_upstream: cfg
      ? String(cfg.metadata_upstream ?? '')
      : defaults.metadata_upstream || '',
    upstream_token: persistToken ? (cfg?.upstream_token || '') : '',
  }
}

function applyPrefetchFromConfig(sch: AppConfig['scheduler'] | undefined) {
  const pf = sch?.prefetch
  prefetchForm.artifact_mode = pf?.artifact_mode || 'portable'
  prefetchForm.extra_wheel_tags = (pf?.extra_wheel_tags || []).join('\n')
  const tp = pf?.target_python
  prefetchForm.target_python = Array.isArray(tp) ? tp.join('\n') : tp || '3.10\n3.11\n3.12'
  prefetchForm.target_platforms = normalizePlatforms(pf?.target_platforms, pf?.target_platform)
}

function linesToList(raw: string): string[] {
  return raw
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

async function load() {
  loading.value = true
  try {
    const cfg: AppConfig = await api.getConfig()
    systemNet.upstream_proxy = cfg.server?.upstream_proxy || ''

    for (const d of MODULES) {
      platformDrafts[d.id] = draftFromConfig(
        cfg.platforms?.[d.id],
        d.defaults,
        d.fields.persistUpstreamToken,
      )
    }
    applyPlatformDraft(moduleId.value)
    applyPrefetchFromConfig(cfg.scheduler)
    dirty.value = false
  } catch (e: any) {
    toast.err(e.message || t('platform.loadFailed'))
  } finally {
    loading.value = false
  }
}

async function save() {
  saving.value = true
  try {
    snapshotCurrentPlatform()
    const platforms: Record<string, PlatformConfig> = {}
    const cfg = await api.getConfig()
    for (const desc of MODULES) {
      const d = platformDrafts[desc.id]
      if (!d) continue
      const prev = cfg.platforms?.[desc.id]
      const row: PlatformConfig = {
        enabled: d.enabled,
        upstream: d.upstream,
        file_upstream: desc.fields.unifiedUpstream ? d.upstream : d.file_upstream,
        // 统一上游且无第三字段时，默认 metadata 与 upstream 同步（Go SumDB 等）；
        // Docker 等 syncMetadataWithUpstream=false 时保存空串，由后端自动发现 Auth。
        metadata_upstream:
          desc.fields.unifiedUpstream && desc.fields.thirdField === 'none'
            ? desc.fields.syncMetadataWithUpstream === false
              ? ''
              : d.upstream
            : d.metadata_upstream,
        download: prev?.download || {
          concurrency: 16,
          chunk_size: 5242880,
          min_size: 102400,
        },
      }
      if (desc.fields.persistUpstreamToken) {
        row.upstream_token = d.upstream_token || ''
      }
      platforms[desc.id] = row
    }

    const prevSch = cfg.scheduler
    const prevPf = prevSch?.prefetch
    const platformsPayload = platforms
    const schedulerPayload = {
      prefetch: {
        idle_quota_ratio: prevPf?.idle_quota_ratio ?? 0.3,
        on_interactive: prevPf?.on_interactive || 'pause',
        resume_on_idle: prevPf?.resume_on_idle !== false,
        artifact_mode: prefetchForm.artifact_mode,
        extra_wheel_tags: linesToList(prefetchForm.extra_wheel_tags),
        target_python: linesToList(prefetchForm.target_python),
        target_platforms: prefetchForm.target_platforms.length
          ? [...prefetchForm.target_platforms]
          : ['linux'],
        target_platform: prefetchForm.target_platforms[0] || 'linux',
      },
      small_file_boost: prevSch?.small_file_boost || {
        enabled: true,
        max_size_kb: 512,
      },
    }

    await api.putConfig({ platforms: platformsPayload, scheduler: schedulerPayload })
    await modulesStore.refresh()
    dirty.value = false
    toast.ok(t('platform.saved'))
  } catch (e: any) {
    toast.err(e.message || t('platform.saveFailed'))
  } finally {
    saving.value = false
  }
}

function moduleLabel(id: string) {
  const desc = MODULE_BY_ID[id]
  return desc ? t(desc.labelKey) : id
}

function collectEnabledPlatformDrafts() {
  snapshotCurrentPlatform()
  const platforms: Record<
    string,
    { enabled: boolean; upstream: string; file_upstream: string; metadata_upstream: string }
  > = {}
  for (const desc of MODULES) {
    const d = platformDrafts[desc.id]
    if (!d?.enabled) continue
    platforms[desc.id] = {
      enabled: true,
      upstream: d.upstream,
      file_upstream: desc.fields.unifiedUpstream ? d.upstream : d.file_upstream,
      metadata_upstream:
        desc.fields.unifiedUpstream && desc.fields.thirdField === 'none'
          ? desc.fields.syncMetadataWithUpstream === false
            ? ''
            : d.upstream
          : d.metadata_upstream,
    }
  }
  return platforms
}

async function testAccess() {
  const platforms = collectEnabledPlatformDrafts()
  const ids = Object.keys(platforms)
  if (!ids.length) {
    toast.err(t('platform.testNoEnabled'))
    return
  }

  testing.value = true
  testPhase.value = 'running'
  // 先列出将测模块，避免长时间空白
  testModules.value = ids.map((id) => ({
    id,
    ok: false,
    checks: [
      {
        name: 'pending',
        ok: false,
        skipped: true,
        detail: t('platform.testPending'),
        ms: 0,
      },
    ],
  }))
  testModalVisible.value = true

  try {
    const res = await api.testAccess({
      upstream_proxy: systemNet.upstream_proxy || undefined,
      platforms,
    })
    const got = res.modules?.length
      ? res.modules
      : res.checks?.length
        ? [{ id: ids[0] || 'unknown', ok: !!res.ok, checks: res.checks }]
        : []
    // 按发起顺序合并，未返回的模块标为无结果
    const byId = new Map(got.map((m) => [m.id, m]))
    testModules.value = ids.map((id) => {
      const m = byId.get(id)
      if (m) return m
      return {
        id,
        ok: false,
        checks: [
          {
            name: 'upstream',
            ok: false,
            detail: t('platform.testNoResult'),
            ms: 0,
          },
        ],
      }
    })
    // 后端额外返回但前端未发起的（极少），追加展示
    for (const m of got) {
      if (!ids.includes(m.id)) testModules.value.push(m)
    }
    testPhase.value = 'done'
    if (res.ok) toast.ok(t('platform.testOk'))
    else toast.err(t('platform.testFailed'))
  } catch (e: any) {
    testModules.value = ids.map((id) => ({
      id,
      ok: false,
      checks: [
        {
          name: 'upstream',
          ok: false,
          detail: e.message || t('platform.testFailed'),
          ms: 0,
        },
      ],
    }))
    testPhase.value = 'done'
    toast.err(e.message || t('platform.testFailed'))
  } finally {
    testing.value = false
  }
}

function checkLabel(name: string) {
  if (name === 'pending') return t('platform.testPending')
  const map: Record<string, string> = {
    upstream: t('platform.sectionUpstream'),
    download: t('platform.testDownload'),
    metadata: t('platform.testMetadata'),
    manifest: t('platform.testManifest'),
    index_upstream: t('platform.indexUpstream'),
    file_upstream: t('platform.fileUpstream'),
    metadata_upstream: t('platform.metadataUpstream'),
    upstream_proxy: t('system.upstreamProxy'),
  }
  return map[name] || name
}

onMounted(() => {
  redirectLegacyTabs()
  syncModuleFromRoute()
  load()
})
</script>

<template>
  <div>
    <PageHeader :title="t('platform.title')">
      <template #actions>
        <span v-if="dirty" class="ui-badge-warn">{{ t('common.unsaved') }}</span>
        <button class="ui-btn" :disabled="loading || saving || testing" @click="testAccess">
          {{ testing ? t('platform.testing') : t('platform.testAccess') }}
        </button>
        <button class="ui-btn" :disabled="loading || saving" @click="load">{{ t('platform.reload') }}</button>
        <button class="ui-btn-primary" :disabled="loading || saving" @click="save">
          {{ saving ? t('platform.saving') : t('common.save') }}
        </button>
      </template>
    </PageHeader>

    <div class="mb-5 flex flex-wrap gap-1 rounded-xl border border-line bg-panel/70 p-1">
      <button
        v-for="item in moduleOptions"
        :key="item.id"
        type="button"
        class="ui-tab inline-flex items-center"
        :class="{ 'ui-tab-active': moduleId === item.id }"
        @click="setModule(item.id)"
      >
        <ModuleIcon
          :name="item.navIcon"
          class="mr-1.5 h-4 w-4 shrink-0"
          :style="{ color: moduleColor(item.id) }"
        />
        {{ t(item.labelKey) }}
      </button>
    </div>

    <div class="space-y-4" @input="markDirty" @change="markDirty">
      <section :key="moduleId" class="ui-panel p-5">
        <div class="mb-5">
          <label class="flex items-center gap-3 text-sm font-medium text-fg">
            <span class="ui-switch">
              <input v-model="form.enabled" type="checkbox" />
              <span class="ui-switch-track" aria-hidden="true" />
              <span class="ui-switch-thumb" aria-hidden="true" />
            </span>
            {{ enableLabel }}
          </label>
        </div>

        <h3 class="ui-section-title">{{ t('platform.sectionUpstream') }}</h3>
        <p v-if="currentDesc.hints.upstreamHintKey" class="mb-3 text-xs text-muted">
          {{ t(currentDesc.hints.upstreamHintKey) }}
        </p>
        <p
          v-if="!showPrefetchSection && currentDesc.hints.prefetchHintKey"
          class="mb-3 text-xs text-muted"
        >
          {{ t(currentDesc.hints.prefetchHintKey) }}
        </p>
        <div class="grid gap-4 sm:grid-cols-2">
          <div :class="unifiedUpstream ? 'sm:col-span-2' : ''">
            <label class="ui-label">{{ t(currentDesc.labels.upstreamKey) }}</label>
            <input
              v-model="form.upstream"
              class="ui-input"
              :placeholder="currentDesc.defaults.upstream"
            />
          </div>
          <div v-if="!unifiedUpstream">
            <label class="ui-label">{{ t(currentDesc.labels.fileUpstreamKey) }}</label>
            <input
              v-model="form.file_upstream"
              class="ui-input"
              :placeholder="currentDesc.defaults.file_upstream"
            />
          </div>

          <div v-if="thirdField === 'metadata'" class="sm:col-span-2">
            <label class="ui-label">
              {{ t(currentDesc.labels.thirdFieldKey || 'platform.metadataUpstream') }}
              <span
                v-if="currentDesc.hints.thirdFieldHintKey"
                class="group relative ml-1 inline-flex cursor-help items-center"
              >
                <svg class="h-3.5 w-3.5 text-muted/60" viewBox="0 0 20 20" fill="currentColor">
                  <path fill-rule="evenodd" d="M18 10a8 8 0 11-16 0 8 8 0 0116 0zm-7-4a1 1 0 11-2 0 1 1 0 012 0zM9 9a1 1 0 000 2v3a1 1 0 001 1h1a1 1 0 100-2v-3a1 1 0 00-1-1H9z" clip-rule="evenodd" />
                </svg>
                <span class="pointer-events-none absolute bottom-full left-1/2 z-50 mb-2 w-64 -translate-x-1/2 rounded-lg border border-line bg-panel px-3 py-2 text-xs text-muted opacity-0 shadow-lg transition-opacity group-hover:opacity-100">
                  {{ t(currentDesc.hints.thirdFieldHintKey) }}
                </span>
              </span>
            </label>
            <input
              v-model="form.metadata_upstream"
              class="ui-input"
              :placeholder="currentDesc.hints.thirdFieldPlaceholder || ''"
            />
          </div>

          <div v-if="thirdField === 'auth' || thirdField === 'sumdb'" class="sm:col-span-2">
            <label class="ui-label">{{ t(currentDesc.labels.thirdFieldKey || '') }}</label>
            <input
              v-model="form.metadata_upstream"
              class="ui-input"
              :placeholder="currentDesc.hints.thirdFieldPlaceholder || ''"
            />
            <p v-if="currentDesc.hints.thirdFieldHintKey" class="mt-1 text-xs text-muted">
              {{ t(currentDesc.hints.thirdFieldHintKey) }}
            </p>
          </div>

          <div v-if="thirdField === 'token'" class="sm:col-span-2">
            <label class="ui-label">{{ t(currentDesc.labels.thirdFieldKey || 'platform.upstreamToken') }}</label>
            <input
              v-model="form.upstream_token"
              type="password"
              autocomplete="off"
              class="ui-input"
              :placeholder="t('platform.upstreamTokenPlaceholder')"
            />
            <p v-if="currentDesc.hints.thirdFieldHintKey" class="mt-1 text-xs text-muted">
              {{ t(currentDesc.hints.thirdFieldHintKey) }}
            </p>
          </div>
        </div>

      </section>

      <section v-if="showPrefetchSection" class="ui-panel space-y-5 p-5">
        <div>
          <h3 class="ui-section-title">{{ t('platform.sectionPrefetch') }}</h3>
          <p v-if="currentDesc.hints.prefetchHintKey" class="text-xs text-muted">
            {{ t(currentDesc.hints.prefetchHintKey) }}
          </p>
        </div>

        <div v-if="prefetchUI === 'pypiWheel'" class="grid gap-4 sm:grid-cols-3">
          <div>
            <label class="ui-label">{{ t('platform.artifactMode') }}</label>
            <select v-model="prefetchForm.artifact_mode" class="ui-input">
              <option value="portable">portable</option>
              <option value="all">all</option>
            </select>
          </div>
          <div class="sm:col-span-2">
            <label class="ui-label">{{ t('platform.minPython') }}</label>
            <textarea
              v-model="prefetchForm.target_python"
              rows="2"
              class="ui-input font-mono text-sm"
              placeholder="3.10&#10;3.11&#10;3.12"
            />
          </div>
          <div class="sm:col-span-3">
            <label class="ui-label">{{ t('platform.targetPlatform') }}</label>
            <p class="mb-2 text-xs text-muted">{{ t('platform.targetPlatformHint') }}</p>
            <div class="flex flex-wrap gap-2">
              <button
                v-for="opt in platformOptions"
                :key="opt.id"
                type="button"
                class="ui-chip"
                :class="{ 'ui-chip-active': prefetchForm.target_platforms.includes(opt.id) }"
                @click="togglePlatform(opt.id)"
              >
                {{ t(opt.labelKey) }}
              </button>
            </div>
          </div>
          <div class="sm:col-span-3">
            <label class="ui-label">{{ t('platform.extraWheelTags') }}</label>
            <textarea
              v-model="prefetchForm.extra_wheel_tags"
              rows="2"
              class="ui-input font-mono text-sm"
              placeholder="manylinux2014_x86_64"
            />
          </div>
        </div>

        <div v-else-if="prefetchUI === 'arch'" class="space-y-3">
          <div>
            <label class="ui-label">{{ t('platform.targetPlatform') }}</label>
            <p class="mb-2 text-xs text-muted">{{ t('platform.dockerArchHint') }}</p>
            <div class="flex flex-wrap gap-2">
              <button
                v-for="opt in platformOptions"
                :key="opt.id"
                type="button"
                class="ui-chip"
                :class="{ 'ui-chip-active': prefetchForm.target_platforms.includes(opt.id) }"
                @click="togglePlatform(opt.id)"
              >
                {{ t(opt.labelKey) }}
              </button>
            </div>
          </div>
        </div>
      </section>
    </div>

    <ModalDialog
      v-model:visible="testModalVisible"
      :title="t('platform.testAccess')"
      :description="
        testPhase === 'running'
          ? t('platform.testRunningDesc', { n: testModules?.length || 0 })
          : t('platform.testAccessDesc', { n: testModules?.length || 0 })
      "
      hide-confirm
      wide
    >
      <div v-if="!testModules?.length" class="py-6 text-center text-sm text-muted">
        {{ t('platform.testEmpty') }}
      </div>
      <div v-else class="max-h-[60vh] space-y-3 overflow-y-auto pr-1">
        <section
          v-for="m in testModules"
          :key="m.id"
          class="rounded-lg border border-line px-3 py-3"
        >
          <div class="mb-2 flex flex-wrap items-center gap-2">
            <span
              class="inline-block h-2.5 w-2.5 rounded-full"
              :class="
                testPhase === 'running' && m.checks.some((c) => c.name === 'pending')
                  ? 'animate-pulse bg-warn'
                  : m.ok
                    ? 'bg-ok'
                    : 'bg-danger'
              "
            />
            <span class="font-medium text-fg">{{ moduleLabel(m.id) }}</span>
            <span class="font-mono text-xs text-muted">{{ m.id }}</span>
            <span
              v-if="testPhase === 'running' && m.checks.some((c) => c.name === 'pending')"
              class="text-xs text-muted"
            >
              {{ t('platform.testing') }}
            </span>
            <span v-else-if="m.ok" class="text-xs text-ok">{{ t('platform.testPass') }}</span>
            <span v-else class="text-xs text-danger">{{ t('platform.testFail') }}</span>
          </div>
          <ul class="space-y-2">
            <li
              v-for="(c, i) in m.checks"
              :key="i"
              class="rounded-md bg-panel/50 px-2.5 py-2 text-sm"
            >
              <div class="flex flex-wrap items-center gap-2">
                <span
                  class="inline-block h-2 w-2 rounded-full"
                  :class="
                    c.skipped || c.name === 'pending'
                      ? 'bg-muted'
                      : c.ok
                        ? 'bg-ok'
                        : 'bg-danger'
                  "
                />
                <span class="font-medium text-fg">{{ checkLabel(c.name) }}</span>
                <span
                  v-if="c.skipped || c.name === 'pending'"
                  class="text-xs text-muted"
                >{{ t('platform.testSkipped') }}</span>
                <span v-else-if="c.ok" class="text-xs text-ok">{{ t('platform.testPass') }}</span>
                <span v-else class="text-xs text-danger">{{ t('platform.testFail') }}</span>
                <span v-if="c.ms" class="ml-auto font-mono text-xs text-muted">{{ c.ms }}ms</span>
              </div>
              <p class="mt-1 break-all font-mono text-xs text-muted">{{ c.detail }}</p>
            </li>
          </ul>
        </section>
      </div>
    </ModalDialog>
  </div>
</template>
