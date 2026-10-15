<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import GuideSnippetList from './GuideSnippetList.vue'
import type { GuideSnippet } from './types'

const props = defineProps<{
  baseURL: string
  displayHost: string
  copied: string
}>()

const emit = defineEmits<{
  copy: [key: string, text: string]
}>()

const { t } = useI18n()

const snippets = computed<GuideSnippet[]>(() => [
  {
    key: 'dockerDaemon',
    title: t('guide.dockerDaemon'),
    text: `{\n  "registry-mirrors": ["${props.baseURL}"],\n  "insecure-registries": ["${props.displayHost}"]\n}`,
  },
])
</script>

<template>
  <section class="ui-panel overflow-hidden">
    <div class="border-b border-line p-4 sm:p-5">
      <p class="text-xs text-muted whitespace-pre-wrap">{{ t('guide.dockerHint') }}</p>
    </div>
    <GuideSnippetList
      :snippets="snippets"
      :copied="copied"
      :copy-label="t('guide.copy')"
      :copied-label="t('guide.copied')"
      @copy="(k, text) => emit('copy', k, text)"
    />
  </section>
</template>
