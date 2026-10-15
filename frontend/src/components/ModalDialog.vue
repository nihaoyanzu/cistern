<script setup lang="ts">
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = defineProps<{
  visible: boolean
  title?: string
  description?: string
  confirmText?: string
  cancelText?: string
  danger?: boolean
  /** 仅关闭按钮（结果展示等） */
  hideConfirm?: boolean
  wide?: boolean
  /** 点遮罩 / Esc 不关闭（升级进度等） */
  persistent?: boolean
}>()

const emit = defineEmits<{
  confirm: []
  cancel: []
  'update:visible': [value: boolean]
}>()

function onOverlayOrEscape() {
  if (props.persistent) return
  onCancel()
}

function onCancel() {
  // 先 cancel 再关窗，避免父组件用同一状态驱动 visible 时动作丢失
  emit('cancel')
  emit('update:visible', false)
}

function onConfirm() {
  // 先 confirm 再关窗，避免父组件在 update:visible 里清空动作上下文
  emit('confirm')
  emit('update:visible', false)
}
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="visible"
        class="fixed inset-0 z-[100] flex items-center justify-center p-4"
        @keydown.escape="onOverlayOrEscape"
      >
        <div class="absolute inset-0 bg-black/45 backdrop-blur-[2px]" @click="onOverlayOrEscape" />

        <div
          class="ui-panel relative w-full p-5"
          :class="wide ? 'max-w-xl' : 'max-w-md'"
          style="box-shadow: var(--shadow-modal)"
        >
          <h3 v-if="title" class="text-base font-semibold leading-snug text-fg">{{ title }}</h3>
          <p v-if="description" class="mt-1.5 text-sm leading-relaxed text-muted">{{ description }}</p>
          <div v-if="$slots.default" :class="title || description ? 'mt-3' : ''">
            <slot />
          </div>
          <div class="mt-4 flex justify-end gap-2">
            <button type="button" class="ui-btn" @click="onCancel">
              {{ cancelText || (hideConfirm ? t('common.close') : t('common.cancel')) }}
            </button>
            <button
              v-if="!hideConfirm"
              type="button"
              :class="danger ? 'ui-btn-danger' : 'ui-btn-primary'"
              @click="onConfirm"
            >
              {{ confirmText || t('common.confirm') }}
            </button>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.modal-enter-active,
.modal-leave-active {
  transition: opacity 0.18s ease;
}
.modal-enter-active .ui-panel,
.modal-leave-active .ui-panel {
  transition: transform 0.18s ease, opacity 0.18s ease;
}
.modal-enter-from,
.modal-leave-to {
  opacity: 0;
}
.modal-enter-from .ui-panel,
.modal-leave-to .ui-panel {
  transform: translateY(6px) scale(0.98);
  opacity: 0;
}
</style>
