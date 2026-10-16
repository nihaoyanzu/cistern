<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import PageHeader from '@/components/PageHeader.vue'
import QueueClearMenu from '@/components/QueueClearMenu.vue'
import TaskTable from '@/components/TaskTable.vue'
import type { TableFilterOption } from '@/components/TableHeaderFilter.vue'
import { api } from '@/api/client'
import { useToastStore } from '@/stores/toast'
import { useQueueStore, type QueueStatusFilter } from '@/stores/queue'
import { isKnownModule, MODULE_BY_ID, MODULES } from '@/modules/registry'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const toast = useToastStore()
const queueStore = useQueueStore()

const text = ref('')
const submitting = ref(false)
const lastSkipped = ref<string[]>([])

const moduleId = computed(() => {
  const raw = route.query.module
  const mod = String(Array.isArray(raw) ? raw[0] : raw || '')
  if (isKnownModule(mod) && MODULE_BY_ID[mod]?.nav.prefetch) return mod
  const fallback = MODULES.find((d) => d.nav.prefetch)?.id || 'pypi'
  return fallback
})

const currentDesc = computed(() => MODULE_BY_ID[moduleId.value] || MODULES[0])

const pasteHint = computed(() => {
  const key = currentDesc.value.hints.prefetchPasteHintKey
  return key ? t(key) : t('prefetch.hint')
})

const pastePlaceholder = computed(() => {
  const key = currentDesc.value.hints.prefetchPlaceholderKey
  return key ? t(key) : t('prefetch.placeholder')
})

function applyQueueQuery() {
  queueStore.setQuery({
    platform: moduleId.value,
    priority: 'P1,P2',
    pageSize: 15,
    status: 'all',
  })
}

const statusOptions = computed<TableFilterOption[]>(() => [
  { value: 'running', label: t('queue.running') },
  { value: 'done', label: t('queue.done') },
  { value: 'error', label: t('queue.error') },
  { value: 'cancelled', label: t('queue.cancelled') },
])

const priorityOptions = computed<TableFilterOption[]>(() => [
  { value: 'P1', label: t('status.p1Resume') },
  { value: 'P2', label: t('status.p2Prefetch') },
])

const statusFilter = computed({
  get: (): string => (queueStore.status === 'all' ? '' : queueStore.status),
  set: (v: string) => queueStore.setStatus((v || 'all') as QueueStatusFilter),
})

const priorityFilter = computed({
  get: (): string => {
    const p = queueStore.priority
    return !p || p === 'P1,P2' ? '' : p
  },
  set: (v: string) => {
    queueStore.setQuery({
      platform: moduleId.value,
      priority: v || 'P1,P2',
    })
  },
})

const lineHint = computed(() => {
  const n = text.value.split(/\r?\n/).filter((l) => l.trim() && !l.trim().startsWith('#')).length
  return n > 1 ? t('prefetch.lines', { n }) : ''
})

function syncModuleFromRoute() {
  const raw = route.query.module
  const mod = String(Array.isArray(raw) ? raw[0] : raw || '')
  if (!isKnownModule(mod) || !MODULE_BY_ID[mod]?.nav.prefetch) {
    router.replace({ path: '/prefetch', query: { module: moduleId.value } })
  }
}

watch(() => route.query.module, syncModuleFromRoute)

async function submit() {
  const content = text.value.trim()
  if (!content) {
    toast.err(t('prefetch.emptyInput'))
    return
  }
  submitting.value = true
  lastSkipped.value = []
  try {
    const res = await api.prefetch([], content, moduleId.value)
    lastSkipped.value = res.skipped || []
    const skipTip = lastSkipped.value.length ? t('prefetch.skipped', { n: lastSkipped.value.length }) : ''
    toast.ok(t('prefetch.submitted', { n: res.enqueued }) + skipTip)
    text.value = ''
    await queueStore.load(true)
  } catch (e: any) {
    toast.err(e.message || t('prefetch.submitFailed'))
  } finally {
    submitting.value = false
  }
}

async function cancel(id: string) {
  try {
    await queueStore.cancel(id)
    toast.ok(t('queue.cancelledOk'))
    await queueStore.load(true)
  } catch (e: any) {
    toast.err(e.message || t('queue.cancelFailed'))
  }
}

async function cancelMany(ids: string[]) {
  try {
    await queueStore.cancelMany(ids)
    toast.ok(t('queue.cancelledManyOk', { n: ids.length }))
    await queueStore.load(true)
  } catch (e: any) {
    toast.err(e.message || t('queue.cancelFailed'))
  }
}

watch(moduleId, () => {
  applyQueueQuery()
})

onMounted(() => {
  syncModuleFromRoute()
  applyQueueQuery()
  queueStore.startPolling(4000)
})
onUnmounted(() => queueStore.stopPolling())
</script>

<template>
  <div>
    <PageHeader :title="t('prefetch.title')">
      <template #actions>
        <QueueClearMenu :platform="moduleId" />
      </template>
    </PageHeader>

    <section class="ui-panel mb-5 p-4 sm:p-5">
      <div class="mb-3 flex items-start justify-between gap-2">
        <p class="text-xs text-muted">{{ pasteHint }}</p>
        <span v-if="lineHint" class="ui-badge-muted shrink-0">{{ lineHint }}</span>
      </div>
      <form class="space-y-3" @submit.prevent="submit">
        <textarea
          v-model="text"
          rows="6"
          class="ui-input font-mono text-sm"
          :placeholder="pastePlaceholder"
        />
        <div class="flex flex-wrap gap-2">
          <button type="submit" class="ui-btn-primary" :disabled="submitting">
            {{ submitting ? t('prefetch.submitting') : t('common.submit') }}
          </button>
          <button type="button" class="ui-btn" :disabled="!text" @click="text = ''">{{ t('common.clear') }}</button>
        </div>
        <p v-if="lastSkipped.length" class="text-xs text-muted">
          {{ t('prefetch.skippedLabel') }}{{ lastSkipped.slice(0, 5).join(' · ') }}
          <span v-if="lastSkipped.length > 5"> 等 {{ lastSkipped.length }} 项</span>
        </p>
      </form>
    </section>

    <section class="ui-panel overflow-hidden">
      <TaskTable
        :tasks="queueStore.tasks"
        :loading="queueStore.loading"
        :show-priority="true"
        :empty-title="t('prefetch.noTasks')"
        :server-page="queueStore.page"
        :server-total="queueStore.total"
        :server-page-size="queueStore.pageSize"
        show-priority-filter
        show-status-filter
        :priority-filter="priorityFilter"
        :status-filter="statusFilter"
        :priority-options="priorityOptions"
        :status-options="statusOptions"
        @update:priority-filter="priorityFilter = $event"
        @update:status-filter="statusFilter = $event"
        @cancel="cancel"
        @cancel-many="cancelMany"
        @update:page="queueStore.setPage"
      />
    </section>
  </div>
</template>
