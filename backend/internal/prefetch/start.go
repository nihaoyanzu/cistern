package prefetch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/downloader"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/scheduler"
	"go.uber.org/zap"
)

func (s *Service) startOne(ctx context.Context, rawURL, platHint string) {
	s.startOneAttempt(ctx, rawURL, platHint, 0)
}

func (s *Service) startOneAttempt(ctx context.Context, rawURL, platHint string, attempt int) {
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	gov := s.dl.HostGov()
	if gov != nil && !gov.TryBeginPrefetch(host) {
		// Host 文件槽满：入等待队列，不消耗 attempt
		s.enqueueWaiting(rawURL, platHint, attempt)
		return
	}

	s.mu.Lock()
	if _, ok := s.cancel[rawURL]; ok {
		s.mu.Unlock()
		if gov != nil {
			gov.EndPrefetch(host)
			go s.drainWaiting(s.root)
		}
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	s.cancel[rawURL] = cancel
	s.mu.Unlock()

	go func() {
		defer func() {
			if gov != nil {
				gov.EndPrefetch(host)
			}
			s.mu.Lock()
			delete(s.cancel, rawURL)
			s.mu.Unlock()
			cancel()
			// 释放文件槽后唤醒等待中的同批预取
			go s.drainWaiting(s.root)
		}()
		cfg := s.cfg.Get()
		plat, pcfg, platName := resolvePlatform(cfg, rawURL, platHint)
		taskID := s.sched.Begin(platName, rawURL, scheduler.PriorityPrefetch, false, false)
		s.sched.BindCancel(taskID, cancel)
		defer s.sched.UnbindCancel(taskID)
		if plat != nil && !pcfg.Enabled {
			s.sched.MarkRunning(taskID)
			err := fmt.Errorf("%s 模块未启用", platName)
			s.sched.End(taskID, err)
			s.log.Warn("prefetch skipped disabled platform", zap.String("url", rawURL), zap.Error(err))
			return
		}
		cacheKey := cache.KeyFromURL(rawURL)
		headers := http.Header{}
		expectedSHA := downloader.LookupDigest(rawURL)
		reqPath := ""
		if u, err := url.Parse(rawURL); err == nil {
			reqPath = u.Path
		}
		if plat != nil {
			if keyer, ok := plat.(platform.PackageKeyer); ok {
				key, sha := keyer.PackageCacheIdentity(reqPath, rawURL, headers, cacheKey)
				cacheKey = key
				if sha != "" {
					expectedSHA = sha
					downloader.RememberDigest(rawURL, sha)
				}
			}
			if auther, ok := plat.(platform.UpstreamAuther); ok {
				if err := auther.InjectUpstreamAuth(cctx, headers, cfg, reqPath); err != nil {
					s.sched.MarkRunning(taskID)
					s.sched.End(taskID, err)
					s.log.Warn("prefetch auth failed", zap.String("url", rawURL), zap.Error(err))
					return
				}
			}
		}
		onAuthFail := func(actx context.Context, h http.Header, challenge http.Header) error {
			if plat == nil {
				return fmt.Errorf("no platform for auth refresh")
			}
			if inv, ok := plat.(platform.UpstreamAuthInvalidator); ok {
				inv.InvalidateUpstreamAuth(cfg, reqPath, challenge)
			}
			auther, ok := plat.(platform.UpstreamAuther)
			if !ok {
				return fmt.Errorf("platform has no upstream auth")
			}
			headers.Del("Authorization")
			if err := auther.InjectUpstreamAuth(actx, headers, cfg, reqPath); err != nil {
				return err
			}
			for k, vals := range headers {
				h[k] = append([]string(nil), vals...)
			}
			return nil
		}
		_, err := s.dl.GetOrDownload(cctx, downloader.Options{
			URL:            rawURL,
			SourceURL:      rawURL,
			CacheKey:       cacheKey,
			Concurrency:    pcfg.Download.Concurrency,
			ChunkSize:      pcfg.Download.ChunkSize,
			MinSize:        pcfg.Download.MinSize,
			TTLSeconds:     cfg.Cache.PackageTTLSeconds,
			Platform:       platName,
			Prefetch:       true,
			TaskID:         taskID,
			Headers:        headers,
			ChunkTTLHours:  cfg.Cache.ChunkTTLHours,
			Kind:           "package",
			ExpectedSHA256: expectedSHA,
			OnAcquired:     func() { s.sched.MarkRunning(taskID) },
			OnProgress:     func(done, total int64) { s.sched.UpdateProgress(taskID, done, total) },
			OnAuthFail:     onAuthFail,
		})
		if err != nil {
			cl := downloader.ClassOf(err)
			recoverable := cl == downloader.ClassTransient || cl == downloader.ClassRateLimit
			if recoverable && attempt+1 < len(delayedBackoff) && cctx.Err() == nil {
				// 本轮可见为 error（带 code）；chunk 进度保留，延迟后再开新任务续传
				s.sched.End(taskID, err)
				s.log.Warn("prefetch defer retry",
					zap.String("url", rawURL),
					zap.Int("next_attempt", attempt+1),
					zap.Error(err),
				)
				s.enqueueDelayed(rawURL, platHint, attempt+1)
				return
			}
			s.sched.End(taskID, err)
			if !strings.Contains(err.Error(), "context canceled") {
				s.log.Warn("prefetch failed", zap.String("url", rawURL), zap.Error(err))
			}
			return
		}
		s.sched.End(taskID, nil)
	}()
}
