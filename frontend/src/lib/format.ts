export function fmtBytes(n: number): string {
  if (!n) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${u[i]}`
}

export function fmtRate(bps: number): string {
  if (!bps || bps < 1) return '0 B/s'
  return `${fmtBytes(bps)}/s`
}

/** 限速等待 P50/P95：为 0 显示 0，否则最多 3 位小数（单位 ms） */
export function fmtWaitMs(ms: number | null | undefined): string {
  const n = Number(ms)
  if (!Number.isFinite(n) || n <= 0) return '0'
  return n.toFixed(3)
}

/**
 * 将毫秒格式化为可读时长：精确到下一单位（如 2小时15分钟 / 3分钟20秒），不下沉到毫秒。
 * 排队中尚未满 1 秒时显示「不到1秒」。
 */
export function fmtDurationMs(ms: number | null | undefined, locale?: string): string {
  if (ms == null || Number.isNaN(Number(ms)) || ms < 0) return '-'
  const zh = !locale || String(locale).toLowerCase().startsWith('zh')
  const totalSec = Math.floor(Number(ms) / 1000)
  if (totalSec < 1) return zh ? '不到1秒' : '<1s'

  const days = Math.floor(totalSec / 86400)
  const hours = Math.floor((totalSec % 86400) / 3600)
  const mins = Math.floor((totalSec % 3600) / 60)
  const secs = totalSec % 60

  if (days > 0) {
    return zh ? `${days}天${hours}小时` : `${days}d ${hours}h`
  }
  if (hours > 0) {
    return zh ? `${hours}小时${mins}分钟` : `${hours}h ${mins}min`
  }
  if (mins > 0) {
    return zh ? `${mins}分钟${secs}秒` : `${mins}min ${secs}s`
  }
  return zh ? `${secs}秒` : `${secs}s`
}

export function fmtTime(v: string | number | Date | null | undefined): string {
  if (v == null || v === '') return '-'
  // Go RFC3339Nano 小数位常超过 3 位，部分环境 Date 解析会失败
  const raw = typeof v === 'string' ? v.replace(/(\.\d{3})\d+/, '$1') : v
  const d = new Date(raw)
  if (Number.isNaN(d.getTime())) return '-'
  return d.toLocaleString()
}

export function hitRate(hits: number, misses: number): string {
  const total = hits + misses
  if (!total) return '-'
  return `${((hits / total) * 100).toFixed(1)}%`
}
