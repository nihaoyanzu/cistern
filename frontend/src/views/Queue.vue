<script setup lang="ts">
import { computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import PageHeader from '@/components/PageHeader.vue'
import QueueClearMenu from '@/components/QueueClearMenu.vue'
import TaskTable from '@/components/TaskTable.vue'
import type { TableFilterOption } from '@/components/TableHeaderFilter.vue'
import { MODULES } from '@/modules/registry'
import { useQueueStore, type QueueStatusFilter } from '@/stores/queue'
import { useToastStore } from '@/stores/toast'

const { t } = useI18n()
const toast = useToastStore()
const queueStore = useQueueStore()

const statusOptions = computed<TableFilterOption[]>(() => [
  { value: 'running', label: t('queue.running') },
  { value: 'done', label: t('queue.done') },
  { value: 'error', label: t('queue.error') },
  { value: 'cancelled', label: t('queue.cancelled') },
])

const priorityOptions = computed<TableFilterOption[]>(() => [
  { value: 'P0', label: t('status.p0Interactive') },
  { value: 'P0+', label: t('status.p0Boost') },
  { value: 'P1', label: t('status.p1Resume') },
  { value: 'P2', label: t('status.p2Prefetch') },
])

const platformOptions = computed<TableFilterOption[]>(() =>
  MODULES.map((d) => ({ value: d.id, label: t(d.labelKey) })),
)

const statusFilter = computed({
  get: (): string => (queueStore.status === 'all' ? '' : queueStore.status),
  set: (v: string) => queueStore.setStatus((v || 'all') as QueueStatusFilter),
})

const priorityFilter = computed({
  get: () => queueStore.priority,
  set: (v: string) => {
    queueStore.setQuery({ platform: queueStore.platform, priority: v })
  },
})

const platformFilter = computed({
  get: () => queueStore.platform,
  set: (v: string) => {
    queueStore.setQuery({ platform: v, priority: queueStore.priority })
  },
})

onMounted(() => {
  queueStore.setQuery({ platform: '', priority: '', pageSize: 15, status: 'all' })
  queueStore.startPolling(3000)
})
onUnmounted(() => queueStore.stopPolling())

async function cancel(id: string) {
  try {
    await queueStore.cancel(id)
    toast.ok(t('queue.cancelledOk'))
  } catch (e: any) {
    toast.err(e.message || t('queue.cancelFailed'))
  }
}

async function cancelMany(ids: string[]) {
  try {
    await queueStore.cancelMany(ids)
    toast.ok(t('queue.cancelledManyOk', { n: ids.length }))
  } catch (e: any) {
    toast.err(e.message || t('queue.cancelFailed'))
  }
}
</script>

<template>
  <div>
    <PageHeader :title="t('queue.title')">
      <template #actions>
        <span class="ui-badge-muted">{{ t('queue.running') }} {{ queueStore.runningCount }}</span>
        <QueueClearMenu />
      </template>
    </PageHeader>

    <section class="ui-panel overflow-hidden">
      <TaskTable
        :tasks="queueStore.tasks"
        :loading="queueStore.loading"
        :show-priority="true"
        :empty-title="t('queue.queueIdle')"
        :server-page="queueStore.page"
        :server-total="queueStore.total"
        :server-page-size="queueStore.pageSize"
        show-priority-filter
        show-status-filter
        show-platform-filter
        :priority-filter="priorityFilter"
        :status-filter="statusFilter"
        :platform-filter="platformFilter"
        :priority-options="priorityOptions"
        :status-options="statusOptions"
        :platform-options="platformOptions"
        @update:priority-filter="priorityFilter = $event"
        @update:status-filter="statusFilter = $event"
        @update:platform-filter="platformFilter = $event"
        @cancel="cancel"
        @cancel-many="cancelMany"
        @update:page="queueStore.setPage"
      />
    </section>
  </div>
</template>
