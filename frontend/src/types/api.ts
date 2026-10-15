// API 响应类型定义

export interface CacheStats {
  entries: number
  total_size: number
  max_size: number
  hits: number
  misses: number
  hit_rate: number
  usage_ratio: number
  disk_free_bytes: number
  disk_free_ok: boolean
  water_warn: boolean
  water_crit: boolean
  /** 各平台已用字节 */
  by_platform?: Record<string, number>
  /** 各平台命中次数 */
  hits_by_platform?: Record<string, number>
  /** 各平台未命中次数 */
  misses_by_platform?: Record<string, number>
}

export interface AccessTestCheck {
  name: string
  ok: boolean
  skipped?: boolean
  detail: string
  ms: number
}

export interface AccessTestModule {
  id: string
  ok: boolean
  checks: AccessTestCheck[]
}

export interface AccessTestResult {
  ok: boolean
  modules?: AccessTestModule[]
  /** @deprecated 旧单模块响应 */
  checks?: AccessTestCheck[]
}

export interface TrafficRecord {
  at: string
  ip: string
  method: string
  path: string
  platform: string
  bytes: number
  cache: string
  status: string
}

  export interface TrafficStats {
  upstream_bps: number
  downstream_bps: number
  upstream_total: number
  downstream_total: number
  /** 仅完整 Snapshot 含；/stats 轮询不返回 */
  recent?: TrafficRecord[]
  recent_capacity?: number
}

export interface RateLimiterInfo {
  platform: string
  bandwidth_mbps: number
  effective_bandwidth_mbps?: number
  bandwidth_limited?: boolean
  active_tasks: number
  active_boost: number
  boost_slots: number
  active_conns: number
  prefetch_paused: boolean
  rate_limit_wait_p50: number
  rate_limit_wait_p95: number
}

export interface SystemStats {
  cache: CacheStats
  traffic: TrafficStats
  interactive_active: number
  resume_count: number
  rate_limiters: RateLimiterInfo[]
}

export interface QueueTask {
  id: string
  platform: string
  url: string
  label?: string
  detail?: string
  status: string
  priority: string
  error: string
  error_code?: string
  wait_ms: number | null
  elapsed_ms?: number | null
  bytes_done?: number
  bytes_total?: number
  created_at?: string
  queued_at?: string
  started_at?: string
  updated_at: string
}

export interface QueueResponse {
  tasks: QueueTask[]
  total?: number
  page?: number
  page_size?: number
  running_count?: number
}

export interface AccessListResponse {
  items: TrafficRecord[]
  total: number
  page: number
  page_size: number
  platforms: string[]
  capacity: number
}

export interface PackageSummary {
  name: string
  cached: boolean
  has_index: boolean
  file_count: number
  versions: string[]
}

export interface PackageListResponse {
  packages: PackageSummary[]
  total: number
  page: number
  page_size: number
  catalog_size: number
  catalog_ready: boolean
  catalog_refreshing: boolean
  catalog_error: string
}

export interface PackageFile {
  key: string
  type: string
  kind: string
  version: string
  filename: string
  source_url: string
  size: number
  last_access: string
}

export interface UpstreamInfo {
  versions: string[]
}

export interface PackageDetail {
  package_count: number
  total_size: number
  versions: string[]
  index_versions: string[]
  files: PackageFile[]
  upstream: UpstreamInfo | null
  upstream_error: string
}

export interface PrefetchResponse {
  enqueued: number
  items: string[]
  skipped: string[]
}

export interface LoginResponse {
  token: string
  username: string
}

export interface MeResponse {
  username: string
}

export interface AppConfig {
  server: {
    upstream_proxy: string
  }
  cache: {
    max_size_gb: number
    index_ttl_seconds: number
    package_ttl_seconds: number
    chunk_ttl_hours?: number
  }
  rate_limit: {
    bandwidth_mbps: number
    max_concurrent: number
    max_connections: number
    windows?: { start: string; end: string; bandwidth_mbps?: number }[]
  }
  scheduler: {
    prefetch: {
      idle_quota_ratio: number
      on_interactive: string
      resume_on_idle: boolean
      artifact_mode: string
      extra_wheel_tags: string[]
      target_python: string[]
      target_platforms?: string[]
      target_platform?: string
    }
    small_file_boost: {
      enabled: boolean
      max_size_kb: number
    }
  }
  platforms: Record<string, PlatformConfig>
}

export interface PlatformConfig {
  enabled: boolean
  upstream: string
  file_upstream: string
  metadata_upstream?: string
  upstream_token?: string
  download: {
    concurrency: number
    chunk_size: number
    min_size: number
  }
}

export interface UpdateProgress {
  phase: string
  bytes_done: number
  bytes_total: number
  percent: number
  message?: string
  error?: string
  can_cancel?: boolean
}

export interface UpdateStatus {
  current: string
  latest?: string
  update_available: boolean
  release_url?: string
  asset_name?: string
  error?: string
  checked_at: string
  progress: UpdateProgress
}
