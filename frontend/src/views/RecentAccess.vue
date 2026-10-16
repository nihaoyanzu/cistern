<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import EmptyState from '@/components/EmptyState.vue'
import PaginationBar from '@/components/PaginationBar.vue'
import PageHeader from '@/components/PageHeader.vue'
import TableHeaderFilter, { type TableFilterOption } from '@/components/TableHeaderFilter.vue'
import { api } from '@/api/client'
import { fmtBytes, fmtTime } from '@/lib/format'
import { cacheClass, cacheKey } from '@/lib/status'
import { MODULE_BY_ID, moduleColor } from '@/modules/registry'
import type { TrafficRecord } from '@/types/api'

const { t } = useI18n()

const loading = ref(false)
const items = ref<TrafficRecord[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const platformFilter = ref('')
const platforms = ref<string[]>([])
let timer: number | undefined

const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value) || 1))

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const res = await api.getAccess({
      page: page.value,
      page_size: pageSize.value,
      platform: platformFilter.value || undefined,
    })
    items.value = res.items || []
    total.value = res.total ?? 0
    if (res.page && res.page > 0) page.value = res.page
    if (res.page_size && res.page_size > 0) pageSize.value = res.page_size
    platforms.value = res.platforms || []
    if (page.value > pageCount.value) page.value = pageCount.value
  } catch {
    /* keep last */
  } finally {
    if (!silent) loading.value = false
  }
}

function go(p: number) {
  const next = Math.min(Math.max(1, p), pageCount.value)
  if (next === page.value) return
  page.value = next
  load(true)
}

watch(platformFilter, () => {
  page.value = 1
  load()
})

  function platformLabel(id: string) {
  return MODULE_BY_ID[id]?.guideTitle || id
}

const platformOptions = computed<TableFilterOption[]>(() =>
  platforms.value.map((id) => ({ value: id, label: platformLabel(id) })),
)

onMounted(() => {
  load()
  timer = window.setInterval(() => load(true), 5000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<template>
  <div>
    <PageHeader :title="t('access.title')" />

    <section class="ui-panel overflow-hidden">
      <div class="overflow-x-auto">
        <table class="ui-table">
          <thead>
            <tr>
              <th>{{ t('access.time') }}</th>
              <th>{{ t('access.ip') }}</th>
              <th>
                <TableHeaderFilter
                  v-model="platformFilter"
                  :options="platformOptions"
                  :all-label="t('access.allPlatforms')"
                  :aria-label="t('access.platform')"
                />
              </th>
              <th>{{ t('access.path') }}</th>
              <th>{{ t('access.cacheStatus') }}</th>
              <th class="text-right">{{ t('access.delivered') }}</th>
            </tr>
          </thead>
          <tbody v-if="total">
            <tr v-for="(r, i) in items" :key="`${r.at}-${r.path}-${i}`">
              <td class="whitespace-nowrap text-sm text-muted">{{ fmtTime(r.at) }}</td>
              <td class="font-mono text-sm">{{ r.ip }}</td>
              <td class="text-sm">
                <span class="inline-flex items-center gap-1.5">
                  <span
                    v-if="r.platform"
                    class="ui-module-dot"
                    :style="{ '--module-color': moduleColor(String(r.platform)) }"
                  />
                  {{ platformLabel(r.platform) || '—' }}
                </span>
              </td>
              <td class="max-w-[480px] truncate font-mono text-sm text-muted" :title="r.path">
                {{ r.path }}
              </td>
              <td>
                <span :class="cacheClass(r.cache)">{{ t(cacheKey(r.cache)) }}</span>
              </td>
              <td class="text-right font-mono text-sm">{{ fmtBytes(r.bytes) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <EmptyState
        v-if="!total"
        :title="loading ? t('common.loading') : t('access.noAccess')"
      />
      <PaginationBar
        :page="page"
        :page-count="pageCount"
        :total="total"
        :page-size="pageSize"
        @update:page="go"
      />
    </section>
  </div>
</template>
