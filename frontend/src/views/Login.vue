<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter, RouterLink } from 'vue-router'
import { api, setToken } from '@/api/client'
import { useSessionStore } from '@/stores/session'
import { useToastStore } from '@/stores/toast'

const { t } = useI18n()
const router = useRouter()
const route = useRoute()
const session = useSessionStore()
const toast = useToastStore()

const username = ref('admin')
const password = ref('')
const showPwd = ref(false)
const error = ref('')
const loading = ref(false)

async function submit() {
  error.value = ''
  loading.value = true
  try {
    const res = await api.login(username.value.trim(), password.value)
    setToken(res.token)
    await session.refresh()
    toast.ok(t('login.welcome', { name: res.username }))
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/dashboard'
    await router.replace(redirect && redirect !== '/' ? redirect : '/dashboard')
  } catch (e: any) {
    error.value = e.message || t('login.loginFailed')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="relative flex min-h-screen items-center justify-center overflow-hidden px-4">
    <div class="pointer-events-none absolute inset-0">
      <div class="absolute -left-24 top-16 h-72 w-72 rounded-full bg-accent/15 blur-3xl" />
      <div
        class="absolute -right-16 bottom-10 h-80 w-80 rounded-full blur-3xl"
        style="background: var(--bg-glow-b)"
      />
      <div
        class="absolute left-1/2 top-[70%] h-56 w-56 -translate-x-1/2 rounded-full blur-3xl"
        style="background: var(--bg-glow-c)"
      />
    </div>

    <div class="relative w-full max-w-sm">
      <div class="mb-6 text-center">
        <div class="mb-3 flex justify-center">
          <img src="/favicon.svg" alt="MirrorHub" class="h-12 w-12" width="48" height="48" />
        </div>
        <div class="text-4xl font-semibold tracking-wide">
          mirror<span class="text-accent">hub</span>
        </div>
        <p class="mt-2 text-sm uppercase tracking-[0.18em] text-muted">{{ t('login.tagline') }}</p>
      </div>

      <form class="ui-panel p-7 sm:p-8" @submit.prevent="submit">
        <div class="space-y-4">
          <div>
            <label class="ui-label">{{ t('login.username') }}</label>
            <input v-model="username" class="ui-input" autocomplete="username" />
          </div>
          <div>
            <label class="ui-label">{{ t('login.password') }}</label>
            <div class="relative">
              <input
                v-model="password"
                class="ui-input pr-14"
                :type="showPwd ? 'text' : 'password'"
                autocomplete="current-password"
              />
              <button
                type="button"
                class="absolute right-1.5 top-1/2 flex h-8 w-8 -translate-y-1/2 items-center justify-center rounded-md text-muted hover:bg-panel hover:text-fg"
                :aria-label="showPwd ? t('login.hide') : t('login.show')"
                :title="showPwd ? t('login.hide') : t('login.show')"
                @click="showPwd = !showPwd"
              >
                <!-- 密文：睁眼；明文：划线眼 -->
                <svg v-if="!showPwd" class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" aria-hidden="true">
                  <path stroke-linecap="round" stroke-linejoin="round" d="M2.458 12C3.732 7.943 7.523 5 12 5s8.268 2.943 9.542 7c-1.274 4.057-5.064 7-9.542 7s-8.268-2.943-9.542-7z" />
                  <circle cx="12" cy="12" r="3" />
                </svg>
                <svg v-else class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" aria-hidden="true">
                  <path stroke-linecap="round" stroke-linejoin="round" d="M3 3l18 18" />
                  <path stroke-linecap="round" stroke-linejoin="round" d="M10.585 10.587a2 2 0 002.828 2.83" />
                  <path stroke-linecap="round" stroke-linejoin="round" d="M9.363 5.365A9.466 9.466 0 0112 5c4.478 0 8.268 2.943 9.542 7a9.52 9.52 0 01-1.563 3.03M6.228 6.228A9.493 9.493 0 002.458 12c1.274 4.057 5.065 7 9.542 7 1.605 0 3.118-.35 4.47-.98" />
                </svg>
              </button>
            </div>
          </div>
        </div>

        <button type="submit" class="ui-btn-primary mt-5 w-full" :disabled="loading || !password">
          {{ loading ? t('login.loggingIn') : t('login.loginBtn') }}
        </button>
        <p class="mt-4 text-center text-xs text-muted">
          <RouterLink class="text-accent hover:underline" to="/">{{ t('guide.title') }}</RouterLink>
        </p>
        <p v-if="error" class="mt-3 text-sm text-danger">{{ error }}</p>
      </form>
    </div>
  </div>
</template>
