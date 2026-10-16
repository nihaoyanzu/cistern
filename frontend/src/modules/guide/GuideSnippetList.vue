<script setup lang="ts">
import type { GuideSnippet } from './types'

defineProps<{
  snippets: GuideSnippet[]
  copied: string
  copyLabel: string
  copiedLabel: string
}>()

const emit = defineEmits<{
  copy: [key: string, text: string]
}>()
</script>

<template>
  <div class="grid gap-3 p-4 sm:gap-4 sm:p-5">
    <article
      v-for="item in snippets"
      :key="item.key"
      class="overflow-hidden rounded-xl border border-line/80 bg-ink/25"
    >
      <div class="flex items-center justify-between gap-2 border-b border-line/60 px-3.5 py-2.5 sm:px-4">
        <h3 class="min-w-0 truncate text-base font-medium text-fg">{{ item.title }}</h3>
        <button
          v-if="!item.hideCopy"
          type="button"
          class="ui-btn-ghost shrink-0 !px-2.5 !py-1 text-sm"
          @click="emit('copy', item.key, item.text)"
        >
          {{ copied === item.key ? copiedLabel : copyLabel }}
        </button>
      </div>
      <pre
        class="overflow-x-auto whitespace-pre-wrap break-all px-3.5 py-3 font-mono text-sm leading-relaxed text-muted sm:px-4"
      >{{ item.text }}</pre>
    </article>
  </div>
</template>
