package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/livehl/mirrorhub/internal/api"
	"github.com/livehl/mirrorhub/internal/auth"
	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	"github.com/livehl/mirrorhub/internal/prefetch"
	"github.com/livehl/mirrorhub/internal/proxy"
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/scheduler"
	"github.com/livehl/mirrorhub/internal/traffic"
	"github.com/livehl/mirrorhub/internal/version"
)

func main() {
	loadDotEnv()

	boot := config.LoadBootstrap()
	cfgMgr, err := config.Open(boot)
	if err != nil {
		panic(err)
	}
	defer cfgMgr.Close()
	cfg := cfgMgr.Get()

	log, err := newLogger(cfg.Logging.Level, boot.DataDir)
	if err != nil {
		panic(err)
	}
	defer log.Sync() //nolint:errcheck

	if err := os.MkdirAll(cfg.Cache.Dir, 0o755); err != nil {
		log.Fatal("mkdir cache", zap.Error(err))
	}
	absCache, _ := filepath.Abs(cfg.Cache.Dir)
	log.Info("cache dir", zap.String("path", absCache))

	cacheMgr, err := cache.New(cfg.Cache.Dir, cfg.Cache.MaxSizeGB, cfgMgr.Store())
	if err != nil {
		log.Fatal("cache init", zap.Error(err))
	}
	defer cacheMgr.Close()
	if moved, skipped, missing, pruned := cacheMgr.ObjectLayoutMigrateStats(); moved > 0 || pruned > 0 || missing > 0 {
		log.Info("cache object layout migrate",
			zap.Int("moved", moved),
			zap.Int("skipped", skipped),
			zap.Int("missing", missing),
			zap.Int("pruned_empty_dirs", pruned),
		)
	}
	downloader.InitDigestPersist(cfg.Cache.Dir)
	if n, err := cacheMgr.PurgeIncomplete(); err != nil {
		log.Fatal("purge incomplete cache", zap.Error(err))
	} else if n > 0 {
		log.Info("purged incomplete cache entries", zap.Int("count", n))
	}
	if n, err := cacheMgr.PurgeTempFiles(); err != nil {
		log.Warn("purge temp files", zap.Error(err))
	} else if n > 0 {
		log.Info("purged orphan temp files", zap.Int("count", n))
	}

	limiters := ratelimit.NewRegistry(cfg.RateLimit, cfg.Scheduler.Prefetch.IdleQuotaRatio)
	limiters.SetInteractiveTaskReserve(cfg.Scheduler.Prefetch.OnInteractive == "continue")
	sched := scheduler.New(limiters, func() config.SchedulerConfig {
		return cfgMgr.Get().Scheduler
	}, log)

	tr := traffic.New(cfgMgr.Store())
	defer tr.Close()
	dl := downloader.New(cacheMgr, limiters, cfg.Server.UpstreamProxy, log, func() float64 {
		return cfgMgr.Get().Scheduler.Prefetch.IdleQuotaRatio
	}, tr)
	dl.SetPrefetchGate(sched.PrefetchMayRun)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pf := prefetch.New(cfgMgr, cacheMgr, dl, sched, log)
	pf.Start(ctx)

	authSvc, err := auth.New(cfgMgr.Store(), boot.DataDir)
	if err != nil {
		log.Fatal("auth init", zap.Error(err))
	}

	proxySrv := proxy.New(cfgMgr, cacheMgr, dl, sched, limiters, tr, log)
	if v := strings.TrimSpace(os.Getenv("MIRRORHUB_WEB_DIR")); v != "" {
		log.Warn("MIRRORHUB_WEB_DIR 已废弃，管理台仅使用嵌入前端，该变量将被忽略", zap.String("value", v))
	}

	adminSrv := api.New(cfgMgr, cacheMgr, dl, limiters, sched, pf, authSvc, tr, log)
	adminSrv.StartCatalog(ctx)

	cfg = cfgMgr.Get()
	logPlatforms(log, cfg)

	proxyHTTP := &http.Server{Addr: cfg.Server.ProxyAddr, Handler: proxySrv.Routes()}
	adminHTTP := &http.Server{Addr: cfg.Server.AdminAddr, Handler: adminSrv.Routes()}

	go func() {
		log.Info("proxy listening", zap.String("addr", cfg.Server.ProxyAddr))
		if err := proxyHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("proxy server", zap.Error(err))
		}
	}()
	go func() {
		log.Info("admin listening",
			zap.String("addr", cfg.Server.AdminAddr),
			zap.String("web", "embed"),
			zap.String("version", version.Version),
		)
		if err := adminHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("admin server", zap.Error(err))
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	log.Info("shutting down")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = proxyHTTP.Shutdown(shutdownCtx)
	_ = adminHTTP.Shutdown(shutdownCtx)
}

func logPlatforms(log *zap.Logger, cfg config.Config) {
	names := make([]string, 0, len(cfg.Platforms))
	for name := range cfg.Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	enabled := make([]string, 0, len(names))
	disabled := make([]string, 0, len(names))
	for _, name := range names {
		if cfg.Platforms[name].Enabled {
			enabled = append(enabled, name)
		} else {
			disabled = append(disabled, name)
		}
	}
	log.Info("platforms",
		zap.Strings("enabled", enabled),
		zap.Strings("disabled", disabled),
	)
}

func loadDotEnv() {
	candidates := []string{".env", "backend/.env"}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(wd, ".env"),
			filepath.Join(wd, "backend", ".env"),
			filepath.Join(filepath.Dir(wd), "backend", ".env"),
		)
	}
	for _, p := range candidates {
		if err := godotenv.Load(p); err == nil {
			return
		}
	}
}

func newLogger(level, dataDir string) (*zap.Logger, error) {
	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	logPath := filepath.Join(logDir, time.Now().Format("mirrorhub-20060102.log"))
	absLog, _ := filepath.Abs(logPath)

	cfg := zap.NewProductionConfig()
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// 同时输出到控制台与 data/logs/mirrorhub-YYYYMMDD.log
	cfg.OutputPaths = []string{"stdout", absLog}
	cfg.ErrorOutputPaths = []string{"stderr", absLog}
	switch level {
	case "debug":
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "warn":
		cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		cfg.Level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	log, err := cfg.Build()
	if err != nil {
		return nil, err
	}
	log.Info("log file", zap.String("path", absLog))
	return log, nil
}
