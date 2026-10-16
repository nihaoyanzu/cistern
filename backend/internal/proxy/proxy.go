package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	npmhandler "github.com/livehl/mirrorhub/internal/handlers/npm"
	pypihandler "github.com/livehl/mirrorhub/internal/handlers/pypi"
	"github.com/livehl/mirrorhub/internal/metrics"
	"github.com/livehl/mirrorhub/internal/platform"
	_ "github.com/livehl/mirrorhub/internal/platform/docker"      // 注册 Docker 平台
	_ "github.com/livehl/mirrorhub/internal/platform/goproxy"     // 注册 Go modules 平台
	_ "github.com/livehl/mirrorhub/internal/platform/huggingface" // 注册 Hugging Face 平台
	_ "github.com/livehl/mirrorhub/internal/platform/maven"       // 注册 Maven/Gradle 平台（须在 npm 之前）
	_ "github.com/livehl/mirrorhub/internal/platform/npm"         // 注册 npm 平台
	_ "github.com/livehl/mirrorhub/internal/platform/pypi"        // 注册 PyPI 平台
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/router"
	"github.com/livehl/mirrorhub/internal/scheduler"
	"github.com/livehl/mirrorhub/internal/traffic"
)

// revalResult 条件请求续期的结果，供并发请求共享
type revalResult struct {
	done         chan struct{}
	entry        *cache.Entry // 续期后的条目（nil 表示未续期成功）
	body         []byte       // 续期成功时的旧内容，或内容变更时的新正文
	contentType  string
	upstreamETag string // 内容变更路径写入缓存用
}

type Server struct {
	cfg      *config.Manager
	cache    *cache.Manager
	dl       *downloader.Engine
	sched    *scheduler.Scheduler
	limiters *ratelimit.Registry
	traffic  *traffic.Recorder
	log      *zap.Logger

	revalLocks sync.Map // cacheKey -> chan struct{}，per-key 续期锁防惊群
}

func New(cfg *config.Manager, c *cache.Manager, dl *downloader.Engine, sched *scheduler.Scheduler, lim *ratelimit.Registry, tr *traffic.Recorder, log *zap.Logger) *Server {
	return &Server{cfg: cfg, cache: c, dl: dl, sched: sched, limiters: lim, traffic: tr, log: log}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.HandleFunc("/*", s.handle)
	return r
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()

	for _, p := range platform.All() {
		if prober, ok := p.(platform.LocalProber); ok {
			if prober.TryLocalProbe(w, r, cfg) {
				return
			}
		}
	}

	// Corepack 等在 COREPACK_NPM_REGISTRY 带尾斜杠时会拼出 //yarn、//@yarnpkg/...，
	// npmmirror 对双斜杠直接 404；此处收敛路径再匹配/回源。
	reqPath := path.Clean(r.URL.Path)
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	if reqPath != r.URL.Path {
		r = r.Clone(r.Context())
		r.URL.Path = reqPath
	}

	mr := platform.Match(reqPath, cfg)
	if mr == nil {
		http.Error(w, "no matching rule", http.StatusNotFound)
		return
	}
	m := mr.Match
	pcfg := cfg.Platforms[mr.Platform.Name()]

	if m.ReadOnly {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			http.Error(w, m.Platform+" mirror is read-only", http.StatusMethodNotAllowed)
			return
		}
	}

	prio := scheduler.PriorityInteractive
	if r.Header.Get("X-Mirrorhub-Priority") == "prefetch" {
		prio = scheduler.PriorityPrefetch
	}

	boost := false
	if prio != scheduler.PriorityPrefetch && cfg.Scheduler.SmallFileBoost.Enabled {
		if m.IsIndex || m.IsMetadata || m.SmallFileBoost {
			boost = true
		}
	}

	cw := &traffic.CountWriter{ResponseWriter: w, Rec: s.traffic}
	strategy := string(m.Strategy)
	// 仅制品下载占用交互槽并暂停预取；索引/metadata 短请求不触发 pause 抖动
	holdInteractive := prio == scheduler.PriorityInteractive && !m.IsIndex && !m.IsMetadata
	taskID := s.sched.Begin(m.Platform, m.TargetURL, prio, boost, holdInteractive)
	// 管理台可强制取消：派生可取消 ctx 并登记
	reqCtx, reqCancel := context.WithCancel(r.Context())
	defer reqCancel()
	s.sched.BindCancel(taskID, reqCancel)
	defer s.sched.UnbindCancel(taskID)
	r = r.WithContext(reqCtx)
	if holdInteractive {
		// 交互制品：立刻标 preparing，避免 HEAD/鉴权阶段显示「排队中」
		s.sched.MarkPreparing(taskID)
	}
	onAcquired := func() { s.sched.MarkRunning(taskID) }
	onHit := func() { s.sched.MarkHit(taskID) }
	onWaiting := func() { s.sched.MarkWaiting(taskID) }
	onProgress := func(done, total int64) { s.sched.UpdateProgress(taskID, done, total) }
	var taskErr error
	cacheLabel := "na"
	defer func() {
		// 仅 OnAcquired→MarkRunning 才占 P0；未下载完成的 preparing/waiting 直接 End
		s.sched.End(taskID, taskErr)
		metrics.RequestsTotal.WithLabelValues(m.Platform, strategy, cacheLabel).Inc()
		if s.traffic != nil {
			// 下游字节已在 CountWriter.Write 中实时计入，此处不再 AddUpload
			if prio != scheduler.PriorityPrefetch {
				status := "ok"
				if taskErr != nil {
					status = "error"
				}
				s.traffic.Record(traffic.Access{
					IP:       traffic.ClientIP(r),
					Method:   r.Method,
					Path:     r.URL.Path,
					Platform: m.Platform,
					Bytes:    cw.N,
					Cache:    cacheLabel,
					Status:   status,
				})
			}
		}
	}()

	switch {
	case m.IsIndex:
		strategy = "proxy"
		cacheLabel, taskErr = s.handleIndex(cw, r, m, mr.Platform, cfg, prio == scheduler.PriorityPrefetch, boost, onAcquired)
	case m.IsMetadata:
		strategy = "metadata"
		cacheLabel, taskErr = s.handleMetadata(cw, r, m, cfg, pcfg, prio == scheduler.PriorityPrefetch, boost, onAcquired, taskID)
	case m.Strategy == router.StrategyParallel:
		cacheLabel, strategy, taskErr = s.handlePackage(cw, r, m, mr.Platform, cfg, pcfg, prio == scheduler.PriorityPrefetch, boost, onAcquired, onHit, onWaiting, onProgress, taskID)
	default:
		http.Error(cw, "unknown strategy", http.StatusInternalServerError)
	}
}

func (s *Server) acquireProxyTask(ctx context.Context, platform string, boost bool, onAcquired func()) (func(), error) {
	pl := s.limiters.Get(platform)
	if pl == nil {
		if onAcquired != nil {
			onAcquired()
		}
		return func() {}, nil
	}
	if err := pl.AcquireTask(ctx, boost, true); err != nil {
		return nil, err
	}
	if onAcquired != nil {
		onAcquired()
	}
	return func() { pl.ReleaseTask(boost) }, nil
}

func writeProxyHeaders(w http.ResponseWriter, respHeader http.Header, ct string, status int, cacheLabel, strategy string, boost bool) {
	outH := platform.FilterResponseHeaders(respHeader)
	stripXetFromHeader(outH)
	for k, vals := range outH {
		lk := strings.ToLower(k)
		if lk == "content-type" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("X-Cache", cacheLabel)
	w.Header().Set("X-Mirrorhub-Strategy", strategy)
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	w.WriteHeader(status)
}
func rememberIndexDigests(body []byte, pageURL string) {
	for _, ref := range pypihandler.ExtractArtifactRefs(body, pageURL) {
		if ref.SHA256 != "" {
			downloader.RememberDigest(ref.URL, ref.SHA256)
		}
	}
	for u, integrity := range npmhandler.ExtractTarballDigests(body) {
		downloader.RememberIntegrity(u, integrity)
	}
}

// cacheLabelForErr 访问日志用的可读缓存标签：回源失败→miss，超时/取消单独标注。
func cacheLabelForErr(err error) string {
	if err == nil {
		return "miss"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "context canceled") || strings.Contains(msg, "context cancelled") {
		return "cancelled"
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "timeout"
	}
	return "miss"
}

func matchETag(inm, etag string) bool {
	inm = strings.TrimSpace(inm)
	if inm == "" || etag == "" {
		return false
	}
	if inm == "*" {
		return true
	}
	for _, part := range strings.Split(inm, ",") {
		if strings.TrimSpace(part) == etag {
			return true
		}
	}
	return false
}

func (s *Server) serveDownloadResult(w http.ResponseWriter, res *downloader.Result, boost bool) (string, error) {
	label, _, err := s.serveDownloadResultFull(w, res, boost)
	return label, err
}

func (s *Server) serveDownloadResultFull(w http.ResponseWriter, res *downloader.Result, boost bool) (string, bool, error) {
	if res == nil || res.Entry == nil || strings.TrimSpace(res.Entry.Key) == "" {
		err := errors.New("empty download result")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return "miss", false, err
	}
	ent, f, ok := s.cache.Open(res.Entry.Key)
	if !ok {
		err := errors.New("cache open miss")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return "miss", false, err
	}
	defer f.Close()
	ct := res.ContentType
	if ct == "" {
		ct = ent.ContentType
	}
	size := res.Size
	if size <= 0 {
		size = ent.Size
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	cacheLabel := "miss"
	strat := res.Strategy
	if strat == "" {
		strat = "parallel"
	}
	if res.FromCache {
		w.Header().Set("X-Cache", "HIT")
		cacheLabel = "hit"
		strat = "cache"
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	w.Header().Set("X-Mirrorhub-Strategy", strat)
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	if ent.RepoCommit != "" {
		w.Header().Set("X-Repo-Commit", ent.RepoCommit)
	}
	_, err := io.Copy(w, f)
	return cacheLabel, false, err
}
