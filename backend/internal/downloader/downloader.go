package downloader

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/ratelimit"
	"github.com/nihaoyanzu/cistern/internal/traffic"
)

type Options struct {
	URL            string // 实际上游 GET/Range 地址（可为重定向后 URL）
	SourceURL      string // 写入缓存的稳定源 URL；空则用 URL
	CacheKey       string // 必须基于重定向前的路径 URL，勿随 302 变化
	ContentType    string
	Concurrency    int
	ChunkSize      int64
	MinSize        int64 // 并行流式阈值；0 则用默认 100KiB
	TTLSeconds     int
	Platform       string
	Prefetch       bool
	Boost          bool   // 小文件插队槽
	TaskID         string // 调度器任务 id；预取门闩（P0 pause / P1>P2）用
	Headers        http.Header
	ChunkTTLHours  int
	Kind           string // package | metadata，默认 package
	ExpectedSHA256 string // 期望内容摘要（无则跳过校验）
	// KnownSize：>=0 使用该大小并跳过 HEAD；-1 表示未知长度走串行；未设置时用 HasKnownSize=false
	HasKnownSize bool
	KnownSize    int64
	RangeHeader  string // 客户端 Range，仅 ServePackage HIT 使用
	// OnAcquired 在拿到任务槽并开始真实下载时回调（MarkRunning / 计 P0）
	OnAcquired func()
	// OnHit 缓存 HIT 开始服务时回调（MarkHit，不计 P0）
	OnHit func()
	// OnWaiting inflight 合并等待时回调（不计 P0）
	OnWaiting func()
	// OnProgress 下载进度（已完成字节, 总字节；总字节未知时为 0）
	OnProgress func(done, total int64)
	// OnAuthFail 上游 401/403 时换票并刷新 headers；challenge 为上游响应头（可含 WWW-Authenticate）。
	// 成功则返回 nil。每文件最多由引擎调用一次。
	OnAuthFail func(ctx context.Context, headers http.Header, challenge http.Header) error
	// IdentityETag 首次探测到的上游 ETag（可选）；续传校验用
	IdentityETag string
}

type Result struct {
	Entry       *cache.Entry
	ContentType string
	Size        int64
	FromCache   bool
	Strategy    string // parallel | serial | cache
}

type Engine struct {
	cache        *cache.Manager
	limiters     *ratelimit.Registry
	client       *http.Client
	headClient   *http.Client // HEAD 专用：不复用连接，避免连接池卡死
	log          *zap.Logger
	idleRatio    func() float64
	traffic      *traffic.Recorder
	inflight     sync.Map                 // cacheKey -> *inflightWait
	prefetchGate func(taskID string) bool // 可选：P1>P2 等调度门闩
	gov          *HostGovernor
}

type inflightMode int

const (
	inflightModeFetch inflightMode = iota
	inflightModeStream
)

type inflightWait struct {
	done   chan struct{}
	mode   inflightMode
	cancel context.CancelFunc // 预取可被交互接管时取消
	err    error
	entry  *cache.Entry
	ct     string
	size   int64
}

func New(c *cache.Manager, lim *ratelimit.Registry, upstreamProxy string, log *zap.Logger, idleRatio func() float64, tr *traffic.Recorder) *Engine {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 包文件本身可能是 gzip（.tar.gz），禁止 Transport 按 Content-Encoding 自动解压
	transport.DisableCompression = true
	transport.TLSHandshakeTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	if upstreamProxy != "" {
		u, err := url.Parse(upstreamProxy)
		if err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	// HEAD 专用 transport：不复用连接，避免连接池中的卡死连接阻塞 HEAD 请求
	headTransport := http.DefaultTransport.(*http.Transport).Clone()
	headTransport.DisableKeepAlives = true
	headTransport.DisableCompression = true
	headTransport.TLSHandshakeTimeout = 10 * time.Second
	headTransport.ResponseHeaderTimeout = 10 * time.Second
	headTransport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 10 * time.Second,
	}).DialContext
	if upstreamProxy != "" {
		u, err := url.Parse(upstreamProxy)
		if err == nil {
			headTransport.Proxy = http.ProxyURL(u)
		}
	}
	return &Engine{
		cache:    c,
		limiters: lim,
		client: &http.Client{
			// hubCaptureRT：跟随 CDN 重定向前保留 X-Repo-Commit 等 Hub 头
			Transport: wrapHubCapture(transport),
			Timeout:   0,
		},
		headClient: &http.Client{
			Transport: wrapHubCapture(headTransport),
			Timeout:   10 * time.Second,
		},
		log:       log,
		idleRatio: idleRatio,
		traffic:   tr,
		gov:       newHostGovernor(),
	}
}

// HostGov 供预取投放软限等外部查询。
func (e *Engine) HostGov() *HostGovernor {
	if e == nil {
		return nil
	}
	return e.gov
}

// SetPrefetchGate 注入预取调度门闩（如 scheduler.PrefetchMayRun）：有活跃 P1 时挡住普通 P2。
func (e *Engine) SetPrefetchGate(fn func(taskID string) bool) {
	e.prefetchGate = fn
}

// SetUpstreamProxy 热更新出站代理；空字符串表示直连（不走环境变量代理）。
func (e *Engine) SetUpstreamProxy(proxyURL string) {
	apply := func(tr *http.Transport) {
		if tr == nil {
			return
		}
		proxyURL = strings.TrimSpace(proxyURL)
		if proxyURL == "" {
			// Proxy=nil 会回退到 ProxyFromEnvironment；显式直连需返回 (nil, nil)
			tr.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
			tr.CloseIdleConnections()
			return
		}
		u, err := url.Parse(proxyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return
		}
		tr.Proxy = http.ProxyURL(u)
		// 换代理后必须丢掉旧 keep-alive，否则会继续直连上游
		tr.CloseIdleConnections()
	}
	if t := unwrapHTTPTransport(e.client.Transport); t != nil {
		apply(t)
	}
	if t := unwrapHTTPTransport(e.headClient.Transport); t != nil {
		apply(t)
	}
}

func (opt Options) metaSource() string {
	if opt.SourceURL != "" {
		return opt.SourceURL
	}
	return opt.URL
}

func (opt Options) metaKind() string {
	if opt.Kind != "" {
		return opt.Kind
	}
	return "package"
}

func notifyAcquired(opt Options) {
	if opt.OnAcquired != nil {
		opt.OnAcquired()
	}
}

func notifyHit(opt Options) {
	if opt.OnHit != nil {
		opt.OnHit()
		return
	}
	// 无 OnHit 时不回落到 OnAcquired，避免 HIT 误 pause
}

func notifyProgress(opt Options, done, total int64) {
	if opt.OnProgress != nil {
		opt.OnProgress(done, total)
	}
}

// GetOrDownload 预取/后台下载：命中缓存或分片/串行拉取。与 ServePackage 共享 inflight 去重。
func (e *Engine) GetOrDownload(ctx context.Context, opt Options) (*Result, error) {
	return e.getOrDownload(ctx, opt, true)
}

func (e *Engine) getOrDownload(ctx context.Context, opt Options, allowFollowerRetry bool) (*Result, error) {
	opt.resolveExpectedDigest()
	if entry, ok := e.cache.Get(opt.CacheKey, opt.Platform); ok {
		// 预取命中也必须占任务槽，避免海量缓存命中同时 MarkRunning 冲破并发上限
		var pl *ratelimit.PlatformLimiter
		if opt.Prefetch {
			pl = e.limiters.Get(opt.Platform)
			if err := e.acquireTaskSlot(ctx, pl, opt); err != nil {
				return nil, err
			}
			defer pl.ReleaseTask(opt.Boost)
		}
		notifyProgress(opt, entry.Size, entry.Size)
		notifyAcquired(opt)
		return &Result{Entry: entry, ContentType: entry.ContentType, Size: entry.Size, FromCache: true, Strategy: "cache"}, nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	wait := &inflightWait{done: make(chan struct{}), mode: inflightModeFetch, cancel: cancel}
	actual, loaded := e.inflight.LoadOrStore(opt.CacheKey, wait)
	sw := actual.(*inflightWait)
	if loaded {
		cancel()
		if opt.OnWaiting != nil {
			opt.OnWaiting()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-sw.done:
		}
		if sw.err != nil {
			return nil, sw.err
		}
		if sw.entry != nil {
			return &Result{Entry: sw.entry, ContentType: sw.ct, Size: sw.size, FromCache: true, Strategy: "cache"}, nil
		}
		if entry, ok := e.cache.Get(opt.CacheKey, opt.Platform); ok {
			return &Result{Entry: entry, ContentType: entry.ContentType, Size: entry.Size, FromCache: true, Strategy: "cache"}, nil
		}
		// leader 被交互取消等导致无 entry：跟随者再占坑一次
		if allowFollowerRetry {
			return e.getOrDownload(ctx, opt, false)
		}
		return nil, fmt.Errorf("download finished but cache miss")
	}
	defer func() {
		e.inflight.Delete(opt.CacheKey)
		close(wait.done)
		cancel()
	}()

	res, err := e.download(runCtx, opt)
	wait.err = err
	if res != nil && res.Entry != nil {
		wait.entry = res.Entry
		wait.ct = res.ContentType
		wait.size = res.Size
	}
	return res, err
}
