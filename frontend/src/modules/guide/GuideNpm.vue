<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import GuideSnippetList from './GuideSnippetList.vue'
import type { GuideSnippet } from './types'

const props = defineProps<{
  registryURL: string
  copied: string
}>()

const emit = defineEmits<{
  copy: [key: string, text: string]
}>()

const { t } = useI18n()
const npmToolTab = ref<'npm' | 'pnpm' | 'yarn'>('npm')

/** Corepack 拼接包路径时以 / 开头；尾斜杠会导致 //yarn、//@yarnpkg/... 被镜像 404 */
const corepackRegistry = computed(() => props.registryURL.replace(/\/+$/, ''))

const npmSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'npmGlobal',
    title: t('guide.npmGlobal'),
    text: `npm config set registry ${props.registryURL}`,
  },
  {
    key: 'npmLocal',
    title: t('guide.npmLocal'),
    text: `# project .npmrc\nregistry=${props.registryURL}`,
  },
])

const pnpmSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'pnpmGlobal',
    title: t('guide.pnpmGlobal'),
    text: `pnpm config set registry ${props.registryURL}`,
  },
  {
    key: 'pnpmLocal',
    title: t('guide.pnpmLocal'),
    text: `# current shell (Corepack; no trailing slash)\nexport COREPACK_NPM_REGISTRY=${corepackRegistry.value}\n\n# project .npmrc\nregistry=${props.registryURL}`,
  },
])

const yarnSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'yarnGlobal',
    title: t('guide.yarnGlobal'),
    text: `yarn config set registry ${props.registryURL}`,
  },
  {
    key: 'yarnLocal',
    title: t('guide.yarnLocal'),
    text: `# current shell (Corepack; no trailing slash)\nexport COREPACK_NPM_REGISTRY=${corepackRegistry.value}\n\n# project .npmrc or .yarnrc\nregistry=${props.registryURL}`,
  },
])

const snippets = computed(() => {
  if (npmToolTab.value === 'pnpm') return pnpmSnippets.value
  if (npmToolTab.value === 'yarn') return yarnSnippets.value
  return npmSnippets.value
})
</script>

<template>
  <section class="ui-panel overflow-hidden">
    <div class="flex justify-center border-b border-line px-4 py-3 sm:px-5">
      <div class="inline-flex gap-1 rounded-xl border border-line bg-ink/30 p-1">
        <button
          type="button"
          class="ui-chip !py-1.5"
          :class="{ 'ui-chip-active': npmToolTab === 'npm' }"
          @click="npmToolTab = 'npm'"
        >
          npm
        </button>
        <button
          type="button"
          class="ui-chip !py-1.5"
          :class="{ 'ui-chip-active': npmToolTab === 'pnpm' }"
          @click="npmToolTab = 'pnpm'"
        >
          pnpm
        </button>
        <button
          type="button"
          class="ui-chip !py-1.5"
          :class="{ 'ui-chip-active': npmToolTab === 'yarn' }"
          @click="npmToolTab = 'yarn'"
        >
          yarn
        </button>
      </div>
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
