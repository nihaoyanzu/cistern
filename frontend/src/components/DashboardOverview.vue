<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { RouterLink } from 'vue-router'
import StatTile from '@/components/StatTile.vue'
import EmptyState from '@/components/EmptyState.vue'
import DonutChart from '@/components/DonutChart.vue'
import TableHeaderFilter, { type TableFilterOption } from '@/components/TableHeaderFilter.vue'
import { fmtBytes, fmtRate, fmtWaitMs, hitRate } from '@/lib/format'
import { MODULE_BY_ID, MODULES, moduleColor } from '@/modules/registry'
import type { SystemStats } from '@/types/api'

const props = withDefaults(
  defineProps<{
    stats: SystemStats | null
    /** 未登录公开页隐藏「拉取限速」设置入口 */
    showSettingsLink?: boolean
  }>(),
  { showSettingsLink: true },
)

const { t } = useI18n()

const upstreamBps = computed(() => fmtRate(Number(props.stats?.traffic?.upstream_bps ?? 0)))
const downstreamBps = computed(() => fmtRate(Number(props.stats?.traffic?.downstream_bps ?? 0)))
const interactiveActive = computed(() => props.stats?.interactive_active ?? 0)
const resumeCount = computed(() => props.stats?.resume_count ?? 0)
const hitRateStr = computed(() => {
  const c = props.stats?.cache
  return hitRate(Number(c?.hits ?? 0), Number(c?.misses ?? 0))
})
const cacheUsedBytes = computed(() => {
  const c = props.stats?.cache
  return Number(c?.total_size ?? 0) + Number(c?.pending_size ?? 0)
})

const capacityCenter = computed(() => {
  const used = cacheUsedBytes.value
  const max = Number(props.stats?.cache?.max_size ?? 0)
  if (!max) return '—'
  return `${Math.min(100, (used / max) * 100).toFixed(0)}%`
})

const quotaLeft = computed(() => {
  const used = cacheUsedBytes.value
  const max = Number(props.stats?.cache?.max_size ?? 0)
  if (!max) return null
  return Math.max(0, max - used)
})

const cacheSlices = computed(() => {
  const by = props.stats?.cache?.by_platform || {}
  const slices = MODULES.map((d) => ({
    label: t(d.labelKey),
    value: Number(by[d.id] || 0),
    color: moduleColor(d.id),
  }))
  const other = Number(by.other || 0)
  if (other > 0) {
    slices.push({
      label: t('dashboard.otherPlatform'),
      value: other,
      color: 'var(--color-module-other)',
    })
  }
  return slices
})

const hitSlices = computed(() => [
  { label: t('dashboard.hit'), value: Number(props.stats?.cache?.hits ?? 0), color: 'var(--color-ok)' },
  { label: t('dashboard.miss'), value: Number(props.stats?.cache?.misses ?? 0), color: 'var(--color-warn)' },
])

const hitByModule = computed(() => {
  const hits = props.stats?.cache?.hits_by_platform || {}
  const misses = props.stats?.cache?.misses_by_platform || {}
  const rows = MODULES.map((d) => {
    const h = Number(hits[d.id] || 0)
    const m = Number(misses[d.id] || 0)
    const total = h + m
    return {
      id: d.id,
      label: t(d.labelKey),
      color: moduleColor(d.id),
      rate: total > 0 ? `${((h / total) * 100).toFixed(1)}%` : '0%',
    }
  })
  const oh = Number(hits.other || 0)
  const om = Number(misses.other || 0)
  const ot = oh + om
  if (ot > 0) {
    rows.push({
      id: 'other',
      label: t('dashboard.otherPlatform'),
      color: 'var(--color-module-other)',
      rate: `${((oh / ot) * 100).toFixed(1)}%`,
    })
  }
  return rows
})

const limiters = computed(() => props.stats?.rate_limiters ?? [])
const limiterPlatformFilter = ref('')
const limiterPlatformOptions = computed<TableFilterOption[]>(() => {
  const seen = new Set<string>()
  const opts: TableFilterOption[] = []
  for (const r of limiters.value) {
    const id = String(r.platform || '').trim()
    if (!id || seen.has(id)) continue
    seen.add(id)
    opts.push({ value: id, label: platformLabel(id) })
  }
  return opts
})
const filteredLimiters = computed(() => {
  const plat = limiterPlatformFilter.value
  if (!plat) return limiters.value
  return limiters.value.filter((r) => r.platform === plat)
})

function platformLabel(id: string) {
  const d = MODULE_BY_ID[id]
  return d ? t(d.labelKey) : id
}
</script>

<template>
  <div>
    <div class="mb-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
      <StatTile :label="t('dashboard.pull')" :value="upstreamBps" icon="pull" />
      <StatTile :label="t('dashboard.share')" :value="downstreamBps" icon="share" />
      <StatTile :label="t('dashboard.p0Interactive')" :value="String(interactiveActive)" tone="warn" />
      <StatTile :label="t('dashboard.p1Resume')" :value="String(resumeCount)" tone="ok" />
    </div>

    <div v-if="stats" class="mb-6 grid gap-4 lg:grid-cols-2">
      <section class="ui-panel p-4 sm:p-5">
        <div class="mb-3 flex items-center justify-between gap-2">
          <h2 class="text-sm font-semibold text-fg">{{ t('dashboard.capacityByModule') }}</h2>
          <span
            v-if="stats.cache?.water_crit || stats.cache?.water_warn"
            class="h-2 w-2 shrink-0 rounded-full"
            :class="stats.cache?.water_crit ? 'bg-danger' : 'bg-warn'"
            :title="stats.cache?.water_crit ? t('cache.critical') : t('cache.warning')"
          />
        </div>
        <DonutChart
          v-if="cacheSlices.length"
          :slices="cacheSlices"
          value-format="bytes"
          :center-label="t('dashboard.quotaUsage')"
          :center-value="capacityCenter"
        />
        <EmptyState v-else :title="t('dashboard.noModuleCache')" />
        <div class="mt-3 grid grid-cols-2 gap-2 border-t border-line pt-3 text-center sm:grid-cols-4">
          <div>
            <div class="text-[0.65rem] uppercase tracking-wide text-muted">{{ t('dashboard.entries') }}</div>
            <div class="mt-0.5 font-mono text-sm tabular-nums text-fg">{{ stats.cache?.entries ?? 0 }}</div>
          </div>
          <div>
            <div class="text-[0.65rem] uppercase tracking-wide text-muted">{{ t('dashboard.usage') }}</div>
            <div class="mt-0.5 font-mono text-sm tabular-nums text-fg">{{ fmtBytes(cacheUsedBytes) }}</div>
          </div>
          <div>
            <div class="text-[0.65rem] uppercase tracking-wide text-muted">{{ t('dashboard.quotaLeft') }}</div>
            <div class="mt-0.5 font-mono text-sm tabular-nums text-fg">
              {{ quotaLeft == null ? t('dashboard.unknown') : fmtBytes(quotaLeft) }}
            </div>
          </div>
          <div>
            <div class="text-[0.65rem] uppercase tracking-wide text-muted">{{ t('dashboard.disk') }}</div>
            <div class="mt-0.5 font-mono text-sm tabular-nums text-fg">
              {{ stats.cache?.disk_free_ok ? fmtBytes(stats.cache?.disk_free_bytes ?? 0) : t('dashboard.unknown') }}
            </div>
          </div>
        </div>
      </section>
      <section class="ui-panel p-4 sm:p-5">
        <h2 class="mb-3 text-sm font-semibold text-fg">{{ t('dashboard.cacheHit') }}</h2>
        <DonutChart
          :slices="hitSlices"
          :center-label="t('dashboard.hitRate')"
          :center-value="hitRateStr"
        />
        <div class="mt-3 flex gap-1 border-t border-line pt-3 text-center">
          <div v-for="row in hitByModule" :key="row.id" class="min-w-0 flex-1">
            <div class="truncate text-xs text-muted" :title="row.label">
              {{ row.label }}
            </div>
            <div class="mt-0.5 font-mono text-sm tabular-nums" :style="{ color: row.color }">
              {{ row.rate }}
            </div>
          </div>
        </div>
      </section>
    </div>

    <section class="ui-panel overflow-x-auto">
      <div class="flex flex-wrap items-center justify-between gap-2 border-b border-line px-4 py-2.5">
        <h2 class="text-sm font-semibold text-fg">{{ t('dashboard.rateLimit') }}</h2>
        <RouterLink
          v-if="showSettingsLink"
          class="ui-btn-ghost !py-1 text-xs"
          to="/settings?tab=rate"
        >
          {{ t('dashboard.openSettings') }}
        </RouterLink>
      </div>
      <div v-if="limiters.length" class="overflow-x-auto">
        <table class="ui-table">
          <thead>
            <tr>
              <th>
                <TableHeaderFilter
                  v-model="limiterPlatformFilter"
                  :options="limiterPlatformOptions"
                  :all-label="t('access.allPlatforms')"
                  :aria-label="t('dashboard.platform')"
                />
              </th>
              <th>{{ t('dashboard.bandwidthMode') }}</th>
              <th class="text-right">{{ t('platform.prefetchMbps') }}</th>
              <th class="text-right">{{ t('dashboard.tasks') }}</th>
              <th class="text-right">{{ t('dashboard.boost') }}</th>
              <th class="text-right">{{ t('dashboard.connections') }}</th>
              <th>{{ t('dashboard.prefetchStatus') }}</th>
              <th class="text-right">P50 {{ t('dashboard.wait') }}</th>
              <th class="text-right">P95 {{ t('dashboard.wait') }}</th>
            </tr>
          </thead>
          <tbody v-if="filteredLimiters.length">
            <tr v-for="(r, i) in filteredLimiters" :key="i">
              <td class="font-medium">
                <span class="inline-flex items-center gap-2">
                  <span
                    class="ui-module-dot"
                    :style="{ '--module-color': moduleColor(r.platform) }"
                  />
                  {{ platformLabel(r.platform) }}
                </span>
              </td>
              <td>
                <span class="text-xs" :class="r.bandwidth_limited ? 'text-warn' : 'text-ok'">
                  {{ r.bandwidth_limited ? t('dashboard.rateLimited') : t('dashboard.fullSpeed') }}
                </span>
              </td>
              <td class="text-right font-mono text-xs">
                {{ r.bandwidth_limited ? (r.effective_bandwidth_mbps ?? r.bandwidth_mbps) : '∞' }}
              </td>
              <td class="text-right font-mono text-xs">{{ r.active_tasks }}</td>
              <td class="text-right font-mono text-xs">{{ r.active_boost }}/{{ r.boost_slots }}</td>
              <td class="text-right font-mono text-xs">{{ r.active_conns }}</td>
              <td>
                <span class="text-xs" :class="r.prefetch_paused ? 'text-warn' : 'text-ok'">
                  {{ r.prefetch_paused ? t('dashboard.paused') : t('dashboard.running') }}
                </span>
              </td>
              <td class="text-right font-mono text-xs">{{ fmtWaitMs(r.rate_limit_wait_p50) }} ms</td>
              <td class="text-right font-mono text-xs">{{ fmtWaitMs(r.rate_limit_wait_p95) }} ms</td>
            </tr>
          </tbody>
        </table>
        <EmptyState v-if="!filteredLimiters.length" :title="t('common.noData')" />
      </div>
      <div v-else>
        <EmptyState :title="t('dashboard.noRateLimit')" />
      </div>
    </section>
  </div>
</template>
