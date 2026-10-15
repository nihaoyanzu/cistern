package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/livehl/mirrorhub/internal/store"
)

// Bootstrap 仅来自 .env，进程启动必需
type Bootstrap struct {
	ProxyAddr   string
	AdminAddr   string
	DataDir     string
	DatabaseURL string
	WebDir      string // 管理台静态资源目录；空则不托管前端
	LogLevel    string
}

// RuntimeSettings 存运营库，由管理页维护
type RuntimeSettings struct {
	UpstreamProxy string                    `json:"upstream_proxy"`
	Cache         CacheConfig               `json:"cache"`
	RateLimit     RateLimitConfig           `json:"rate_limit"`
	Scheduler     SchedulerConfig           `json:"scheduler"`
	Platforms     map[string]PlatformConfig `json:"platforms"`
}

type Config struct {
	Server    ServerConfig              `json:"server"`
	Cache     CacheConfig               `json:"cache"`
	RateLimit RateLimitConfig           `json:"rate_limit"`
	Scheduler SchedulerConfig           `json:"scheduler"`
	Platforms map[string]PlatformConfig `json:"platforms"`
	Logging   LoggingConfig             `json:"logging"`
}

type ServerConfig struct {
	ProxyAddr     string `json:"proxy_addr"`
	AdminAddr     string `json:"admin_addr"`
	UpstreamProxy string `json:"upstream_proxy"`
	// PublicHost 仅请求期内由代理按访问 Host 填入，用于索引改写；不持久化、不对外暴露。
	PublicHost string `json:"-"`
}

type CacheConfig struct {
	Dir               string  `json:"dir"`
	MaxSizeGB         float64 `json:"max_size_gb"`
	IndexTTLSeconds   int     `json:"index_ttl_seconds"`
	PackageTTLSeconds int     `json:"package_ttl_seconds"` // 0=永不过期（推荐；wheel/sdist 不可变）
	ChunkTTLHours     int     `json:"chunk_ttl_hours"`
}

type SchedulerConfig struct {
	Prefetch       PrefetchConfig       `json:"prefetch"`
	SmallFileBoost SmallFileBoostConfig `json:"small_file_boost"`
}

type PrefetchConfig struct {
	IdleQuotaRatio  float64  `json:"idle_quota_ratio"`
	OnInteractive   string   `json:"on_interactive"`
	ResumeOnIdle    bool     `json:"resume_on_idle"`
	ArtifactMode    string   `json:"artifact_mode"`    // portable | all；默认 portable
	ExtraWheelTags  []string `json:"extra_wheel_tags"` // 组织预热标签子串，非本机
	TargetPython    []string `json:"target_python"`    // 目标 Python 版本列表，如 ["3.10","3.12"]
	TargetPlatforms []string `json:"target_platforms"` // 多选：linux / linux-arm / win32 / win-arm / darwin / darwin-arm
	TargetPlatform  string   `json:"target_platform"`  // 兼容旧单值；加载时并入 TargetPlatforms

	// resumeOnIdlePresent：JSON 是否显式包含 resume_on_idle（缺省则 applyDefaults 置 true）
	resumeOnIdlePresent bool `json:"-"`
}

// UnmarshalJSON 识别 resume_on_idle 是否显式出现，避免旧配置缺字段被当成 false。
func (p *PrefetchConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, p.resumeOnIdlePresent = raw["resume_on_idle"]
	type alias PrefetchConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = PrefetchConfig(a)
	_, p.resumeOnIdlePresent = raw["resume_on_idle"]
	return nil
}

// TargetPythonVersions 返回配置的目标 Python 版本列表；空则返回默认 ["3.10","3.11","3.12"]。
func (p PrefetchConfig) TargetPythonVersions() []string {
	if len(p.TargetPython) > 0 {
		return p.TargetPython
	}
	return []string{"3.10", "3.11", "3.12"}
}

// TargetPlatformList 返回规范化后的目标平台多选列表。
func (p PrefetchConfig) TargetPlatformList() []string {
	return NormalizeTargetPlatforms(append(append([]string{}, p.TargetPlatforms...), splitPlatformField(p.TargetPlatform)...))
}

type SmallFileBoostConfig struct {
	Enabled   bool `json:"enabled"`
	MaxSizeKB int  `json:"max_size_kb"`
}

type PlatformConfig struct {
	Enabled          bool           `json:"enabled"`
	Upstream         string         `json:"upstream"`
	FileUpstream     string         `json:"file_upstream"`
	MetadataUpstream string         `json:"metadata_upstream"` // PEP 658；为空时回退到 file_upstream。
	UpstreamToken    string         `json:"upstream_token,omitempty"` // 可选；HF 等回源 Bearer
	Download         DownloadConfig `json:"download"`
	// RateLimit 已迁到全局 Config.RateLimit；指针 + omitempty 避免零值结构体仍被编码。
	RateLimit *RateLimitConfig `json:"rate_limit,omitempty"`
}

type RateLimitConfig struct {
	BandwidthMbps  float64           `json:"bandwidth_mbps"` // 默认带宽；0=默认不限速（仅约束预取）
	MaxConcurrent  int               `json:"max_concurrent"`
	MaxConnections int               `json:"max_connections"`
	Windows        []RateLimitWindow `json:"windows"` // 命中时段覆盖默认带宽
}

// RateLimitWindow 本地时区 HH:MM 时段；跨午夜时 start > end。
// BandwidthMbps：该时段带宽；0=该时段不限速。
type RateLimitWindow struct {
	Start         string  `json:"start"`
	End           string  `json:"end"`
	BandwidthMbps float64 `json:"bandwidth_mbps"`
}

// EffectiveBandwidthMbps 当前应使用的带宽上限（Mbps）；0 表示不限速。
// 命中任一窗口用窗口值，否则用默认 BandwidthMbps。
func (c RateLimitConfig) EffectiveBandwidthMbps(t time.Time) float64 {
	hm := t.Format("15:04")
	for _, w := range c.Windows {
		if InHMWindow(hm, NormalizeHM(w.Start), NormalizeHM(w.End)) {
			if w.BandwidthMbps > 0 {
				return w.BandwidthMbps
			}
			return 0
		}
	}
	if c.BandwidthMbps > 0 {
		return c.BandwidthMbps
	}
	return 0
}

// BandwidthLimitedAt 此刻是否做带宽整形。
func (c RateLimitConfig) BandwidthLimitedAt(t time.Time) bool {
	return c.EffectiveBandwidthMbps(t) > 0
}

// NormalizeHM 将 H:MM / HH:MM 规范为 HH:MM；非法则原样返回。
func NormalizeHM(s string) string {
	s = strings.TrimSpace(s)
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return s
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return s
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

// InHMWindow 判断 now（HH:MM）是否落在 [start, end]；支持跨午夜。
func InHMWindow(now, start, end string) bool {
	if start == "" || end == "" {
		return false
	}
	if start <= end {
		return now >= start && now <= end
	}
	return now >= start || now <= end
}

type DownloadConfig struct {
	Concurrency int   `json:"concurrency"`
	ChunkSize   int64 `json:"chunk_size"`
	MinSize     int64 `json:"min_size"`
}

type LoggingConfig struct {
	Level string `json:"level"`
}

const settingsKey = "runtime"

type Manager struct {
	mu   sync.RWMutex
	boot Bootstrap
	cfg  Config
	db   *store.Store
}

func LoadBootstrap() Bootstrap {
	dataDir := envOr("MIRRORHUB_DATA_DIR", "./data")
	return Bootstrap{
		ProxyAddr:   envOr("MIRRORHUB_PROXY_ADDR", ":8081"),
		AdminAddr:   envOr("MIRRORHUB_ADMIN_ADDR", ":8082"),
		DataDir:     dataDir,
		DatabaseURL: os.Getenv("DATABASE_URL"),
		WebDir:      strings.TrimSpace(os.Getenv("MIRRORHUB_WEB_DIR")),
		LogLevel:    envOr("MIRRORHUB_LOG_LEVEL", "info"),
	}
}

func Open(boot Bootstrap) (*Manager, error) {
	db, err := store.Open(boot.DatabaseURL, boot.DataDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{boot: boot, db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var rt RuntimeSettings
	ok, err := db.GetJSON(ctx, settingsKey, &rt)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !ok {
		rt = defaultRuntime()
		if err := db.PutJSON(ctx, settingsKey, rt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("初始化运营配置失败: %w", err)
		}
	}
	m.cfg = merge(boot, rt)
	applyDefaults(&m.cfg)
	// 将平台级 rate_limit 迁移结果写回，避免双源
	if err := m.persistLocked(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("持久化运营配置失败: %w", err)
	}
	return m, nil
}

func (m *Manager) Store() *store.Store {
	return m.db
}

func (m *Manager) Close() error {
	if m.db == nil {
		return nil
	}
	return m.db.Close()
}

func (m *Manager) Bootstrap() Bootstrap {
	return m.boot
}

func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Manager) Update(fn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.cfg)
	applyDefaults(&m.cfg)
	return m.persistLocked()
}

func (m *Manager) ReplaceRateLimit(rl RateLimitConfig) error {
	return m.Update(func(c *Config) {
		c.RateLimit = rl
	})
}

func (m *Manager) ReplaceScheduler(s SchedulerConfig) error {
	return m.Update(func(c *Config) {
		c.Scheduler = s
	})
}

func (m *Manager) persistLocked() error {
	// 持久化时去掉平台级 rate_limit，避免双源
	platforms := make(map[string]PlatformConfig, len(m.cfg.Platforms))
	for name, p := range m.cfg.Platforms {
		p.RateLimit = nil
		platforms[name] = p
	}
	rt := RuntimeSettings{
		UpstreamProxy: m.cfg.Server.UpstreamProxy,
		Cache: CacheConfig{
			MaxSizeGB:         m.cfg.Cache.MaxSizeGB,
			IndexTTLSeconds:   m.cfg.Cache.IndexTTLSeconds,
			PackageTTLSeconds: m.cfg.Cache.PackageTTLSeconds,
			ChunkTTLHours:     m.cfg.Cache.ChunkTTLHours,
		},
		RateLimit: m.cfg.RateLimit,
		Scheduler: m.cfg.Scheduler,
		Platforms: platforms,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.db.PutJSON(ctx, settingsKey, rt)
}

func merge(boot Bootstrap, rt RuntimeSettings) Config {
	cacheDir := filepath.Join(boot.DataDir, "cache")
	cfg := Config{
		Server: ServerConfig{
			ProxyAddr:     boot.ProxyAddr,
			AdminAddr:     boot.AdminAddr,
			UpstreamProxy: rt.UpstreamProxy,
		},
		Cache: CacheConfig{
			Dir:               cacheDir,
			MaxSizeGB:         rt.Cache.MaxSizeGB,
			IndexTTLSeconds:   rt.Cache.IndexTTLSeconds,
			PackageTTLSeconds: rt.Cache.PackageTTLSeconds,
			ChunkTTLHours:     rt.Cache.ChunkTTLHours,
		},
		RateLimit: rt.RateLimit,
		Scheduler: rt.Scheduler,
		Platforms: rt.Platforms,
		Logging:   LoggingConfig{Level: boot.LogLevel},
	}
	return cfg
}

func defaultRuntime() RuntimeSettings {
	return RuntimeSettings{
		UpstreamProxy: "",
		Cache: CacheConfig{
			MaxSizeGB:         100,
			IndexTTLSeconds:   604800, // 7 天
			PackageTTLSeconds: 0,      // 制品永不过期
			ChunkTTLHours:     48,
		},
		Scheduler: SchedulerConfig{
			Prefetch: PrefetchConfig{
				IdleQuotaRatio:  0.3,
				OnInteractive:   "pause",
				ResumeOnIdle:    true,
				ArtifactMode:    "portable",
				TargetPython:    []string{"3.10", "3.11", "3.12"},
				TargetPlatforms: []string{defaultTargetPlatform()},
				TargetPlatform:  defaultTargetPlatform(),
			},
			SmallFileBoost: SmallFileBoostConfig{Enabled: true, MaxSizeKB: 512},
		},
		Platforms: map[string]PlatformConfig{
			"pypi": {
				Enabled:          true,
				Upstream:         "https://mirrors.aliyun.com/pypi",
				FileUpstream:     "https://mirrors.aliyun.com/pypi",
				MetadataUpstream: "https://files.pythonhosted.org",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
			"npm": {
				Enabled:      true,
				Upstream:     "https://registry.npmmirror.com",
				FileUpstream: "https://registry.npmmirror.com",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
			"docker": {
				Enabled:          true,
				Upstream:         "https://docker.m.daocloud.io",
				FileUpstream:     "https://docker.m.daocloud.io",
				MetadataUpstream: "https://auth.docker.io",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
			"goproxy": {
				Enabled:          true,
				Upstream:         "https://goproxy.cn",
				FileUpstream:     "https://goproxy.cn",
				MetadataUpstream: "https://goproxy.cn",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
			"huggingface": {
				Enabled:      true,
				Upstream:     "https://hf-mirror.com",
				FileUpstream: "https://hf-mirror.com",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
			"maven": {
				Enabled:      true,
				Upstream:     "https://maven.aliyun.com/repository/central",
				FileUpstream: "https://maven.aliyun.com/repository/central",
				Download: DownloadConfig{
					Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024,
				},
			},
		},
		RateLimit: RateLimitConfig{
			BandwidthMbps: 0, MaxConcurrent: 20, MaxConnections: 80,
			Windows: []RateLimitWindow{},
		},
	}
}

func applyDefaults(cfg *Config) {
	if cfg.Server.ProxyAddr == "" {
		cfg.Server.ProxyAddr = ":8081"
	}
	if cfg.Server.AdminAddr == "" {
		cfg.Server.AdminAddr = ":8082"
	}
	if cfg.Cache.Dir == "" {
		cfg.Cache.Dir = "./data/cache"
	}
	if cfg.Cache.MaxSizeGB <= 0 {
		cfg.Cache.MaxSizeGB = 100
	}
	if cfg.Cache.IndexTTLSeconds <= 0 {
		cfg.Cache.IndexTTLSeconds = 604800 // 7 天
	}
	// PackageTTLSeconds：0=永不过期；负值夹成 0。制品在 cache.Get 对 kind=package 亦不按 TTL 失效。
	if cfg.Cache.PackageTTLSeconds < 0 {
		cfg.Cache.PackageTTLSeconds = 0
	}
	if cfg.Cache.ChunkTTLHours <= 0 {
		cfg.Cache.ChunkTTLHours = 48
	}
	if cfg.Scheduler.Prefetch.IdleQuotaRatio <= 0 {
		cfg.Scheduler.Prefetch.IdleQuotaRatio = 0.3
	}
	if cfg.Scheduler.Prefetch.OnInteractive == "" {
		cfg.Scheduler.Prefetch.OnInteractive = "pause"
	}
	// 旧库缺 resume_on_idle 时 Go 零值为 false，会导致 pause 后永不清；与前端 !== false 对齐
	if !cfg.Scheduler.Prefetch.resumeOnIdlePresent {
		cfg.Scheduler.Prefetch.ResumeOnIdle = true
		cfg.Scheduler.Prefetch.resumeOnIdlePresent = true
	}
	if cfg.Scheduler.Prefetch.ArtifactMode == "" {
		cfg.Scheduler.Prefetch.ArtifactMode = "portable"
	}
	if len(cfg.Scheduler.Prefetch.TargetPython) == 0 {
		cfg.Scheduler.Prefetch.TargetPython = []string{"3.10", "3.11", "3.12"}
	}
	// 旧单值 target_platform → target_platforms；并规范化
	cfg.Scheduler.Prefetch.TargetPlatforms = cfg.Scheduler.Prefetch.TargetPlatformList()
	if len(cfg.Scheduler.Prefetch.TargetPlatforms) > 0 {
		cfg.Scheduler.Prefetch.TargetPlatform = cfg.Scheduler.Prefetch.TargetPlatforms[0]
	} else {
		cfg.Scheduler.Prefetch.TargetPlatforms = []string{defaultTargetPlatform()}
		cfg.Scheduler.Prefetch.TargetPlatform = defaultTargetPlatform()
	}
	if cfg.Scheduler.SmallFileBoost.MaxSizeKB <= 0 {
		cfg.Scheduler.SmallFileBoost.MaxSizeKB = 512
	}
	if cfg.Platforms == nil {
		cfg.Platforms = map[string]PlatformConfig{}
	}
	// 旧配置：平台级 rate_limit → 全局桶
	if cfg.RateLimit.BandwidthMbps <= 0 {
		if p, ok := cfg.Platforms["pypi"]; ok && p.RateLimit != nil && p.RateLimit.BandwidthMbps > 0 {
			cfg.RateLimit = *p.RateLimit
		}
	}
	if cfg.RateLimit.MaxConcurrent <= 0 {
		cfg.RateLimit.MaxConcurrent = 20
	}
	if cfg.RateLimit.MaxConnections <= 0 {
		cfg.RateLimit.MaxConnections = 80
	}
	// BandwidthMbps=0 表示默认不限速，不再回填 50
	if cfg.RateLimit.Windows == nil {
		cfg.RateLimit.Windows = []RateLimitWindow{}
	} else {
		normed := make([]RateLimitWindow, 0, len(cfg.RateLimit.Windows))
		for _, w := range cfg.RateLimit.Windows {
			s, e := NormalizeHM(w.Start), NormalizeHM(w.End)
			if s == "" || e == "" {
				continue
			}
			mbps := w.BandwidthMbps
			if mbps < 0 {
				mbps = 0
			}
			normed = append(normed, RateLimitWindow{Start: s, End: e, BandwidthMbps: mbps})
		}
		cfg.RateLimit.Windows = normed
	}

	// 缺平台条目时补默认（初始化）；已有条目只做空字段归一化
	for _, id := range []string{"pypi", "npm", "docker", "goproxy", "huggingface", "maven"} {
		if _, ok := cfg.Platforms[id]; !ok {
			cfg.Platforms[id] = defaultPlatformSeed(id)
		}
		cfg.Platforms[id] = NormalizePlatform(id, cfg.Platforms[id])
	}

	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
}

// defaultPlatformSeed 新建配置时的平台初始条目（含 Enabled）。
func defaultPlatformSeed(id string) PlatformConfig {
	dl := DownloadConfig{Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024}
	switch id {
	case "pypi":
		return PlatformConfig{
			Enabled: true, Upstream: "https://mirrors.aliyun.com/pypi", FileUpstream: "https://mirrors.aliyun.com/pypi",
			MetadataUpstream: "https://files.pythonhosted.org", Download: dl,
		}
	case "npm":
		return PlatformConfig{
			Enabled: true, Upstream: "https://registry.npmmirror.com", FileUpstream: "https://registry.npmmirror.com", Download: dl,
		}
	case "docker":
		return PlatformConfig{
			Enabled: true, Upstream: "https://docker.m.daocloud.io", FileUpstream: "https://docker.m.daocloud.io",
			MetadataUpstream: "https://auth.docker.io", Download: dl,
		}
	case "goproxy":
		return PlatformConfig{
			Enabled: true, Upstream: "https://goproxy.cn", FileUpstream: "https://goproxy.cn",
			MetadataUpstream: "https://goproxy.cn", Download: dl,
		}
	case "huggingface":
		return PlatformConfig{
			Enabled: true, Upstream: "https://hf-mirror.com", FileUpstream: "https://hf-mirror.com", Download: dl,
		}
	case "maven":
		return PlatformConfig{
			Enabled: true, Upstream: "https://maven.aliyun.com/repository/central",
			FileUpstream: "https://maven.aliyun.com/repository/central", Download: dl,
		}
	default:
		return PlatformConfig{Download: dl}
	}
}

// NormalizePlatform 空上游字段填该平台当前默认；FileUpstream 空则跟 Upstream。
// 供 applyDefaults 与探测草稿共用；不含旧 URL 迁移。
func NormalizePlatform(id string, p PlatformConfig) PlatformConfig {
	seed := defaultPlatformSeed(id)
	if p.Download.Concurrency <= 0 {
		p.Download.Concurrency = seed.Download.Concurrency
	}
	if p.Download.ChunkSize <= 0 {
		p.Download.ChunkSize = seed.Download.ChunkSize
	}
	if p.Download.MinSize <= 0 {
		p.Download.MinSize = seed.Download.MinSize
	}
	if strings.TrimSpace(p.Upstream) == "" {
		p.Upstream = seed.Upstream
	}
	if strings.TrimSpace(p.FileUpstream) == "" {
		p.FileUpstream = p.Upstream
	}
	switch id {
	case "pypi":
		if strings.TrimSpace(p.MetadataUpstream) == "" {
			p.MetadataUpstream = seed.MetadataUpstream
		}
	case "docker":
		if strings.TrimSpace(p.MetadataUpstream) == "" {
			p.MetadataUpstream = seed.MetadataUpstream
		}
	case "goproxy":
		if strings.TrimSpace(p.MetadataUpstream) == "" {
			p.MetadataUpstream = p.Upstream
		}
	}
	p.RateLimit = nil
	return p
}

func defaultTargetPlatform() string {
	return "linux"
}

func splitPlatformField(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '|' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// NormalizeTargetPlatforms 规范化并去重目标平台；空输入回退 linux。
func NormalizeTargetPlatforms(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, raw := range in {
		p := NormalizeTargetPlatform(raw)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{defaultTargetPlatform()}
	}
	return out
}

// NormalizeTargetPlatform 将别名归一为：linux / linux-arm / win32 / win-arm / darwin / darwin-arm。
func NormalizeTargetPlatform(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "", "linux", "linux-amd64", "linux-x86-64", "linux-x64", "manylinux":
		return "linux"
	case "linux-arm", "linux-aarch64", "linux-arm64", "aarch64", "arm64", "armv7", "armv7l":
		return "linux-arm"
	case "win", "win32", "windows", "win-amd64", "win-x64":
		return "win32"
	case "win-arm", "win-arm64", "windows-arm", "windows-arm64":
		return "win-arm"
	case "macos", "mac", "darwin", "osx", "macosx", "darwin-amd64", "darwin-x64":
		return "darwin"
	case "darwin-arm", "darwin-arm64", "macos-arm", "macos-arm64", "macosx-arm64", "osx-arm64":
		return "darwin-arm"
	default:
		return ""
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
