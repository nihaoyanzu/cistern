package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/platform"
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/traffic"
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
	inflight     sync.Map // cacheKey -> *inflightWait
	prefetchGate func(taskID string) bool // 可选：P1>P2 等调度门闩
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
			Transport: transport,
			Timeout:   0,
		},
		headClient: &http.Client{
			Transport: headTransport,
			Timeout:   10 * time.Second,
		},
		log:       log,
		idleRatio: idleRatio,
		traffic:   tr,
	}
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
	if t, ok := e.client.Transport.(*http.Transport); ok {
		apply(t)
	}
	if t, ok := e.headClient.Transport.(*http.Transport); ok {
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

func (e *Engine) download(ctx context.Context, opt Options) (*Result, error) {
	opt.resolveExpectedDigest()
	if opt.Prefetch {
		if err := e.cache.AllowPrefetchWrite(); err != nil {
			return nil, err
		}
	}
	pl := e.limiters.Get(opt.Platform)
	if pl == nil {
		return nil, fmt.Errorf("no rate limiter for platform %s", opt.Platform)
	}
	if err := e.acquireTaskSlot(ctx, pl, opt); err != nil {
		return nil, err
	}
	lease := newPrefetchLease(e, pl, opt)
	defer lease.close()
	notifyAcquired(opt)

	size := int64(-1)
	ct := opt.ContentType
		if opt.HasKnownSize {
		size = opt.KnownSize
	} else {
		hs, hct, finalURL, err := e.head(ctx, opt)
		if err != nil {
			return e.downloadSerial(ctx, pl, opt, lease, ct, "serial")
		}
		size = hs
		if hct != "" {
			ct = hct
		}
		if finalURL != "" {
			opt.URL = finalURL
		}
	}
	if size < 0 {
		return e.downloadSerial(ctx, pl, opt, lease, ct, "serial")
	}

	tmp := e.cache.TempPath(opt.CacheKey)
	resume := false
	if st, err := os.Stat(tmp); err == nil && st.Size() == size {
		resume = true
	} else {
		_ = os.Remove(tmp)
	}

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if !resume {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, err
		}
	}

	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	chunkSize := opt.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 5 * 1024 * 1024
	}

	type chunk struct{ start, end int64 }
	var chunks []chunk
	for start := int64(0); start < size; start += chunkSize {
		end := start + chunkSize - 1
		if end >= size {
			end = size - 1
		}
		chunks = append(chunks, chunk{start, end})
	}

	doneSet := map[[2]int64]bool{}
	var already int64
	if resume {
		for _, r := range e.cache.GetChunks(opt.URL, size, opt.ChunkTTLHours) {
			doneSet[r] = true
			already += r[1] - r[0] + 1
		}
	}
	var downloaded atomic.Int64
	downloaded.Store(already)
	notifyProgress(opt, already, size)
	onBytes := func(n int64) {
		if n <= 0 {
			return
		}
		notifyProgress(opt, downloaded.Add(n), size)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	var mu sync.Mutex
	var completed [][2]int64

	for _, ch := range chunks {
		ch := ch
		if doneSet[[2]int64{ch.start, ch.end}] {
			continue
		}
		g.Go(func() error {
			if _, err := e.waitPrefetchGate(gctx, pl, opt, lease, nil); err != nil {
				return err
			}
			if err := pl.AcquireConn(gctx, !opt.Prefetch); err != nil {
				return err
			}
			connHeld := true
			releaseConn := func() {
				if connHeld {
					pl.ReleaseConn()
					connHeld = false
				}
			}
			defer releaseConn()
			ensureConn := func() error {
				if connHeld {
					return nil
				}
				if err := pl.AcquireConn(gctx, !opt.Prefetch); err != nil {
					return err
				}
				connHeld = true
				return nil
			}

			// 分片内断点续传：pause/瞬时错误后从已写入偏移继续，不回退总进度
			cur := ch.start
			var lastErr error
			for attempt := 0; attempt < 5; attempt++ {
				if cur > ch.end {
					break
				}
				if attempt > 0 {
					if _, err := e.waitPrefetchGate(gctx, pl, opt, lease, releaseConn); err != nil {
						return err
					}
					if err := ensureConn(); err != nil {
						return err
					}
				}
				var got int64
				err := e.fetchChunk(gctx, pl, opt, lease, f, cur, ch.end, releaseConn, func(n int64) {
					got += n
					onBytes(n)
				})
				if err != nil {
					cur += got
					lastErr = err
					if errors.Is(err, errPrefetchReconnect) || isTransientNetErr(err) {
						time.Sleep(time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond)
						continue
					}
					return err
				}
				mu.Lock()
				completed = append(completed, [2]int64{ch.start, ch.end})
				mu.Unlock()
				return nil
			}
			return lastErr
		})
	}
	if err := g.Wait(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(completed) > 0 {
		_ = e.cache.MarkChunks(opt.URL, size, completed)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	notifyProgress(opt, size, size)
	opt.resolveExpectedDigest()
	if err := verifyAndRemember(tmp, opt); err != nil {
		return nil, err
	}

	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL: opt.metaSource(),
		Kind:      opt.metaKind(),
		Digest:    opt.ExpectedSHA256,
	})
	if err != nil {
		return nil, err
	}
	_ = e.cache.ClearChunks(opt.URL)
	_ = os.Remove(tmp)
	return &Result{Entry: entry, ContentType: ct, Size: size, FromCache: false, Strategy: "parallel"}, nil
}

// downloadSerial 单连接整包下载（上游未提供 Content-Length 时使用）
func (e *Engine) downloadSerial(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, ct, strategy string) (*Result, error) {
	tmp := e.cache.TempPath(opt.CacheKey + ".serial")
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if _, err := e.waitPrefetchGate(ctx, pl, opt, lease, nil); err != nil {
				_ = os.Remove(tmp)
				return nil, err
			}
		}
		res, err := e.downloadSerialOnce(ctx, pl, opt, lease, ct, strategy, tmp)
		if err == nil {
			return res, nil
		}
		last = err
		if !errors.Is(err, errPrefetchReconnect) && !errors.Is(err, errUpstreamStall) && !isTransientNetErr(err) {
			_ = os.Remove(tmp)
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(tmp)
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond):
		}
	}
	_ = os.Remove(tmp)
	return nil, last
}

func (e *Engine) downloadSerialOnce(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, ct, strategy, tmp string) (*Result, error) {
	if err := pl.AcquireConn(ctx, !opt.Prefetch); err != nil {
		return nil, err
	}
	connHeld := true
	releaseConn := func() {
		if connHeld {
			pl.ReleaseConn()
			connHeld = false
		}
	}
	defer releaseConn()

	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
		if have < 0 {
			have = 0
		}
	}

	headers := cloneHeader(opt.Headers)
	if have > 0 {
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := e.doGET(ctx, http.MethodGet, opt.URL, headers, 3)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 续传：要 206；若上游忽略 Range 回 200，则从头重写
	restart := false
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		// ok
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && have == 0:
		// ok full body
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && have > 0:
		restart = true
		have = 0
	default:
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("upstream %s", resp.Status)
		}
		return nil, fmt.Errorf("upstream %s", resp.Status)
	}

	if ct == "" {
		ct = resp.Header.Get("Content-Type")
	}
	if (opt.Kind == "package" || opt.Kind == "") && isHTMLContentType(ct) {
		return nil, fmt.Errorf("upstream returned HTML, not a package file")
	}
	if opt.Platform == "docker" && looksLikeDockerNonBlobJSON(ct) {
		return nil, fmt.Errorf("upstream returned JSON/manifest for docker blob (not a layer)")
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	if restart {
		_ = os.Remove(tmp)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if have > 0 {
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			return nil, err
		}
	} else if err := f.Truncate(0); err != nil {
		return nil, err
	}

	idle := 0.3
	if e.idleRatio != nil {
		idle = e.idleRatio()
	}

	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	watchDone := make(chan struct{})
	go func(body io.ReadCloser) {
		defer close(watchDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-t.C:
				last := time.Unix(0, lastProgress.Load())
				if time.Since(last) >= streamStallTimeout {
					_ = body.Close()
					reqCancel()
					return
				}
			}
		}
	}(resp.Body)

	body := &stallAwareBody{ctx: reqCtx, body: resp.Body}
	lr := &limitReader{
		ctx:       ctx,
		r:         body,
		pl:        pl,
		prefetch:  opt.Prefetch,
		idle:      idle,
		traffic:   e.traffic,
		allow:     e.prefetchAllow(opt),
		lease:     lease,
		onBlocked: releaseConn,
	}

	total := resp.ContentLength
	if have > 0 && resp.StatusCode == http.StatusPartialContent {
		if total > 0 {
			total = have + total
		}
	}
	if cr := resp.Header.Get("Content-Range"); have > 0 && total <= 0 {
		if n := parseContentRangeTotal(cr); n > 0 {
			total = n
		}
	}
	wantSize := int64(-1)
	if opt.HasKnownSize && opt.KnownSize > 0 {
		wantSize = opt.KnownSize
	}
	if total > 0 {
		if wantSize > 0 && total != wantSize {
			return nil, fmt.Errorf("upstream size mismatch: known=%d get=%d", wantSize, total)
		}
		wantSize = total
	}
	if total > 0 {
		notifyProgress(opt, have, total)
	} else if have > 0 {
		notifyProgress(opt, have, 0)
	}

	done := have
	var doneMu sync.Mutex
	cw := &progressWriter{
		w: f,
		onWrite: func(n int64) {
			lastProgress.Store(time.Now().UnixNano())
			doneMu.Lock()
			done += n
			cur, tot := done, total
			doneMu.Unlock()
			if tot > 0 {
				notifyProgress(opt, cur, tot)
			} else {
				notifyProgress(opt, cur, 0)
			}
		},
	}
	n, err := io.Copy(cw, lr)
	reqCancel()
	<-watchDone
	if err != nil {
		cleanup = false
		return nil, err
	}
	if err := f.Close(); err != nil {
		cleanup = false
		return nil, err
	}
	final := have + n
	if wantSize > 0 && final != wantSize {
		cleanup = false
		return nil, fmt.Errorf("incomplete download: got %d want %d", final, wantSize)
	}
	notifyProgress(opt, final, final)
	opt.resolveExpectedDigest()
	if err := verifyAndRemember(tmp, opt); err != nil {
		return nil, err
	}
	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL: opt.metaSource(),
		Kind:      opt.metaKind(),
		Digest:    opt.ExpectedSHA256,
	})
	if err != nil {
		return nil, err
	}
	cleanup = false
	_ = os.Remove(tmp)
	return &Result{Entry: entry, ContentType: ct, Size: final, FromCache: false, Strategy: strategy}, nil
}

func (e *Engine) prefetchAllow(opt Options) func() bool {
	if !opt.Prefetch || e.prefetchGate == nil || opt.TaskID == "" {
		return nil
	}
	taskID := opt.TaskID
	gate := e.prefetchGate
	return func() bool { return gate(taskID) }
}

// errPrefetchReconnect：pause 期间停读后连接可能已僵死，调用方应换新请求重试。
var errPrefetchReconnect = errors.New("prefetch reconnect after pause")

// prefetchLease 预取任务槽租约：pause/让路等待时释放槽，恢复后再占回，避免饿死 P0。
type prefetchLease struct {
	e           *Engine
	pl          *ratelimit.PlatformLimiter
	opt         Options
	mu          sync.Mutex
	held        bool
	acquiring   bool
	acquireWait chan struct{}
}

func newPrefetchLease(e *Engine, pl *ratelimit.PlatformLimiter, opt Options) *prefetchLease {
	if !opt.Prefetch || pl == nil {
		return nil
	}
	return &prefetchLease{e: e, pl: pl, opt: opt, held: true}
}

func (l *prefetchLease) release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		l.pl.ReleaseTask(l.opt.Boost)
		l.held = false
	}
}

func (l *prefetchLease) close() {
	l.release()
}

func (l *prefetchLease) ensureHeld(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		if l.held {
			l.mu.Unlock()
			return nil
		}
		if l.acquiring {
			ch := l.acquireWait
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ch:
				continue
			}
		}
		l.acquiring = true
		l.acquireWait = make(chan struct{})
		l.mu.Unlock()

		err := l.e.acquireTaskSlot(ctx, l.pl, l.opt)

		l.mu.Lock()
		l.acquiring = false
		if err == nil {
			l.held = true
		}
		close(l.acquireWait)
		l.mu.Unlock()
		return err
	}
}

// waitPrefetchGate：P0 交互 pause 时挡住预取；等待期间通过 lease 释放任务槽。
// onBlocked 在首次进入等待时调用（用于释放连接槽，避免预取攥 conn 饿死交互）。
// held=true 表示曾被门闩挡住，上游连接不宜继续读。
func (e *Engine) waitPrefetchGate(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, onBlocked func()) (held bool, err error) {
	if !opt.Prefetch || pl == nil {
		return false, nil
	}
	allow := e.prefetchAllow(opt)
	blockedOnce := false
	for {
		if !pl.PrefetchPaused() && (allow == nil || allow()) {
			if held {
				if err := lease.ensureHeld(ctx); err != nil {
					return true, err
				}
			}
			return held, nil
		}
		held = true
		if !blockedOnce {
			blockedOnce = true
			if onBlocked != nil {
				onBlocked()
			}
		}
		lease.release()
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// acquireTaskSlot 获取任务槽；预取在 pause / 让路期间不占槽，避免暂停间隙继续开跑、冲破并发体感。
func (e *Engine) acquireTaskSlot(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options) error {
	if pl == nil {
		return fmt.Errorf("no rate limiter")
	}
	if !opt.Prefetch {
		return pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch)
	}
	allow := e.prefetchAllow(opt)
	for {
		for {
			if !pl.PrefetchPaused() && (allow == nil || allow()) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if err := pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch); err != nil {
			return err
		}
		if !pl.PrefetchPaused() && (allow == nil || allow()) {
			return nil
		}
		pl.ReleaseTask(opt.Boost)
	}
}

// Head 探测上游文件大小（用于 boost / 分片 / Docker descriptor 校验）。
// 成功时保证 size>0；拿不到有效大小则返回 error（绝不把 -1/0 当成功）。
func (e *Engine) Head(ctx context.Context, rawURL string, headers http.Header) (int64, string, string, error) {
	return e.head(ctx, Options{URL: rawURL, Headers: headers})
}

func (e *Engine) head(ctx context.Context, opt Options) (int64, string, string, error) {
	// 使用独立的 headClient，不复用连接池，避免卡死连接阻塞
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, opt.URL, nil)
	if err != nil {
		return 0, "", "", err
	}
	copyHeaders(req, opt.Headers)
	req.Header.Del("Accept-Encoding")
	resp, err := e.headClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	finalURL := resp.Request.URL.String()
	ct := resp.Header.Get("Content-Type")
	status := resp.StatusCode
	cl := resp.ContentLength
	_ = resp.Body.Close()
	if status >= 400 {
		return 0, "", "", fmt.Errorf("HEAD %s: HTTP %d", opt.URL, status)
	}
	if cl > 0 {
		return cl, ct, finalURL, nil
	}
	// 无 Content-Length：用 Range bytes=0-0 从 Content-Range 解析 total（containerd 依赖真实 size）
	size, rangeCT, rangeURL, rangeErr := e.headSizeViaRange(ctx, opt)
	if rangeErr != nil {
		if cl == 0 {
			return 0, "", "", fmt.Errorf("HEAD %s: content size of zero", opt.URL)
		}
		return 0, "", "", fmt.Errorf("HEAD %s: missing content length: %w", opt.URL, rangeErr)
	}
	if rangeCT != "" {
		ct = rangeCT
	}
	if rangeURL != "" {
		finalURL = rangeURL
	}
	return size, ct, finalURL, nil
}

// headSizeViaRange 发 Range: bytes=0-0，从 Content-Range 解析总长度。
func (e *Engine) headSizeViaRange(ctx context.Context, opt Options) (int64, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opt.URL, nil)
	if err != nil {
		return 0, "", "", err
	}
	copyHeaders(req, opt.Headers)
	req.Header.Del("Accept-Encoding")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := e.headClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 {
			return 0, "", "", fmt.Errorf("Range probe %s: %s", opt.URL, resp.Status)
		}
		return 0, "", "", fmt.Errorf("Range probe unexpected status %s", resp.Status)
	}
	size := parseContentRangeTotal(resp.Header.Get("Content-Range"))
	if size <= 0 && resp.ContentLength > 0 && resp.StatusCode == http.StatusOK {
		// 上游忽略 Range 回了整包：CL 即总长
		size = resp.ContentLength
	}
	if size <= 0 {
		return 0, "", "", fmt.Errorf("Range probe: cannot resolve size")
	}
	return size, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
}

// parseContentRangeTotal 解析 Content-Range: bytes start-end/total；* 或无效返回 -1。
func parseContentRangeTotal(cr string) int64 {
	cr = strings.TrimSpace(cr)
	if cr == "" {
		return -1
	}
	i := strings.LastIndexByte(cr, '/')
	if i < 0 || i+1 >= len(cr) {
		return -1
	}
	total := strings.TrimSpace(cr[i+1:])
	if total == "*" || total == "" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

func (e *Engine) fetchChunk(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, f *os.File, start, end int64, onBlocked func(), onBytes func(int64)) error {
	want := end - start + 1
	if want <= 0 {
		return fmt.Errorf("invalid chunk range %d-%d", start, end)
	}
	headers := cloneHeader(opt.Headers)
	headers.Del("Accept-Encoding")
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := e.doGET(ctx, http.MethodGet, opt.URL, headers, 3)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && start != 0 {
		return fmt.Errorf("chunk ignored Range (got 200 for start=%d)", start)
	}
	if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == http.StatusOK && start == 0) {
		return fmt.Errorf("chunk status %d", resp.StatusCode)
	}

	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	watchDone := make(chan struct{})
	go func(body io.ReadCloser) {
		defer close(watchDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-reqCtx.Done():
				return
			case <-t.C:
				last := time.Unix(0, lastProgress.Load())
				if time.Since(last) >= streamStallTimeout {
					_ = body.Close()
					reqCancel()
					return
				}
			}
		}
	}(resp.Body)

	body := &stallAwareBody{ctx: reqCtx, body: resp.Body}
	buf := make([]byte, 32*1024)
	offset := start
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			lastProgress.Store(time.Now().UnixNano())
			idle := 0.3
			if e.idleRatio != nil {
				idle = e.idleRatio()
			}
			if opt.Prefetch {
				held, gateErr := e.waitPrefetchGate(ctx, pl, opt, lease, onBlocked)
				if gateErr != nil {
					return gateErr
				}
				if held {
					return errPrefetchReconnect
				}
				if err := pl.WaitPrefetch(ctx, n, idle); err != nil {
					return err
				}
			} else if err := pl.WaitNClass(ctx, n, ratelimit.ClassInteractive); err != nil {
				return err
			}
			if e.traffic != nil {
				e.traffic.AddDownload(int64(n))
			}
			if _, err := f.WriteAt(buf[:n], offset); err != nil {
				return err
			}
			offset += int64(n)
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, errUpstreamStall) {
				return errUpstreamStall
			}
			return readErr
		}
	}
	got := offset - start
	if got != want {
		return fmt.Errorf("chunk short read: got %d want %d (range %d-%d)", got, want, start, end)
	}
	return nil
}

type progressWriter struct {
	w       io.Writer
	onWrite func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 && p.onWrite != nil {
		p.onWrite(int64(n))
	}
	return n, err
}

func copyHeaders(req *http.Request, h http.Header) {
	if h == nil {
		return
	}
	for k, vals := range h {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
}

func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "tls handshake timeout"),
		strings.Contains(s, "timeout awaiting response headers"),
		strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "connection reset"),
		strings.Contains(s, "connection refused"),
		strings.Contains(s, "unexpected eof"),
		strings.Contains(s, "broken pipe"),
		strings.Contains(s, "server closed idle connection"),
		strings.Contains(s, "temporary"):
		return true
	default:
		return false
	}
}

// doGET 对无 body 的上游请求做瞬时错误重试（TLS 握手超时等）
func (e *Engine) doGET(ctx context.Context, method, rawURL string, headers http.Header, attempts int) (*http.Response, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			backoff := time.Duration(i) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			if e.log != nil {
				e.log.Warn("upstream retry",
					zap.String("method", method),
					zap.String("url", rawURL),
					zap.Int("attempt", i+1),
					zap.Error(last),
				)
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		copyHeaders(req, headers)
		req.Header.Del("Accept-Encoding")
		resp, err := e.client.Do(req)
		if err == nil {
			return resp, nil
		}
		last = err
		if !isTransientNetErr(err) {
			return nil, err
		}
	}
	return nil, last
}

// ProxyHEAD 轻量级 HEAD 请求，仅返回状态码；用于条件请求续期（If-None-Match）
func (e *Engine) ProxyHEAD(ctx context.Context, rawURL string, headers http.Header) (int, error) {
	resp, err := e.doGET(ctx, http.MethodHead, rawURL, headers, 2)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// ProxyBytes 整包拉取（索引等）；platform 非空时按字节限速
func (e *Engine) ProxyBytes(ctx context.Context, method, rawURL string, headers http.Header, body io.Reader, platform string, prefetch bool) (int, http.Header, []byte, error) {
	// 有请求体时不重试，避免 body 被消费后无法重放
	if body != nil && body != http.NoBody {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
		if err != nil {
			return 0, nil, nil, err
		}
		copyHeaders(req, headers)
		req.Header.Del("Accept-Encoding")
		resp, err := e.client.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer resp.Body.Close()
		return e.readProxyBody(ctx, resp, platform, prefetch)
	}

	var lastStatus int
	var lastHeader http.Header
	var lastBody []byte
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := e.doGET(ctx, method, rawURL, headers, 3)
		if err != nil {
			return 0, nil, nil, err
		}
		status, hdr, data, readErr := e.readProxyBody(ctx, resp, platform, prefetch)
		_ = resp.Body.Close()
		if readErr == nil {
			return status, hdr, data, nil
		}
		lastStatus, lastHeader, lastBody, lastErr = status, hdr, data, readErr
		if !errors.Is(readErr, errPrefetchReconnect) {
			return status, hdr, data, readErr
		}
		select {
		case <-ctx.Done():
			return 0, nil, nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	return lastStatus, lastHeader, lastBody, lastErr
}

func (e *Engine) readProxyBody(ctx context.Context, resp *http.Response, platform string, prefetch bool) (int, http.Header, []byte, error) {
	var r io.Reader = resp.Body
	limited := false
	if platform != "" {
		if pl := e.limiters.Get(platform); pl != nil {
			idle := 0.3
			if e.idleRatio != nil {
				idle = e.idleRatio()
			}
			r = &limitReader{ctx: ctx, r: resp.Body, pl: pl, prefetch: prefetch, idle: idle, traffic: e.traffic}
			limited = true
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, nil, nil, err
	}
	if e.traffic != nil && !limited {
		e.traffic.AddDownload(int64(len(data)))
	}
	return resp.StatusCode, resp.Header.Clone(), data, nil
}

// FetchSimpleIndex 读取/缓存改写前的 simple 索引正文（供预取解析与代理共用）。
// ttlSeconds<=0 时不读写通用包缓存（用于巨型根包名索引等场景）。
func (e *Engine) FetchSimpleIndex(ctx context.Context, indexURL, platName string, ttlSeconds int, prefetch bool) ([]byte, string, error) {
	cacheKey := "pypi:index:" + cache.KeyFromURL(indexURL)
	if ttlSeconds > 0 {
		if entry, ok := e.cache.Get(cacheKey, platName); ok {
			data, err := os.ReadFile(entry.FilePath)
			if err != nil {
				return nil, "", err
			}
			return data, entry.ContentType, nil
		}
	}
	status, respHeader, body, err := e.ProxyBytes(ctx, http.MethodGet, indexURL, nil, nil, platName, prefetch)
	if err != nil || status >= 400 {
		if ttlSeconds > 0 {
			if stale, ok := e.cache.GetStale(cacheKey); ok {
				data, readErr := os.ReadFile(stale.FilePath)
				if readErr == nil {
					return data, stale.ContentType, nil
				}
			}
		}
		if err != nil {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("upstream status %d", status)
	}
	body, _ = platform.MaybeGunzip(body)
	ct := platform.DetectContentType(respHeader.Get("Content-Type"), body)
	if ttlSeconds > 0 {
		_, _ = e.cache.PutBytes(cacheKey, body, ct, ttlSeconds, cache.Meta{
			SourceURL:    indexURL,
			Kind:         "index",
			UpstreamETag: respHeader.Get("ETag"),
		})
	}
	return body, ct, nil
}

// FetchMetadata 拉取 PEP 658 metadata；优先新鲜缓存，上游失败时回退过期缓存。
func (e *Engine) FetchMetadata(ctx context.Context, metaURL, platName string, ttlSeconds int, prefetch bool) ([]byte, error) {
	cacheKey := cache.KeyFromURL(metaURL)
	if ttlSeconds > 0 {
		if entry, ok := e.cache.Get(cacheKey, platName); ok {
			return os.ReadFile(entry.FilePath)
		}
	}
	status, respHeader, body, err := e.ProxyBytes(ctx, http.MethodGet, metaURL, nil, nil, platName, prefetch)
	if err != nil || status < 200 || status >= 300 {
		if ttlSeconds > 0 {
			if stale, ok := e.cache.GetStale(cacheKey); ok {
				data, readErr := os.ReadFile(stale.FilePath)
				if readErr == nil {
					return data, nil
				}
			}
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("metadata status %d url=%s", status, metaURL)
	}
	ct := platform.DetectContentType(respHeader.Get("Content-Type"), body)
	if ct == "" {
		ct = "text/plain"
	}
	if ttlSeconds > 0 {
		_, _ = e.cache.PutBytes(cacheKey, body, ct, ttlSeconds, cache.Meta{
			SourceURL:    metaURL,
			Kind:         "metadata",
			UpstreamETag: respHeader.Get("ETag"),
		})
	}
	return body, nil
}
