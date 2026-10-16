package prefetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	"github.com/livehl/mirrorhub/internal/platform"
	_ "github.com/livehl/mirrorhub/internal/platform/docker"
	_ "github.com/livehl/mirrorhub/internal/platform/goproxy"
	_ "github.com/livehl/mirrorhub/internal/platform/huggingface"
	_ "github.com/livehl/mirrorhub/internal/platform/maven"
	_ "github.com/livehl/mirrorhub/internal/platform/npm"
	_ "github.com/livehl/mirrorhub/internal/platform/pypi"
	"github.com/livehl/mirrorhub/internal/scheduler"
)

type Service struct {
	cfg   *config.Manager
	cache *cache.Manager
	dl    *downloader.Engine
	sched *scheduler.Scheduler
	log   *zap.Logger

	mu      sync.Mutex
	manual  []string
	cancel  map[string]context.CancelFunc
	root    context.Context
	delayed []delayedItem
}

func New(cfg *config.Manager, c *cache.Manager, dl *downloader.Engine, sched *scheduler.Scheduler, log *zap.Logger) *Service {
	return &Service{
		cfg:    cfg,
		cache:  c,
		dl:     dl,
		sched:  sched,
		log:    log,
		cancel: map[string]context.CancelFunc{},
		root:   context.Background(),
	}
}

func (s *Service) Start(ctx context.Context) {
	s.root = ctx
	go s.loop(ctx)
}

// Enqueue 接受包文件 URL，或各平台规格（如 PEP 508、image:tag、GAV）。
func (s *Service) Enqueue(items []string) {
	s.mu.Lock()
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		s.manual = append(s.manual, it)
	}
	s.mu.Unlock()
	go s.drainManual(s.root)
}

func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var manualTick int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.drainDelayed(ctx)
			manualTick++
			if manualTick%8 == 0 { // ~16s
				s.drainManual(ctx)
			}
		}
	}
}

func (s *Service) drainManual(ctx context.Context) {
	s.mu.Lock()
	items := append([]string{}, s.manual...)
	s.manual = nil
	s.mu.Unlock()
	for _, item := range items {
		urls, err := s.expandItem(ctx, item)
		if err != nil {
			s.failResolve(item, err)
			continue
		}
		if len(urls) == 0 {
			s.failResolve(item, fmt.Errorf("未找到匹配文件"))
			continue
		}
		s.log.Info("prefetch resolved", zap.String("item", item), zap.Int("files", len(urls)))
		for _, u := range urls {
			s.startOne(ctx, u)
		}
	}
}

func (s *Service) failResolve(item string, err error) {
	plat := platformNameForItem(item)
	taskID := s.sched.Begin(plat, item, scheduler.PriorityPrefetch, false, false)
	s.sched.MarkRunning(taskID)
	s.sched.End(taskID, err)
	s.log.Warn("prefetch resolve failed", zap.String("item", item), zap.Error(err))
}

func platformNameForItem(item string) string {
	if p := platform.LookupPrefetchOwner(item); p != nil {
		return p.Name()
	}
	return "unknown"
}

func platformForURL(cfg config.Config, rawURL string) (platform.Platform, config.PlatformConfig) {
	if p := platform.LookupPrefetchOwner(rawURL); p != nil {
		name := p.Name()
		pcfg, ok := cfg.Platforms[name]
		if !ok {
			return p, config.PlatformConfig{
				Download: config.DownloadConfig{Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024},
			}
		}
		return p, pcfg
	}
	name := "pypi"
	pcfg, ok := cfg.Platforms[name]
	if !ok {
		pcfg = config.PlatformConfig{
			Download: config.DownloadConfig{Concurrency: 16, ChunkSize: 5 * 1024 * 1024, MinSize: 100 * 1024},
		}
	}
	p := platform.ByName(name)
	return p, pcfg
}

func (s *Service) expandItem(ctx context.Context, item string) ([]string, error) {
	cfg := s.cfg.Get()
	env := platform.PrefetchExpandEnv{
		Ctx:     ctx,
		Cfg:     cfg,
		Backend: s,
		Log:     s.log,
	}
	for _, exp := range platform.PrefetchExpanders() {
		if !exp.OwnsPrefetchItem(item) {
			continue
		}
		// 最高优先级认领者已禁用：直接失败，禁止落到更低优先级平台继续展开
		if p, ok := exp.(platform.Platform); ok {
			name := p.Name()
			if pcfg, ok := cfg.Platforms[name]; ok && !pcfg.Enabled {
				return nil, fmt.Errorf("%s 模块未启用", name)
			}
		}
		return exp.ExpandPrefetchItem(env, item)
	}
	// 未被任何平台认领的绝对 URL：原样入队
	if strings.HasPrefix(item, "http://") || strings.HasPrefix(item, "https://") {
		return []string{item}, nil
	}
	return nil, fmt.Errorf("无法识别预取项: %s", item)
}

// PrefetchBackend
func (s *Service) ProxyBytes(ctx context.Context, method, rawURL string, headers http.Header, body io.Reader, plat string, prefetch bool) (int, http.Header, []byte, error) {
	return s.dl.ProxyBytes(ctx, method, rawURL, headers, body, plat, prefetch)
}

func (s *Service) PutBytes(key string, body []byte, contentType string, ttlSeconds int, meta cache.Meta) (*cache.Entry, error) {
	return s.cache.PutBytes(key, body, contentType, ttlSeconds, meta)
}

func (s *Service) FetchSimpleIndex(ctx context.Context, indexURL, plat string, ttlSeconds int, prefetch bool) ([]byte, string, error) {
	return s.dl.FetchSimpleIndex(ctx, indexURL, plat, ttlSeconds, prefetch)
}

func (s *Service) FetchMetadata(ctx context.Context, metaURL, plat string, ttlSeconds int, prefetch bool) ([]byte, error) {
	return s.dl.FetchMetadata(ctx, metaURL, plat, ttlSeconds, prefetch)
}

func (s *Service) RememberDigest(rawURL, sha256hex string) {
	downloader.RememberDigest(rawURL, sha256hex)
}

func (s *Service) Cancel(urlOrID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 含 P0 交互：调度器标 cancelled 并 abort 已 Bind 的请求 ctx
	taskURL, marked := s.sched.CancelTask(urlOrID)
	keys := []string{urlOrID}
	if taskURL != "" {
		keys = append(keys, taskURL)
	}
	if u, ok := s.sched.URLOf(urlOrID); ok && u != "" {
		keys = append(keys, u)
	}

	stopped := false
	seen := map[string]struct{}{}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if c, ok := s.cancel[k]; ok {
			c()
			delete(s.cancel, k)
			stopped = true
		}
	}
	return stopped || marked
}

// CancelMatching 取消本平台（空=全部）活跃预取 P1/P2，并打断下载 ctx、清空待展开 manual。
func (s *Service) CancelMatching(platform string) int {
	platform = normalizePrefetchPlatform(platform)
	n := s.sched.CancelMatching(scheduler.Match{Platform: platform, PrefetchOnly: true}, true)
	s.abortDownloadsMatching(platform)
	s.clearManualMatching(platform)
	s.clearDelayedMatching(platform)
	return n
}

// Purge 同一调度锁内取消预取（P1/P2）并清理已完成（成功/已取消，保留失败），再打断下载 ctx / manual（不伤 P0）。
func (s *Service) Purge(platform string) (cancelled, cleared int) {
	platform = normalizePrefetchPlatform(platform)
	cancelled, cleared = s.sched.PurgePrefetch(platform)
	s.abortDownloadsMatching(platform)
	s.clearManualMatching(platform)
	s.clearDelayedMatching(platform)
	return cancelled, cleared
}

// PurgeAll 取消匹配平台全部活跃任务（含 P0）并清理已完成（成功/已取消，保留失败），再打断预取下载 ctx / manual。
func (s *Service) PurgeAll(platform string) (cancelled, cleared int) {
	platform = normalizePrefetchPlatform(platform)
	cancelled, cleared = s.sched.PurgeAll(platform)
	s.abortDownloadsMatching(platform)
	s.clearManualMatching(platform)
	s.clearDelayedMatching(platform)
	return cancelled, cleared
}

func normalizePrefetchPlatform(platform string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	switch platform {
	case "hf":
		return "huggingface"
	case "go":
		return "goproxy"
	default:
		return platform
	}
}

func (s *Service) abortDownloadsMatching(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg.Get()
	for rawURL, cancel := range s.cancel {
		if platform != "" {
			plat, _ := platformForURL(cfg, rawURL)
			name := "pypi"
			if plat != nil {
				name = plat.Name()
			}
			if name != platform {
				continue
			}
		}
		cancel()
		delete(s.cancel, rawURL)
	}
}

func (s *Service) clearManualMatching(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if platform == "" {
		s.manual = nil
		return
	}
	kept := s.manual[:0]
	for _, it := range s.manual {
		if platformNameForItem(it) == platform {
			continue
		}
		kept = append(kept, it)
	}
	s.manual = kept
}
