package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	pypihandler "github.com/livehl/mirrorhub/internal/handlers/pypi"
	"github.com/livehl/mirrorhub/internal/platform"
	"github.com/livehl/mirrorhub/internal/router"
)

func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request, m *router.Match, plat platform.Platform, cfg config.Config, pypi config.PlatformConfig, prefetch, boost bool, onAcquired func(), onProgress func(done, total int64), taskID string) (string, string, error) {
	origURL := m.TargetURL
	headers := platform.FilterRequestHeaders(r.Header)
	cacheKey := cache.KeyFromURL(origURL)
	expectedSHA := downloader.LookupDigest(origURL)

	if keyer, ok := plat.(platform.PackageKeyer); ok {
		key, sha := keyer.PackageCacheIdentity(r.URL.Path, origURL, r.Header, cacheKey)
		cacheKey = key
		if sha != "" {
			expectedSHA = sha
			downloader.RememberDigest(origURL, sha)
		}
	}

	if auther, ok := plat.(platform.UpstreamAuther); ok {
		if err := auther.InjectUpstreamAuth(r.Context(), headers, cfg, r.URL.Path); err != nil {
			if entry, ok := s.cache.Get(cacheKey, m.Platform); ok {
				return s.servePackageHit(w, r, entry, boost, onAcquired, onProgress)
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			// 无本地缓存且鉴权/回源前失败：记 miss，避免访问日志显示为「-」
			return "miss", "proxy", err
		}
	}

	if entry, ok := s.cache.Get(cacheKey, m.Platform); ok {
		return s.servePackageHit(w, r, entry, boost, onAcquired, onProgress)
	}

	// HEAD 请求直接探测上游文件大小，绕过 task semaphore 避免被暂停的 prefetch 任务阻塞。
	if r.Method == http.MethodHead {
		// 给 HEAD 请求一个短超时，避免被上游慢连接阻塞
		headCtx, headCancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer headCancel()
		size, ct, _, headErr := s.dl.Head(headCtx, origURL, headers)
		if headErr != nil {
			http.Error(w, headErr.Error(), http.StatusBadGateway)
			return "miss", "proxy", headErr
		}
		if pypihandler.IsHTMLContentType(ct) {
			err := fmt.Errorf("upstream returned HTML for package path (not an artifact)")
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "proxy", err
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Mirrorhub-Strategy", "proxy")
		if d, ok := plat.(platform.PackageHeaderDecorator); ok {
			d.DecoratePackageHeaders(w)
		}
		if boost {
			w.Header().Set("X-Mirrorhub-Boost", "1")
		}
		w.WriteHeader(http.StatusOK)
		return "miss", "proxy", nil
	}

	if r.Method != http.MethodGet {
		c, err := s.handleIndexFallbackProxy(w, r, m, prefetch, boost, onAcquired)
		return c, "proxy", err
	}

	fetchURL := origURL
	var size int64 = -1
	hasSize := false
	// SkipUpstreamHead：交互冷路径跳过上游 HEAD；HF 等需 CL 校验的不设此标志
	skipHead := m.SkipUpstreamHead && !prefetch
	// 交互路径：Begin 时已 MarkPreparing；真正占槽下载时由 OnAcquired→MarkRunning
	if !skipHead {
		hs, headCT, finalURL, headErr := s.dl.Head(r.Context(), origURL, headers)
		if headErr == nil {
			if pypihandler.IsHTMLContentType(headCT) {
				err := fmt.Errorf("upstream returned HTML for package path (not an artifact)")
				http.Error(w, err.Error(), http.StatusBadGateway)
				return "miss", "proxy", err
			}
			size = hs
			hasSize = true
			if finalURL != "" {
				fetchURL = finalURL
			}
		}
	}

	fileBoost := boost
	if !prefetch && (skipHead || !hasSize) {
		// 无大小信息（跳过 HEAD / HEAD 失败）时默认插队，避免被预取占满
		fileBoost = true
	} else if !prefetch && cfg.Scheduler.SmallFileBoost.Enabled {
		maxBoost := int64(cfg.Scheduler.SmallFileBoost.MaxSizeKB) * 1024
		if hasSize && size >= 0 {
			if (pypi.Download.MinSize > 0 && size < pypi.Download.MinSize) || (maxBoost > 0 && size <= maxBoost) {
				fileBoost = true
			}
		}
	}

	opt := downloader.Options{
		URL:            fetchURL,
		SourceURL:      origURL,
		CacheKey:       cacheKey,
		Concurrency:    pypi.Download.Concurrency,
		ChunkSize:      pypi.Download.ChunkSize,
		MinSize:        pypi.Download.MinSize,
		TTLSeconds:     cfg.Cache.PackageTTLSeconds,
		Platform:       m.Platform,
		Prefetch:       prefetch,
		Boost:          fileBoost,
		TaskID:         taskID,
		Headers:        headers,
		ChunkTTLHours:  cfg.Cache.ChunkTTLHours,
		Kind:           "package",
		HasKnownSize:   hasSize,
		KnownSize:      size,
		ExpectedSHA256: expectedSHA,
		RangeHeader:    r.Header.Get("Range"),
		OnAcquired:     onAcquired,
		OnProgress:     onProgress,
	}

	if !prefetch {
		tw := &writeTrack{ResponseWriter: w}
		label, err := s.dl.ServePackage(r.Context(), tw, opt)
		strat := "stream"
		if label == "hit" {
			strat = "cache"
		} else if hasSize && size >= pypi.Download.MinSize && pypi.Download.MinSize > 0 {
			strat = "parallel-stream"
		}
		// 未写响应头就失败时补 502，避免净空 200
		if err != nil && !tw.wrote {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return label, strat, err
	}

	if err := s.cache.AllowPrefetchWrite(); err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return "miss", "parallel", err
	}

	res, err := s.dl.GetOrDownload(r.Context(), opt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return "miss", "parallel", err
	}
	label, _, err := serveDownloadResultFull(w, res, fileBoost)
	strat := res.Strategy
	if strat == "" {
		strat = "parallel"
	}
	return label, strat, err
}

// writeTrack 记录是否已向客户端写过响应（头或体）。
type writeTrack struct {
	http.ResponseWriter
	wrote bool
}

func (w *writeTrack) WriteHeader(statusCode int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *writeTrack) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

func (w *writeTrack) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) servePackageHit(w http.ResponseWriter, r *http.Request, entry *cache.Entry, boost bool, onAcquired func(), onProgress func(done, total int64)) (string, string, error) {
	if onAcquired != nil {
		onAcquired()
	}
	if onProgress != nil {
		onProgress(entry.Size, entry.Size)
	}
	etag := entry.ETag
	if etag == "" && entry.Digest != "" {
		etag = `"` + entry.Digest + `"`
	}
	if etag == "" {
		etag = fmt.Sprintf(`W/"%d"`, entry.Size)
	}
	if r.Method == http.MethodHead || r.Method == http.MethodGet {
		if matchETag(r.Header.Get("If-None-Match"), etag) {
			w.Header().Set("ETag", etag)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusNotModified)
			return "hit", "cache", nil
		}
	}
	if r.Method == http.MethodHead {
		ct := entry.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", etag)
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		return "hit", "cache", nil
	}
	// GET 命中：直接回本地缓存，禁止再打上游 HEAD（否则每次 HIT 都被上游探测拖慢）
	if r.Method == http.MethodGet {
		tw := &writeTrack{ResponseWriter: w}
		label, err := s.dl.ServeCachedEntry(tw, entry, boost, r.Header.Get("Range"), "HIT")
		if err != nil && !tw.wrote {
			// 元数据在、文件缺失：删坏条目并 502，避免空 200
			_ = s.cache.Delete(entry.Key)
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return label, "cache", err
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return "miss", "cache", fmt.Errorf("method not allowed")
}
