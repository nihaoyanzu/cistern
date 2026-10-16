<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import GuideSnippetList from './GuideSnippetList.vue'
import type { GuideSnippet } from './types'

const props = defineProps<{
  baseURL: string
  copied: string
}>()

const emit = defineEmits<{
  copy: [key: string, text: string]
}>()

const { t } = useI18n()

const snippets = computed<GuideSnippet[]>(() => [
  {
    key: 'goproxyGlobal',
    title: t('guide.goproxyGlobal'),
    text: `go env -w GOPROXY=${props.baseURL},direct\n# optional: private modules stay direct\n# go env -w GOPRIVATE=*.example.com\n# offline (cached modules only)\n# go env -w GOPROXY=${props.baseURL},off`,
  },
  {
    key: 'goproxyLocal',
    title: t('guide.goproxyLocal'),
    text: `# current shell\nexport GOPROXY=${props.baseURL},direct\n# optional: export GOPRIVATE=*.example.com\n\n# one-off\nGOPROXY=${props.baseURL},direct go mod download\n\n# offline (cached modules only)\n# export GOPROXY=${props.baseURL},off`,
  },
])
</script>

<template>
  <section class="ui-panel overflow-hidden">
    <GuideSnippetList
      :snippets="snippets"
      :copied="copied"
      :copy-label="t('guide.copy')"
      :copied-label="t('guide.copied')"
      @copy="(k, text) => emit('copy', k, text)"
    />
  </section>
</template>
