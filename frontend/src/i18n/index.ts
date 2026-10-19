import { createI18n } from 'vue-i18n'
import zhCN from './zh-CN'
import en from './en'

const STORAGE_KEY = 'cistern_lang'

function detectLocale(): string {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'zh-CN' || saved === 'en') return saved
  } catch {}
  return navigator.language.startsWith('zh') ? 'zh-CN' : 'en'
}

/**
 * vue-i18n 消息语法：`@` 链接、`{name}` 插值、`|` 复数。
 * 文案里的 JSON / image@sha256 等字面量会触发编译错误 → 整页空白。
 * 本项目不用 @:key 链接与 | 复数；保留合法具名插值 {foo}/{0}，其余特殊字符转成字面量。
 */
function escapeI18nLiterals(s: string): string {
  const kept: string[] = []
  const stash = (m: string) => {
    const i = kept.length
    kept.push(m)
    return `\uE000${i}\uE001`
  }

  // 已转义字面量、合法插值先保护起来
  let out = s
    .replace(/\{'[^{}']*'\}/g, stash)
    .replace(/\{[a-zA-Z_][a-zA-Z0-9_]*\}/g, stash)
    .replace(/\{[0-9]+\}/g, stash)

  out = out.replace(/[@{}|]/g, (ch) => `{'${ch}'}`)

  return out.replace(/\uE000(\d+)\uE001/g, (_, i) => kept[Number(i)] ?? '')
}

function sanitizeMessages<T>(input: T): T {
  if (typeof input === 'string') return escapeI18nLiterals(input) as T
  if (Array.isArray(input)) return input.map((v) => sanitizeMessages(v)) as T
  if (input && typeof input === 'object') {
    const out: Record<string, unknown> = {}
    for (const [k, v] of Object.entries(input as Record<string, unknown>)) {
      out[k] = sanitizeMessages(v)
    }
    return out as T
  }
  return input
}

export const i18n = createI18n({
  legacy: false,
  locale: detectLocale(),
  fallbackLocale: 'en',
  messages: {
    'zh-CN': sanitizeMessages(zhCN),
    en: sanitizeMessages(en),
  },
})

export function setLocale(locale: 'zh-CN' | 'en') {
  ;(i18n.global.locale as any).value = locale
  try {
    localStorage.setItem(STORAGE_KEY, locale)
  } catch {}
}

export type SupportedLocale = 'zh-CN' | 'en'
