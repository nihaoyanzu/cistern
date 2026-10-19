package ratelimit

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"

	"github.com/nihaoyanzu/cistern/internal/config"
)

type TrafficClass int

const (
	ClassPrefetch TrafficClass = iota
	ClassInteractive
)

type PlatformLimiter struct {
	name           string
	bandwidth      *rate.Limiter // 预取桶
	connSem        chan struct{}
	connReserved   int // 非交互最多占用 maxConn-reserved
	maxBoost       int
	mu             sync.Mutex
	cfg            config.RateLimitConfig
	activeTasks    int
	activeBoost    int
	activeConns    int
	prefetchPaused bool
	// taskReserve：OnInteractive=continue 时非交互任务槽保留；pause 模式为 false
	taskReserve bool
	waitSamples []float64 // 秒，滑动窗口
}

type Registry struct {
	mu          sync.RWMutex
	byName      map[string]*PlatformLimiter
	cfg         config.RateLimitConfig
	idleRatio   float64
	taskReserve bool // continue 模式为 true
}

var (
	RateLimitWaitSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cistern_rate_limit_wait_seconds",
		Help:    "Time spent in WaitN",
		Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.5, 1, 2, 5},
	}, []string{"platform", "class"})
)

func init() {
	prometheus.MustRegister(RateLimitWaitSeconds)
}

func NewRegistry(cfg config.RateLimitConfig, idleRatio float64) *Registry {
	r := &Registry{
		byName:    map[string]*PlatformLimiter{},
		cfg:       cfg,
		idleRatio: idleRatio,
	}
	if r.idleRatio <= 0 {
		r.idleRatio = 0.3
	}
	// 预创建常用平台，便于 stats 一开始就有条目
	for _, name := range []string{"pypi", "npm", "docker", "goproxy", "huggingface", "maven"} {
		r.byName[name] = newPlatformLimiter(name, cfg)
	}
	return r
}

func newPlatformLimiter(name string, cfg config.RateLimitConfig) *PlatformLimiter {
	bytesPerSec := cfg.BandwidthMbps * 1024 * 1024 / 8
	var lim *rate.Limiter
	if bytesPerSec <= 0 {
		lim = rate.NewLimiter(rate.Inf, 1<<20)
	} else {
		lim = rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec))
	}
	maxTask := cfg.MaxConcurrent
	if maxTask <= 0 {
		maxTask = 20
	}
	maxConn := cfg.MaxConnections
	if maxConn <= 0 {
		maxConn = 80
	}
	boostSlots := boostSlotsFor(maxTask)
	reserved := connReservedFor(maxConn)
	cfg.MaxConcurrent = maxTask
	cfg.MaxConnections = maxConn
	return &PlatformLimiter{
		name:         name,
		bandwidth:    lim,
		connSem:      make(chan struct{}, maxConn),
		cfg:          cfg,
		connReserved: reserved,
		maxBoost:     boostSlots,
	}
}

func boostSlotsFor(maxTask int) int {
	boostSlots := maxTask / 5
	if boostSlots < 2 {
		boostSlots = 2
	}
	if boostSlots > maxTask {
		boostSlots = maxTask
	}
	return boostSlots
}

func connReservedFor(maxConn int) int {
	reserved := maxConn / 10
	if reserved < 8 {
		reserved = 8
	}
	if reserved >= maxConn {
		reserved = maxConn / 2
		if reserved < 1 {
			reserved = 1
		}
	}
	return reserved
}

// resizeSem 按已占用 held 重建信号量：held>cap 时槽位填满，新 Acquire 需等旧任务结束收窄。
func resizeSem(held, capacity int) chan struct{} {
	if capacity < 1 {
		capacity = 1
	}
	ch := make(chan struct{}, capacity)
	n := held
	if n > capacity {
		n = capacity
	}
	if n < 0 {
		n = 0
	}
	for i := 0; i < n; i++ {
		ch <- struct{}{}
	}
	return ch
}

func (r *Registry) Get(platform string) *PlatformLimiter {
	platform = normalizePlatformName(platform)
	r.mu.Lock()
	defer r.mu.Unlock()
	if lim, ok := r.byName[platform]; ok {
		return lim
	}
	lim := newPlatformLimiter(platform, r.cfg)
	lim.taskReserve = r.taskReserve
	r.byName[platform] = lim
	return lim
}

func normalizePlatformName(platform string) string {
	if platform == "" {
		return "other"
	}
	return platform
}

// Update 原地热更新各平台 limiter。
func (r *Registry) Update(cfg config.RateLimitConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
	if len(r.byName) == 0 {
		r.byName["other"] = newPlatformLimiter("other", cfg)
		return
	}
	for _, lim := range r.byName {
		lim.ApplyConfig(cfg)
	}
}

// ApplyConfig 保留 active* 与 prefetchPaused；收窄上限时不杀旧任务，只挡住超额新启动。
func (p *PlatformLimiter) ApplyConfig(cfg config.RateLimitConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()

	maxTask := cfg.MaxConcurrent
	if maxTask <= 0 {
		maxTask = 20
	}
	maxConn := cfg.MaxConnections
	if maxConn <= 0 {
		maxConn = 80
	}
	boostSlots := boostSlotsFor(maxTask)
	reserved := connReservedFor(maxConn)

	bytesPerSec := cfg.BandwidthMbps * 1024 * 1024 / 8
	if p.bandwidth == nil {
		if bytesPerSec <= 0 {
			p.bandwidth = rate.NewLimiter(rate.Inf, 1<<20)
		} else {
			p.bandwidth = rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec))
		}
	} else if bytesPerSec <= 0 {
		p.bandwidth.SetLimit(rate.Inf)
		p.bandwidth.SetBurst(1 << 20)
	} else {
		p.bandwidth.SetLimit(rate.Limit(bytesPerSec))
		burst := int(bytesPerSec)
		if burst < 1 {
			burst = 1
		}
		p.bandwidth.SetBurst(burst)
	}

	cfg.MaxConcurrent = maxTask
	cfg.MaxConnections = maxConn
	p.cfg = cfg
	p.maxBoost = boostSlots
	p.connReserved = reserved
	p.connSem = resizeSem(p.activeConns, maxConn)
}

func (r *Registry) SetIdleRatio(ratio float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ratio > 0 {
		r.idleRatio = ratio
	}
}

// SetInteractiveTaskReserve 仅 OnInteractive=continue 时为 true：非交互不可吃满 MaxConcurrent。
func (r *Registry) SetInteractiveTaskReserve(enable bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taskReserve = enable
	for _, lim := range r.byName {
		lim.mu.Lock()
		lim.taskReserve = enable
		lim.mu.Unlock()
	}
}

func (r *Registry) IdleRatio() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.idleRatio
}

// interactiveReserveFor 从 MaxConcurrent 中保留给交互的槽位数（continue 模式下防止预取吃满）。
func interactiveReserveFor(maxTask int) int {
	r := maxTask / 10
	if r < 2 {
		r = 2
	}
	if r >= maxTask {
		r = maxTask / 2
		if r < 1 {
			r = 1
		}
	}
	return r
}

// AcquireTask 获取任务槽。boost：小文件插队池；interactive：交互下载（非预取）。
// taskReserve（continue 模式）时非交互最多占 MaxConcurrent-reserve；pause 模式靠 PrefetchPaused 让路。
func (p *PlatformLimiter) AcquireTask(ctx context.Context, boost, interactive bool) error {
	for {
		p.mu.Lock()
		maxTask := p.cfg.MaxConcurrent
		if maxTask <= 0 {
			maxTask = 20
		}
		maxBoost := p.maxBoost
		if maxBoost <= 0 {
			maxBoost = boostSlotsFor(maxTask)
			p.maxBoost = maxBoost
		}

		ok := false
		if boost {
			if p.activeBoost < maxBoost {
				p.activeBoost++
				p.activeTasks++
				ok = true
			}
		} else {
			normal := p.activeTasks - p.activeBoost
			if normal < 0 {
				normal = 0
			}
			limit := maxTask
			if !interactive && p.taskReserve {
				limit = maxTask - interactiveReserveFor(maxTask)
				if limit < 1 {
					limit = 1
				}
			}
			if normal < limit {
				p.activeTasks++
				ok = true
			}
		}
		p.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (p *PlatformLimiter) ReleaseTask(boost bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.activeTasks > 0 {
		p.activeTasks--
	}
	if boost && p.activeBoost > 0 {
		p.activeBoost--
	}
}

func (p *PlatformLimiter) AcquireConn(ctx context.Context, interactive bool) error {
	for {
		p.mu.Lock()
		maxConn := cap(p.connSem)
		limit := maxConn
		if !interactive {
			limit = maxConn - p.connReserved
			if limit < 1 {
				limit = 1
			}
		}
		under := p.activeConns < limit
		p.mu.Unlock()
		if !under {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
			continue
		}
		select {
		case p.connSem <- struct{}{}:
			p.mu.Lock()
			p.activeConns++
			p.mu.Unlock()
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
}

func (p *PlatformLimiter) ReleaseConn() {
	select {
	case <-p.connSem:
	default:
	}
	p.mu.Lock()
	if p.activeConns > 0 {
		p.activeConns--
	}
	p.mu.Unlock()
}

func (p *PlatformLimiter) WaitNClass(ctx context.Context, n int, class TrafficClass) error {
	if n <= 0 {
		return nil
	}
	// 交互下载始终全速，带宽整形只作用于预取
	if class == ClassInteractive {
		return nil
	}
	return p.WaitPrefetch(ctx, n, 0.3)
}

// WaitPrefetch 按空闲配额比例消耗预取带宽桶；生效 Mbps≤0 时不整形。
func (p *PlatformLimiter) WaitPrefetch(ctx context.Context, n int, idleRatio float64) error {
	if n <= 0 {
		return nil
	}
	mbps := p.cfg.EffectiveBandwidthMbps(time.Now())
	if mbps <= 0 {
		return nil
	}
	p.syncPrefetchLimit(mbps)
	scale := idleRatio
	if scale <= 0 {
		scale = 0.3
	}
	bps := mbps * 1024 * 1024 / 8
	extra := time.Duration(float64(n) / (bps * scale) * float64(time.Second))
	if extra > 0 {
		timer := time.NewTimer(extra / 4)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return p.waitLimiter(ctx, n, p.bandwidth, "prefetch")
}

func (p *PlatformLimiter) syncPrefetchLimit(mbps float64) {
	bps := mbps * 1024 * 1024 / 8
	if bps <= 0 {
		return
	}
	lim := rate.Limit(bps)
	burst := int(bps)
	if burst < 1 {
		burst = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bandwidth.Limit() == lim && p.bandwidth.Burst() == burst {
		return
	}
	p.bandwidth.SetLimit(lim)
	p.bandwidth.SetBurst(burst)
}

func (p *PlatformLimiter) waitLimiter(ctx context.Context, n int, lim *rate.Limiter, class string) error {
	start := time.Now()
	burst := lim.Burst()
	if burst <= 0 {
		burst = 1
	}
	left := n
	for left > 0 {
		chunk := left
		if chunk > burst {
			chunk = burst
		}
		if err := lim.WaitN(ctx, chunk); err != nil {
			return err
		}
		left -= chunk
	}
	sec := time.Since(start).Seconds()
	RateLimitWaitSeconds.WithLabelValues(p.name, class).Observe(sec)
	p.mu.Lock()
	p.waitSamples = append(p.waitSamples, sec)
	if len(p.waitSamples) > 256 {
		p.waitSamples = p.waitSamples[len(p.waitSamples)-256:]
	}
	p.mu.Unlock()
	return nil
}

// SetPrefetchPaused 设置暂停；返回是否发生状态变化。
func (p *PlatformLimiter) SetPrefetchPaused(paused bool) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefetchPaused == paused {
		return false
	}
	p.prefetchPaused = paused
	return true
}

func (p *PlatformLimiter) PrefetchPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prefetchPaused
}

func (p *PlatformLimiter) WaitStats() (p50, p95 float64, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n = len(p.waitSamples)
	if n == 0 {
		return 0, 0, 0
	}
	cp := append([]float64(nil), p.waitSamples...)
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	p50 = cp[(n-1)*50/100]
	p95 = cp[(n-1)*95/100]
	return p50, p95, n
}

func (p *PlatformLimiter) Snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p50, p95, n := 0.0, 0.0, 0
	if len(p.waitSamples) > 0 {
		cp := append([]float64(nil), p.waitSamples...)
		for i := 0; i < len(cp); i++ {
			for j := i + 1; j < len(cp); j++ {
				if cp[j] < cp[i] {
					cp[i], cp[j] = cp[j], cp[i]
				}
			}
		}
		n = len(cp)
		p50 = cp[(n-1)*50/100]
		p95 = cp[(n-1)*95/100]
	}
	eff := p.cfg.EffectiveBandwidthMbps(time.Now())
	return map[string]any{
		"platform":                 p.name,
		"bandwidth_mbps":           p.cfg.BandwidthMbps,
		"effective_bandwidth_mbps": eff,
		"max_concurrent":           p.cfg.MaxConcurrent,
		"max_connections":          p.cfg.MaxConnections,
		"windows":                  p.cfg.Windows,
		"bandwidth_limited":        eff > 0,
		"active_tasks":             p.activeTasks,
		"active_boost":             p.activeBoost,
		"boost_slots":              p.maxBoost,
		"active_conns":             p.activeConns,
		"conn_reserved":            p.connReserved,
		"prefetch_paused":          p.prefetchPaused,
		"rate_limit_wait_p50":      p50,
		"rate_limit_wait_p95":      p95,
		"rate_limit_wait_samples":  n,
	}
}

func (r *Registry) Snapshots() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, r.byName[name].Snapshot())
	}
	return out
}
