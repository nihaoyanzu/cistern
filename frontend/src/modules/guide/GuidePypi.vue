<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import GuideSnippetList from './GuideSnippetList.vue'
import type { GuideSnippet } from './types'

const props = defineProps<{
  indexURL: string
  trustedHost: string
  copied: string
}>()

const emit = defineEmits<{
  copy: [key: string, text: string]
}>()

const { t } = useI18n()
const toolTab = ref<'pip' | 'uv'>('pip')

const pipSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'pipOnce',
    title: t('guide.pipOnce'),
    text: `pip install <pkg> -i ${props.indexURL} --trusted-host ${props.trustedHost}`,
  },
  {
    key: 'pipConfig',
    title: t('guide.pipConfig'),
    text: `pip config set global.index-url ${props.indexURL}\npip config set global.trusted-host ${props.trustedHost}`,
  },
])

const uvSnippets = computed<GuideSnippet[]>(() => [
  {
    key: 'uvOnce',
    title: t('guide.uvOnce'),
    text: `uv pip install <pkg> -i ${props.indexURL}`,
  },
  {
    key: 'uvConfig',
    title: t('guide.uvConfig'),
    text: `# Linux / macOS: ~/.config/uv/uv.toml\n# Windows: %APPDATA%\\uv\\uv.toml\n\n[[index]]\nurl = "${props.indexURL}"\ndefault = true\n\nallow-insecure-host = ["${props.trustedHost}"]`,
  },
  {
    key: 'uvEnv',
    title: t('guide.uvEnv'),
    text: `# Linux / macOS\nexport UV_DEFAULT_INDEX=${props.indexURL}\n\n# Windows PowerShell\n$env:UV_DEFAULT_INDEX="${props.indexURL}"`,
  },
])

const snippets = computed(() => (toolTab.value === 'pip' ? pipSnippets.value : uvSnippets.value))
</script>

<template>
  <section class="ui-panel overflow-hidden">
    <div class="flex justify-center border-b border-line px-4 py-3 sm:px-5">
      <div class="inline-flex gap-1 rounded-xl border border-line bg-ink/30 p-1">
        <button
          type="button"
          class="ui-chip !py-1.5"
          :class="{ 'ui-chip-active': toolTab === 'pip' }"
          @click="toolTab = 'pip'"
        >
          pip
        </button>
        <button
          type="button"
          class="ui-chip !py-1.5"
          :class="{ 'ui-chip-active': toolTab === 'uv' }"
          @click="toolTab = 'uv'"
        >
          uv
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
