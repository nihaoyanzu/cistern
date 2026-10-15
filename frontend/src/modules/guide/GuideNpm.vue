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

const npmSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'npmGlobal',
    title: t('guide.npmGlobal'),
    text: `npm config set registry ${props.registryURL}`,
  },
  {
    key: 'npmLocal',
    title: t('guide.npmLocal'),
    text: `# 项目 .npmrc\nregistry=${props.registryURL}`,
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
    text: `# 当前终端（Corepack 拉 pnpm 时需要）\nexport COREPACK_NPM_REGISTRY=${props.registryURL}\n\n# 项目 .npmrc\nregistry=${props.registryURL}`,
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
    text: `# 当前终端（Corepack 拉 Yarn 时需要）\nexport COREPACK_NPM_REGISTRY=${props.registryURL}\n\n# 项目 .npmrc 或 .yarnrc\nregistry=${props.registryURL}`,
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
    <div class="flex flex-wrap items-center justify-between gap-3 border-b border-line p-4 sm:p-5">
      <p class="text-xs text-muted whitespace-pre-wrap">{{ t('guide.npmHint') }}</p>
      <div class="flex gap-1 rounded-xl border border-line bg-bg/60 p-1">
        <button
          type="button"
          class="ui-chip"
          :class="{ 'ui-chip-active': npmToolTab === 'npm' }"
          @click="npmToolTab = 'npm'"
        >
          npm
        </button>
        <button
          type="button"
          class="ui-chip"
          :class="{ 'ui-chip-active': npmToolTab === 'pnpm' }"
          @click="npmToolTab = 'pnpm'"
        >
          pnpm
        </button>
        <button
          type="button"
          class="ui-chip"
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
