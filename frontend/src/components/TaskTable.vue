<script setup lang="ts">
import EmptyState from '@/components/EmptyState.vue'
import ModalDialog from '@/components/ModalDialog.vue'
import PaginationBar from '@/components/PaginationBar.vue'
import TableHeaderFilter, { type TableFilterOption } from '@/components/TableHeaderFilter.vue'
import { usePagination } from '@/composables/usePagination'
import { fmtBytes, fmtDurationMs } from '@/lib/format'
import { prioClass, prioKey, statusClass, statusKey } from '@/lib/status'
import { computed, ref, toRef, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { QueueTask } from '@/types/api'

const { t, locale } = useI18n()

const props = withDefaults(
  defineProps<{
    tasks: QueueTask[]
    loading?: boolean
    showPriority?: boolean
    emptyTitle?: string
    emptyDescription?: string
    pageSize?: number
    /** 服务端分页：传入后不再对 tasks 做客户端切片 */
    serverPage?: number
    serverTotal?: number
    serverPageSize?: number
    /** 表头过滤：传入 options 时渲染下拉，否则显示静态标题 */
    priorityFilter?: string
    statusFilter?: string
    platformFilter?: string
    priorityOptions?: TableFilterOption[]
    statusOptions?: TableFilterOption[]
    platformOptions?: TableFilterOption[]
    showPriorityFilter?: boolean
    showStatusFilter?: boolean
    showPlatformFilter?: boolean
  }>(),
  {
    loading: false,
    showPriority: true,
    emptyTitle: '',
    emptyDescription: '',
    pageSize: 15,
    priorityFilter: '',
    statusFilter: '',
    platformFilter: '',
    priorityOptions: () => [],
    statusOptions: () => [],
    platformOptions: () => [],
    showPriorityFilter: false,
    showStatusFilter: false,
    showPlatformFilter: false,
  },
)

const emit = defineEmits<{
  cancel: [id: string]
  cancelMany: [ids: string[]]
  'update:page': [n: number]
  'update:priorityFilter': [v: string]
  'update:statusFilter': [v: string]
  'update:platformFilter': [v: string]
}>()

const tasksRef = toRef(props, 'tasks')
const clientPager = usePagination(tasksRef, props.pageSize)

const serverMode = computed(() => props.serverTotal != null)

const page = computed(() => (serverMode.value ? props.serverPage || 1 : clientPager.page.value))
const size = computed(() =>
  serverMode.value ? props.serverPageSize || props.pageSize : clientPager.size.value,
)
const total = computed(() => (serverMode.value ? props.serverTotal || 0 : clientPager.total.value))
const pageCount = computed(() =>
  serverMode.value
    ? Math.max(1, Math.ceil(total.value / size.value) || 1)
    : clientPager.pageCount.value,
)
const slice = computed(() => (serverMode.value ? props.tasks : clientPager.slice.value))

function go(p: number) {
  if (serverMode.value) {
    emit('update:page', p)
    return
  }
  clientPager.go(p)
}

const selected = ref<Set<string>>(new Set())
const expandedId = ref<string | null>(null)
const cancelling = ref(false)
const showCancelModal = ref(false)
const headerCheck = ref<HTMLInputElement | null>(null)

const colCount = computed(() => (props.showPriority ? 8 : 7))

const effectiveEmptyTitle = computed(() => props.emptyTitle || t('common.noData'))
const effectiveEmptyDesc = computed(() => props.emptyDescription || undefined)

const priorityModel = computed({
  get: () => props.priorityFilter,
  set: (v: string) => emit('update:priorityFilter', v),
})
const statusModel = computed({
  get: () => props.statusFilter,
  set: (v: string) => emit('update:statusFilter', v),
})
const platformModel = computed({
  get: () => props.platformFilter,
  set: (v: string) => emit('update:platformFilter', v),
})

function canCancel(task: QueueTask) {
  // P0/P1/P2 及 waiting 均可管理台强制取消
  if (
    task.status !== 'running' &&
    task.status !== 'preparing' &&
    task.status !== 'queued' &&
    task.status !== 'waiting'
  ) {
    return false
  }
  return true
}

function hasErrorDetail(task: QueueTask) {
  return task.status === 'error' && !!(task.error || task.url || task.detail)
}

function formatTaskError(task: QueueTask): string {
  const code = (task.error_code || '').trim()
  if (code) {
    const key = `queue.errorCodes.${code}`
    const mapped = t(key)
    if (mapped && mapped !== key) {
      return task.error ? `${mapped} — ${task.error}` : mapped
    }
  }
  return task.error || ''
}

function isExpanded(id: string) {
  return expandedId.value === id
}

function toggleExpand(id: string) {
  expandedId.value = expandedId.value === id ? null : id
}

function taskTitle(task: QueueTask) {
  return task.label || task.url || '—'
}

function taskSub(task: QueueTask) {
  const label = task.label || ''
  const detail = task.detail || ''
  if (detail && detail !== label) return detail
  return ''
}

function progressPct(task: QueueTask): number | null {
  const totalBytes = Number(task.bytes_total || 0)
  if (totalBytes <= 0) return null
  if (task.status !== 'running' && task.status !== 'preparing' && task.status !== 'queued') {
    if (task.status === 'done') return 100
    return null
  }
  const done = Number(task.bytes_done || 0)
  return Math.min(100, Math.max(0, Math.round((done / totalBytes) * 100)))
}

function progressText(task: QueueTask) {
  const pct = progressPct(task)
  if (pct == null) return ''
  return t('queue.progress', {
    done: fmtBytes(Number(task.bytes_done || 0)),
    total: fmtBytes(Number(task.bytes_total || 0)),
    pct,
  })
}

const cancellableOnPage = computed(() => slice.value.filter(canCancel))
const selectedCancellable = computed(() =>
  props.tasks.filter((task) => selected.value.has(task.id) && canCancel(task)),
)

const allPageSelected = computed(() => {
  const list = cancellableOnPage.value
  return list.length > 0 && list.every((task) => selected.value.has(task.id))
})

const somePageSelected = computed(() => {
  const list = cancellableOnPage.value
  return list.some((task) => selected.value.has(task.id)) && !allPageSelected.value
})

watch(
  () => props.tasks.map((task) => task.id).join('\0'),
  () => {
    const alive = new Set(props.tasks.map((task) => task.id))
    selected.value = new Set([...selected.value].filter((id) => alive.has(id)))
    if (expandedId.value && !alive.has(expandedId.value)) expandedId.value = null
  },
)

watch([somePageSelected, headerCheck], () => {
  if (headerCheck.value) headerCheck.value.indeterminate = somePageSelected.value
})

function toggleOne(id: string, on: boolean) {
  const next = new Set(selected.value)
  if (on) next.add(id)
  else next.delete(id)
  selected.value = next
}

function togglePage(on: boolean) {
  const next = new Set(selected.value)
  for (const task of cancellableOnPage.value) {
    if (on) next.add(task.id)
    else next.delete(task.id)
  }
  selected.value = next
}

function onHeaderCheck(ev: Event) {
  const el = ev.target as HTMLInputElement
  togglePage(el.checked)
}

function requestCancelSelected() {
  if (!selectedCancellable.value.length) return
  showCancelModal.value = true
}

async function confirmCancelSelected() {
  const ids = selectedCancellable.value.map((task) => task.id)
  if (!ids.length) return
  cancelling.value = true
  try {
    emit('cancelMany', ids)
    selected.value = new Set()
  } finally {
    cancelling.value = false
    showCancelModal.value = false
  }
}
</script>

<template>
  <div>
    <div
      v-if="selectedCancellable.length"
      class="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5"
    >
      <span class="ui-badge-muted">{{ t('queue.selected', { n: selectedCancellable.length }) }}</span>
      <button
        type="button"
        class="ui-btn-danger !px-3 !py-1.5 text-xs"
        :disabled="cancelling"
        @click="requestCancelSelected"
      >
        {{ t('queue.cancelSelected') }}
      </button>
      <button type="button" class="ui-btn-ghost !px-2 !py-1 text-xs" @click="selected = new Set()">
        {{ t('queue.clearSelection') }}
      </button>
    </div>

    <div class="overflow-x-auto">
      <table class="ui-table">
        <thead>
          <tr>
            <th class="w-10">
              <input
                ref="headerCheck"
                type="checkbox"
                class="accent-accent"
                :checked="allPageSelected"
                :disabled="!cancellableOnPage.length"
                :title="t('queue.selectAllPage')"
                @change="onHeaderCheck"
              />
            </th>
            <th v-if="showPriority">
              <TableHeaderFilter
                v-if="showPriorityFilter"
                v-model="priorityModel"
                :options="priorityOptions"
                :all-label="t('queue.allPriorities')"
                :aria-label="t('queue.priority')"
              />
              <template v-else>{{ t('queue.priority') }}</template>
            </th>
            <th>
              <TableHeaderFilter
                v-if="showStatusFilter"
                v-model="statusModel"
                :options="statusOptions"
                :all-label="t('queue.all')"
                :aria-label="t('queue.status')"
              />
              <template v-else>{{ t('queue.status') }}</template>
            </th>
            <th>
              <TableHeaderFilter
                v-if="showPlatformFilter"
                v-model="platformModel"
                :options="platformOptions"
                :all-label="t('access.allPlatforms')"
                :aria-label="t('dashboard.platform')"
              />
              <template v-else>{{ t('dashboard.platform') }}</template>
            </th>
            <th>{{ t('queue.task') }}</th>
            <th>{{ t('queue.wait') }}</th>
            <th>{{ t('queue.download') }}</th>
            <th class="w-24">{{ t('queue.actions') }}</th>
          </tr>
        </thead>
        <tbody v-if="slice.length">
          <template v-for="task in slice" :key="task.id">
            <tr
              :class="hasErrorDetail(task) ? 'cursor-pointer hover:bg-panel/80' : ''"
              @click="hasErrorDetail(task) && toggleExpand(task.id)"
            >
              <td @click.stop>
                <input
                  v-if="canCancel(task)"
                  type="checkbox"
                  class="accent-accent"
                  :checked="selected.has(task.id)"
                  @change="toggleOne(task.id, ($event.target as HTMLInputElement).checked)"
                />
              </td>
              <td v-if="showPriority">
                <span :class="prioClass(task.priority)">{{ t(prioKey(task.priority)) }}</span>
              </td>
              <td>
                <span :class="statusClass(task.status)">{{ t(statusKey(task.status)) }}</span>
              </td>
              <td>{{ task.platform }}</td>
              <td class="min-w-[220px] max-w-[420px]">
                <div class="flex items-start gap-1.5">
                  <span
                    v-if="hasErrorDetail(task)"
                    class="mt-0.5 shrink-0 select-none text-xs text-muted"
                    aria-hidden="true"
                  >{{ isExpanded(task.id) ? '▾' : '▸' }}</span>
                  <div class="min-w-0 flex-1">
                    <div class="truncate text-sm text-fg" :title="taskTitle(task)">{{ taskTitle(task) }}</div>
                    <div
                      v-if="taskSub(task) && !isExpanded(task.id)"
                      class="mt-0.5 truncate font-mono text-xs text-muted"
                      :title="taskSub(task)"
                    >
                      {{ taskSub(task) }}
                    </div>
                    <div v-if="progressPct(task) != null" class="mt-1.5">
                      <div class="h-1.5 overflow-hidden rounded bg-line">
                        <div
                          class="h-full rounded bg-accent transition-[width] duration-200"
                          :style="{ width: `${progressPct(task)}%` }"
                        />
                      </div>
                      <div class="mt-0.5 font-mono text-xs text-muted">{{ progressText(task) }}</div>
                    </div>
                    <div
                      v-if="task.error && !isExpanded(task.id)"
                      class="mt-1 truncate text-xs text-danger"
                      :title="formatTaskError(task)"
                    >
                      {{ formatTaskError(task) }}
                    </div>
                  </div>
                </div>
              </td>
              <td class="whitespace-nowrap font-mono text-xs text-muted">
                {{ fmtDurationMs(task.wait_ms, locale) }}
              </td>
              <td class="whitespace-nowrap font-mono text-xs text-muted">
                {{
                  task.status === 'queued'
                    ? '-'
                    : fmtDurationMs(task.elapsed_ms, locale)
                }}
              </td>
              <td @click.stop>
                <button
                  v-if="canCancel(task)"
                  type="button"
                  class="ui-btn-danger !px-2 !py-1 text-xs"
                  @click="emit('cancel', task.id)"
                >
                  {{ t('queue.cancelAction') }}
                </button>
              </td>
            </tr>
            <tr v-if="hasErrorDetail(task) && isExpanded(task.id)" class="bg-panel/40">
              <td :colspan="colCount" class="!whitespace-normal px-4 py-3">
                <div class="space-y-2 text-xs">
                  <div v-if="task.error">
                    <div class="mb-1 font-medium text-danger">{{ t('status.error') }}</div>
                    <pre class="overflow-x-auto whitespace-pre-wrap break-all font-mono text-danger/90">{{ formatTaskError(task) }}</pre>
                  </div>
                  <div v-if="task.detail && task.detail !== task.label">
                    <div class="mb-1 font-medium text-muted">{{ t('queue.task') }}</div>
                    <pre class="overflow-x-auto whitespace-pre-wrap break-all font-mono text-muted">{{ task.detail }}</pre>
                  </div>
                  <div v-if="task.url">
                    <div class="mb-1 font-medium text-muted">URL</div>
                    <pre class="overflow-x-auto whitespace-pre-wrap break-all font-mono text-muted">{{ task.url }}</pre>
                  </div>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
      <EmptyState
        v-if="!slice.length"
        :title="loading ? t('common.loading') : effectiveEmptyTitle"
        :description="effectiveEmptyDesc"
      />
    </div>
    <PaginationBar
      :page="page"
      :page-count="pageCount"
      :total="total"
      :page-size="size"
      @update:page="go"
    />

    <ModalDialog
      v-model:visible="showCancelModal"
      :title="t('queue.cancelSelected')"
      :description="t('queue.cancelSelectedConfirm', { n: selectedCancellable.length })"
      :confirm-text="t('queue.cancelSelected')"
      danger
      @confirm="confirmCancelSelected"
      @cancel="showCancelModal = false"
    />
  </div>
</template>
