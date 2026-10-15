<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import ModalDialog from '@/components/ModalDialog.vue'
import { useQueueStore } from '@/stores/queue'
import { useToastStore } from '@/stores/toast'

const props = defineProps<{
  /** 空=全局；模块预取页传入当前 platform */
  platform?: string
}>()

const { t } = useI18n()
const queueStore = useQueueStore()
const toast = useToastStore()

const open = ref(false)
const busy = ref(false)
const menuRef = ref<HTMLElement | null>(null)

type Action = 'finished' | 'prefetch' | 'all'
/** 与弹窗 visible 分离，避免关窗时清空动作导致确认无效果 */
const pending = ref<Action | null>(null)
const showConfirm = ref(false)

const confirmTitle = ref('')
const confirmDesc = ref('')
const confirmDanger = ref(false)

function closeMenu() {
  open.value = false
}

function toggleMenu() {
  if (busy.value) return
  open.value = !open.value
}

function onDocClick(e: MouseEvent) {
  const el = menuRef.value
  if (!el || !open.value) return
  if (!el.contains(e.target as Node)) closeMenu()
}

onMounted(() => document.addEventListener('click', onDocClick))
onUnmounted(() => document.removeEventListener('click', onDocClick))

function ask(action: Action) {
  closeMenu()
  pending.value = action
  confirmDanger.value = action === 'all'
  if (action === 'finished') {
    confirmTitle.value = t('queue.clearFinished')
    confirmDesc.value = t('queue.clearFinishedConfirm')
  } else if (action === 'prefetch') {
    confirmTitle.value = t('queue.clearPrefetch')
    confirmDesc.value = t('queue.clearPrefetchConfirm')
  } else {
    confirmTitle.value = t('queue.clearAll')
    confirmDesc.value = t('queue.clearAllConfirm')
  }
  showConfirm.value = true
}

function dismissConfirm() {
  showConfirm.value = false
  pending.value = null
}

async function onConfirm() {
  const action = pending.value
  showConfirm.value = false
  if (!action) return
  pending.value = null
  busy.value = true
  const plat = props.platform || undefined
  try {
    if (action === 'finished') {
      const n = await queueStore.clearFinished(plat)
      toast.ok(t('queue.clearedOk', { n }))
    } else if (action === 'prefetch') {
      const r = await queueStore.clearPrefetch(plat)
      toast.ok(t('queue.purgedPrefetchOk', { cancelled: r.cancelled, cleared: r.cleared }))
    } else {
      const r = await queueStore.clearAll(plat)
      toast.ok(t('queue.purgedOk', { cancelled: r.cancelled, cleared: r.cleared }))
    }
  } catch (e: any) {
    toast.err(e?.message || t('queue.clearFailed'))
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div ref="menuRef" class="relative">
    <button type="button" class="ui-btn" :disabled="busy" @click.stop="toggleMenu">
      {{ t('queue.clearMenu') }}
      <svg
        class="ml-1 inline h-3 w-3 text-muted transition-transform"
        :class="{ 'rotate-180': open }"
        viewBox="0 0 20 20"
        fill="currentColor"
      >
        <path
          fill-rule="evenodd"
          d="M5.293 7.293a1 1 0 011.414 0L10 10.586l3.293-3.293a1 1 0 111.414 1.414l-4 4a1 1 0 01-1.414 0l-4-4a1 1 0 010-1.414z"
          clip-rule="evenodd"
        />
      </svg>
    </button>
    <div
      v-show="open"
      class="absolute right-0 top-full z-40 mt-1.5 min-w-[11rem] overflow-hidden rounded-lg border border-line bg-panel py-1 shadow-lg"
    >
      <button type="button" class="user-menu-item w-full text-left" @click="ask('finished')">
        {{ t('queue.clearFinished') }}
      </button>
      <button type="button" class="user-menu-item w-full text-left" @click="ask('prefetch')">
        {{ t('queue.clearPrefetch') }}
      </button>
      <button type="button" class="user-menu-item w-full text-left" @click="ask('all')">
        {{ t('queue.clearAll') }}
      </button>
    </div>
  </div>

  <ModalDialog
    v-model:visible="showConfirm"
    :title="confirmTitle"
    :description="confirmDesc"
    :danger="confirmDanger"
    @cancel="dismissConfirm"
    @confirm="onConfirm"
  />
</template>
