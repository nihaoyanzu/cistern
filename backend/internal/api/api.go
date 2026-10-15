package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/auth"
	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	"github.com/livehl/mirrorhub/internal/metrics"
	"github.com/livehl/mirrorhub/internal/platform"
	_ "github.com/livehl/mirrorhub/internal/platform/docker"
	_ "github.com/livehl/mirrorhub/internal/platform/goproxy"
	_ "github.com/livehl/mirrorhub/internal/platform/huggingface"
	_ "github.com/livehl/mirrorhub/internal/platform/maven"
	_ "github.com/livehl/mirrorhub/internal/platform/npm"
	_ "github.com/livehl/mirrorhub/internal/platform/pypi"
	"github.com/livehl/mirrorhub/internal/prefetch"
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/scheduler"
	"github.com/livehl/mirrorhub/internal/traffic"
)

type Server struct {
	cfg      *config.Manager
	cache    *cache.Manager
	dl       *downloader.Engine
	limiters *ratelimit.Registry
	sched    *scheduler.Scheduler
	prefetch *prefetch.Service
	auth     *auth.Service
	traffic  *traffic.Recorder
	log      *zap.Logger
	webDir   string

	catalogMu         sync.RWMutex
	catalogNames      []string
	catalogNameSet    map[string]struct{}
	catalogURL        string
	catalogAt         time.Time
	catalogRefreshing bool
	catalogLastErr    string

	searchCacheMu sync.Mutex
	searchCache   map[string]searchCacheEntry
}

type searchCacheEntry struct {
	names []string
	at    time.Time
}

func New(cfg *config.Manager, c *cache.Manager, dl *downloader.Engine, lim *ratelimit.Registry, sched *scheduler.Scheduler, pf *prefetch.Service, authSvc *auth.Service, tr *traffic.Recorder, log *zap.Logger, webDir string) *Server {
	return &Server{
		cfg:         cfg,
		cache:       c,
		dl:          dl,
		limiters:    lim,
		sched:       sched,
		prefetch:    pf,
		auth:        authSvc,
		traffic:     tr,
		log:         log,
		webDir:      strings.TrimSpace(webDir),
		searchCache: map[string]searchCacheEntry{},
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(s.cors)
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	r.Handle("/metrics", promhttp.Handler())

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/login", s.login)
		r.Get("/public/guide", s.getPublicGuide)
		r.Group(func(r chi.Router) {
			r.Use(s.authRequired)
			r.Post("/logout", s.logout)
			r.Get("/me", s.me)
			r.Put("/password", s.changePassword)
			r.Get("/config", s.getConfig)
			r.Put("/config", s.putConfig)
			r.Post("/config/test", s.postAccessTest)
			r.Get("/stats", s.getStats)
			r.Get("/access", s.getAccess)
			r.Get("/queue", s.getQueue)
			r.Delete("/queue", s.clearQueue)
			r.Post("/prefetch", s.postPrefetch)
			r.Delete("/prefetch", s.deletePrefetch)
			r.Get("/cache", s.getCache)
			r.Delete("/cache", s.clearCache)
			r.Get("/packages", s.listPackages)
			r.Get("/packages/detail", s.getPackage)
			r.Get("/packages/{name}", s.getPackage)
			r.Delete("/packages/entry", s.deletePackageEntry)
		})
	})

	if s.webDir != "" {
		r.NotFound(spaFileServer(s.webDir))
	}
	return r
}

// spaFileServer 托管前端构建产物；找不到文件时回退 index.html（Vue Router history）
func spaFileServer(webDir string) http.HandlerFunc {
	root := http.Dir(webDir)
	fs := http.FileServer(root)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if f, err := root.Open(path); err == nil {
				_ = f.Close()
				fs.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFile(w, r, filepath.Join(webDir, "index.html"))
	}
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

func (s *Server) authRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.auth.Validate(bearerToken(r)); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body loginReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	token, err := s.auth.Login(r.Context(), body.Username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    token,
		"username": body.Username,
	})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(bearerToken(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	user, err := s.auth.Validate(bearerToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": user})
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	user, err := s.auth.Validate(bearerToken(r))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body changePasswordReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.auth.ChangePassword(r.Context(), user, body.OldPassword, body.NewPassword); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			// 用 400 而非 401，避免前端把「旧密码错误」当成会话失效并跳转登录
			http.Error(w, "当前密码不正确", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get())
}

type configUpdate struct {
	UpstreamProxy *string                          `json:"upstream_proxy"`
	Cache         *config.CacheConfig              `json:"cache"`
	RateLimit     *config.RateLimitConfig          `json:"rate_limit"`
	Scheduler     *config.SchedulerConfig          `json:"scheduler"`
	Platforms     map[string]config.PlatformConfig `json:"platforms"`
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var body configUpdate
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	prevUpstream := s.catalogRootURL()
	prevResumeOnIdle := s.cfg.Get().Scheduler.Prefetch.ResumeOnIdle
	err := s.cfg.Update(func(c *config.Config) {
		if body.UpstreamProxy != nil {
			c.Server.UpstreamProxy = *body.UpstreamProxy
		}
		if body.Cache != nil {
			c.Cache.MaxSizeGB = body.Cache.MaxSizeGB
			c.Cache.IndexTTLSeconds = body.Cache.IndexTTLSeconds
			c.Cache.PackageTTLSeconds = body.Cache.PackageTTLSeconds
			c.Cache.ChunkTTLHours = body.Cache.ChunkTTLHours
		}
		if body.RateLimit != nil {
			c.RateLimit = *body.RateLimit
		}
		if body.Scheduler != nil {
			c.Scheduler = *body.Scheduler
		}
		if body.Platforms != nil {
			if c.Platforms == nil {
				c.Platforms = map[string]config.PlatformConfig{}
			}
			for name, p := range body.Platforms {
				p.RateLimit = nil
				c.Platforms[name] = p
			}
		}
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg := s.cfg.Get()
	if body.RateLimit != nil {
		s.limiters.Update(cfg.RateLimit)
	}
	s.limiters.SetIdleRatio(cfg.Scheduler.Prefetch.IdleQuotaRatio)
	s.limiters.SetInteractiveTaskReserve(cfg.Scheduler.Prefetch.OnInteractive == "continue")
	if body.Cache != nil {
		s.cache.SetMaxSizeGB(cfg.Cache.MaxSizeGB)
	}
	if body.UpstreamProxy != nil && s.dl != nil {
		s.dl.SetUpstreamProxy(cfg.Server.UpstreamProxy)
	}
	// 热开 ResumeOnIdle：武装按平台 idle resume，不强制清仍有 P0 的平台
	if body.Scheduler != nil && !prevResumeOnIdle && cfg.Scheduler.Prefetch.ResumeOnIdle && s.sched != nil {
		s.sched.ResumePrefetchIfIdle()
	}
	if s.catalogRootURL() != prevUpstream {
		s.kickCatalogRefresh(true)
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) getStats(w http.ResponseWriter, _ *http.Request) {
	cs := s.cache.Stats()
	// 累计分平台为空时（升级前旧计数），用近期访问回填展示，避免仪表盘空白
	if len(cs.HitsByPlatform) == 0 && len(cs.MissesByPlatform) == 0 && s.traffic != nil {
		h, m := s.traffic.HitMissByPlatform()
		if len(h) > 0 || len(m) > 0 {
			cs.HitsByPlatform = h
			cs.MissesByPlatform = m
		}
	}
	metrics.CacheUsageRatio.Set(cs.UsageRatio)
	metrics.DiskFreeBytes.Set(float64(cs.DiskFreeBytes))
	metrics.SchedulerP0Active.Set(float64(s.sched.InteractiveActive()))
	out := map[string]any{
		"cache":              cs,
		"rate_limiters":      s.limiters.Snapshots(),
		"interactive_active": s.sched.InteractiveActive(),
		"resume_count":       s.sched.ResumeCount(),
	}
	if s.traffic != nil {
		out["traffic"] = s.traffic.SnapshotRates()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getAccess(w http.ResponseWriter, r *http.Request) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 20)
	if pageSize > 100 {
		pageSize = 100
	}
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	capacity := 200
	var items any = []struct{}{}
	total := 0
	var platforms []string
	if s.traffic != nil {
		list, n := s.traffic.ListAccess(page, pageSize, platform)
		total = n
		platforms = s.traffic.AccessPlatforms()
		items = list
		_ = capacity
	}
	if platforms == nil {
		platforms = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"platforms": platforms,
		"capacity":  capacity,
	})
}

func (s *Server) getQueue(w http.ResponseWriter, r *http.Request) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), 15)
	if pageSize > 100 {
		pageSize = 100
	}
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if status == "all" {
		status = ""
	}
	plat := strings.TrimSpace(r.URL.Query().Get("platform"))
	prioRaw := strings.TrimSpace(r.URL.Query().Get("priority"))
	prioSet := map[string]struct{}{}
	if prioRaw != "" {
		for _, p := range strings.Split(prioRaw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				prioSet[p] = struct{}{}
			}
		}
	}

	all := s.sched.List()
	running := 0
	filtered := make([]any, 0, len(all))
	for _, t := range all {
		if t.Status == "running" {
			running++
		}
		if status != "" && !strings.EqualFold(t.Status, status) {
			continue
		}
		if plat != "" && !strings.EqualFold(strings.TrimSpace(t.Platform), plat) {
			continue
		}
		if len(prioSet) > 0 {
			if _, ok := prioSet[t.Priority]; !ok {
				continue
			}
		}
		filtered = append(filtered, t)
	}
	total := len(filtered)
	start := (page - 1) * pageSize
	var pageTasks []any
	if start >= total {
		pageTasks = []any{}
	} else {
		end := start + pageSize
		if end > total {
			end = total
		}
		pageTasks = filtered[start:end]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":         pageTasks,
		"total":         total,
		"page":          page,
		"page_size":     pageSize,
		"running_count": running,
	})
}

func (s *Server) clearQueue(w http.ResponseWriter, _ *http.Request) {
	n := s.sched.ClearFinished()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n})
}

type prefetchReq struct {
	URLs     []string `json:"urls"`
	Text     string   `json:"text"`     // 粘贴的 requirements / 依赖文件内容
	Platform string   `json:"platform"` // 可选：前端当前模块，强制该平台解析
}

func (s *Server) postPrefetch(w http.ResponseWriter, r *http.Request) {
	var body prefetchReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items := append([]string{}, body.URLs...)
	var skipped []string
	platHint := strings.ToLower(strings.TrimSpace(body.Platform))
	if platHint == "hf" {
		platHint = "huggingface"
	}
	if platHint == "go" {
		platHint = "goproxy"
	}

	if strings.TrimSpace(body.Text) != "" {
		if platHint != "" {
			p := platform.ByName(platHint)
			if p == nil {
				http.Error(w, "未知平台: "+platHint, http.StatusBadRequest)
				return
			}
			det, ok := p.(platform.TextDetector)
			if !ok {
				http.Error(w, "该平台不支持粘贴预取", http.StatusBadRequest)
				return
			}
			if !det.LookLikePrefetchText(body.Text) {
				http.Error(w, "粘贴内容不符合当前模块格式", http.StatusBadRequest)
				return
			}
			res, ok := det.ParsePrefetchText(body.Text)
			if !ok {
				http.Error(w, "无法解析预取内容", http.StatusBadRequest)
				return
			}
			items = append(items, res.Items...)
			skipped = append(skipped, res.Skipped...)
		} else {
			for _, det := range platform.TextDetectors() {
				if !det.LookLikePrefetchText(body.Text) {
					continue
				}
				res, ok := det.ParsePrefetchText(body.Text)
				if !ok || len(res.Items) == 0 {
					// 误匹配且无有效条目时继续尝试下一检测器
					continue
				}
				items = append(items, res.Items...)
				skipped = append(skipped, res.Skipped...)
				break
			}
		}
	}

	// 指定平台时，过滤非本平台 Owns 的条目
	var owner platform.PrefetchExpander
	if platHint != "" {
		if p := platform.ByName(platHint); p != nil {
			if e, ok := p.(platform.PrefetchExpander); ok {
				owner = e
			}
		}
	}

	seen := map[string]struct{}{}
	uniq := make([]string, 0, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" {
			continue
		}
		if _, ok := seen[it]; ok {
			continue
		}
		if owner != nil && !owner.OwnsPrefetchItem(it) {
			skipped = append(skipped, it)
			continue
		}
		seen[it] = struct{}{}
		uniq = append(uniq, it)
	}
	if len(uniq) == 0 {
		msg := "未解析到可预拉取的包需求"
		if len(skipped) > 0 {
			msg += "（已跳过选项/VCS 等行）"
		}
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	s.prefetch.Enqueue(uniq)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"enqueued": len(uniq),
		"items":    uniq,
		"skipped":  skipped,
	})
}

func (s *Server) deletePrefetch(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	// 预取 P1/P2 与交互 P0 均可取消（Cancel 内走 CancelTask）
	ok := s.prefetch.Cancel(id)
	if !ok && s.sched != nil {
		_, ok = s.sched.CancelTask(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": ok})
}

func (s *Server) getCache(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cache.Stats())
}

func (s *Server) clearCache(w http.ResponseWriter, _ *http.Request) {
	// clearing 置位后再取消在飞任务（resume=false），堵住 Cancel→clearing 窗口且清空前不恢复预取
	if err := s.cache.Clear(func() {
		if s.sched != nil {
			s.sched.CancelAllActive(false)
		}
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.sched != nil {
		s.sched.ResumePrefetchIfIdle()
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
