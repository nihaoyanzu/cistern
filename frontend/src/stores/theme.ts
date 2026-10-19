import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

export type ThemeMode = 'dark' | 'light' | 'system'

const STORAGE_KEY = 'cistern_theme'

function systemPrefersDark(): boolean {
  return window.matchMedia('(prefers-color-scheme: dark)').matches
}

function applyTheme(mode: ThemeMode) {
  const effective = mode === 'system' ? (systemPrefersDark() ? 'dark' : 'light') : mode
  const el = document.documentElement
  el.classList.remove('dark', 'light')
  el.classList.add(effective)
  el.style.colorScheme = effective
}

export const useThemeStore = defineStore('theme', () => {
  const mode = ref<ThemeMode>('dark')

  const isDark = computed(() => {
    if (mode.value === 'dark') return true
    if (mode.value === 'light') return false
    return systemPrefersDark()
  })

  function init() {
    try {
      const saved = localStorage.getItem(STORAGE_KEY) as ThemeMode | null
      if (saved === 'dark' || saved === 'light' || saved === 'system') {
        mode.value = saved
      }
    } catch {}
    applyTheme(mode.value)
    window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
      if (mode.value === 'system') applyTheme('system')
    })
  }

  function set(m: ThemeMode) {
    mode.value = m
    applyTheme(m)
    try {
      localStorage.setItem(STORAGE_KEY, m)
    } catch {}
  }

  function toggle() {
    const cur = mode.value === 'system' ? (systemPrefersDark() ? 'dark' : 'light') : mode.value
    set(cur === 'dark' ? 'light' : 'dark')
  }

  return { mode, isDark, init, set, toggle }
})
