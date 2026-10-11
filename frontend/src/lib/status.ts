/** 任务 / 缓存状态 — 返回 i18n key，由组件侧 t() 翻译 */

export function statusKey(s: string): string {
  switch (s) {
    case 'running': return 'status.running'
    case 'preparing': return 'status.preparing'
    case 'done': return 'status.done'
    case 'error': return 'status.error'
    case 'cancelled': return 'status.cancelled'
    case 'queued': return 'status.queued'
    default: return s || '-'
  }
}

export function statusClass(s: string): string {
  if (s === 'running') return 'ui-badge-ok'
  if (s === 'preparing') return 'ui-badge-warn'
  if (s === 'error') return 'ui-badge-danger'
  if (s === 'cancelled') return 'ui-badge-warn'
  if (s === 'queued') return 'ui-badge'
  return 'ui-badge-muted'
}

export function prioClass(p: string): string {
  if (p === 'P0' || p === 'P0+') return 'ui-badge-warn'
  if (p === 'P1') return 'ui-badge-ok'
  if (p === 'P2') return 'ui-badge-muted'
  return 'ui-badge'
}

export function prioKey(p: string): string {
  if (p === 'P0+') return 'status.p0Boost'
  if (p === 'P0') return 'status.p0Interactive'
  if (p === 'P1') return 'status.p1Resume'
  if (p === 'P2') return 'status.p2Prefetch'
  return p || '-'
}

export function cacheKey(c: string): string {
  const v = (c || '').toLowerCase()
  if (v === 'hit') return 'status.hit'
  if (v === 'stale') return 'status.stale'
  if (v === 'revalidated') return 'status.revalidated'
  if (v === 'miss') return 'status.miss'
  if (v === 'timeout') return 'status.timeout'
  if (v === 'cancelled') return 'status.cancelled'
  if (v === 'na' || !v) return '-'
  return c
}

export function cacheClass(c: string): string {
  const v = (c || '').toLowerCase()
  if (v === 'hit' || v === 'revalidated') return 'ui-badge-ok'
  if (v === 'stale') return 'ui-badge-warn'
  if (v === 'miss' || v === 'timeout') return 'ui-badge-warn'
  if (v === 'cancelled') return 'ui-badge-muted'
  return 'ui-badge-muted'
}
