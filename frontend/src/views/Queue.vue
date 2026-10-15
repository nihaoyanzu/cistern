<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import PageHeader from '@/components/PageHeader.vue'
import QueueClearMenu from '@/components/QueueClearMenu.vue'
import TaskTable from '@/components/TaskTable.vue'
import { useQueueStore, type QueueStatusFilter } from '@/stores/queue'
import { useToastStore } from '@/stores/toast'

const { t } = useI18n()
const toast = useToastStore()
const queueStore = useQueueStore()

const filter = defineModel<'all' | 'running' | 'done' | 'error' | 'cancelled'>('filter', {
  default: 'all',
})

const filters = computed(() => [
  { id: 'all' as const, label: t('queue.all') },
  { id: 'running' as const, label: t('queue.running') },
  { id: 'done' as const, label: t('queue.done') },
  { id: 'error' as const, label: t('queue.error') },
  { id: 'cancelled' as const, label: t('queue.cancelled') },
])

watch(
  filter,
  (v) => {
    queueStore.setQuery({ platform: '', priority: '' })
    queueStore.setStatus(v as QueueStatusFilter)
  },
  { immediate: true },
)

onMounted(() => {
  queueStore.setQuery({ platform: '', priority: '', pageSize: 15 })
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
        <span class="h-4 w-px bg-line" />
        <button
          v-for="f in filters"
          :key="f.id"
          type="button"
          class="ui-chip"
          :class="{ 'ui-chip-active': filter === f.id }"
          @click="filter = f.id"
        >
          {{ f.label }}
        </button>
      </template>
    </PageHeader>

    <section class="ui-panel overflow-hidden">
      <TaskTable
        :key="filter"
        :tasks="queueStore.tasks"
        :loading="queueStore.loading"
        :show-priority="true"
        :empty-title="t('queue.queueIdle')"
        :server-page="queueStore.page"
        :server-total="queueStore.total"
        :server-page-size="queueStore.pageSize"
        @cancel="cancel"
        @cancel-many="cancelMany"
        @update:page="queueStore.setPage"
      />
    </section>
  </div>
</template>
