import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '@/api/client'
import type { QueueTask } from '@/types/api'

export type QueueStatusFilter = 'all' | 'running' | 'done' | 'error' | 'cancelled'

export const useQueueStore = defineStore('queue', () => {
  const tasks = ref<QueueTask[]>([])
  const loading = ref(false)
  const auto = ref(true)
  const page = ref(1)
  const pageSize = ref(15)
  const total = ref(0)
  const runningCount = ref(0)
  const status = ref<QueueStatusFilter>('all')
  const platform = ref('')
  const priority = ref('')
  let timer: number | undefined

  const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value) || 1))

  async function load(silent = false) {
    if (!silent) loading.value = true
    try {
      const res = await api.getQueue({
        page: page.value,
        page_size: pageSize.value,
        status: status.value === 'all' ? undefined : status.value,
        platform: platform.value || undefined,
        priority: priority.value || undefined,
      })
      tasks.value = res.tasks || []
      total.value = res.total ?? tasks.value.length
      if (res.page && res.page > 0) page.value = res.page
      if (res.page_size && res.page_size > 0) pageSize.value = res.page_size
      runningCount.value = res.running_count ?? tasks.value.filter((t) => t.status === 'running').length
      if (page.value > pageCount.value) {
        page.value = pageCount.value
      }
    } catch {
      // silent
    } finally {
      if (!silent) loading.value = false
    }
  }

  function setPage(p: number) {
    const next = Math.min(Math.max(1, p), pageCount.value)
    if (next === page.value) return
    page.value = next
    return load(true)
  }

  function setStatus(s: QueueStatusFilter) {
    status.value = s
    page.value = 1
    return load()
  }

  function setQuery(opts: {
    platform?: string
    priority?: string
    pageSize?: number
    status?: QueueStatusFilter
  }) {
    if (opts.platform !== undefined) platform.value = opts.platform
    if (opts.priority !== undefined) priority.value = opts.priority
    if (opts.pageSize !== undefined) pageSize.value = opts.pageSize
    if (opts.status !== undefined) status.value = opts.status
    page.value = 1
    return load()
  }

  async function cancel(id: string) {
    await api.cancelPrefetch(id)
    await load(true)
  }

  async function cancelMany(ids: string[]) {
    const uniq = [...new Set(ids.filter(Boolean))]
    for (const id of uniq) {
      await api.cancelPrefetch(id)
    }
    await load(true)
  }

  async function clearFinished(platform?: string) {
    const res = await api.clearQueue({ mode: 'finished', platform })
    await load(true)
    return res.cleared ?? 0
  }

  async function clearPrefetch(platform?: string) {
    // 仅 P1/P2，不伤 P0
    const res = await api.clearQueue({ mode: 'prefetch', platform })
    await load(true)
    return { cancelled: res.cancelled ?? 0, cleared: res.cleared ?? 0 }
  }

  async function clearAll(platform?: string) {
    // 含 P0 交互下载
    const res = await api.clearQueue({ mode: 'all', platform })
    await load(true)
    return { cancelled: res.cancelled ?? 0, cleared: res.cleared ?? 0 }
  }

  function startPolling(intervalMs = 3000) {
    stopPolling()
    auto.value = true
    load()
    timer = window.setInterval(() => {
      if (auto.value) load(true)
    }, intervalMs)
  }

  function stopPolling() {
    if (timer) {
      clearInterval(timer)
      timer = undefined
    }
  }

  return {
    tasks,
    loading,
    auto,
    page,
    pageSize,
    total,
    pageCount,
    runningCount,
    status,
    platform,
    priority,
    load,
    setPage,
    setStatus,
    setQuery,
    cancel,
    cancelMany,
    clearFinished,
    clearPrefetch,
    clearAll,
    startPolling,
    stopPolling,
  }
})
