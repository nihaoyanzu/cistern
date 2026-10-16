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
    key: 'mavenSettings',
    title: t('guide.mavenSettings'),
    text: `<settings>
  <mirrors>
    <mirror>
      <id>mirrorhub</id>
      <mirrorOf>*</mirrorOf>
      <url>${props.baseURL}/</url>
    </mirror>
  </mirrors>
</settings>`,
  },
  {
    key: 'mavenGradle',
    title: t('guide.mavenGradle'),
    text: `repositories {
  maven { url = uri("${props.baseURL}/") }
}`,
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
