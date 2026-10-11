package proxy

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/downloader"
	"github.com/livehl/mirrorhub/internal/platform"
	"github.com/livehl/mirrorhub/internal/router"
)

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request, m *router.Match, cfg config.Config, pypi config.PlatformConfig, prefetch, boost bool, onAcquired func(), taskID string) (string, error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return s.handleIndexFallbackProxy(w, r, m, prefetch, boost, onAcquired)
	}

	cacheKey := cache.KeyFromURL(m.TargetURL)

	// HEAD：优先本地缓存（含过期），避免断网时透传上游失败
	if r.Method == http.MethodHead {
		if entry, ok := s.cache.Get(cacheKey, m.Platform); ok {
			return serveMetadataHead(w, entry, boost, "HIT", onAcquired)
		}
		if stale, ok := s.cache.GetStale(cacheKey); ok {
			return serveMetadataHead(w, stale, boost, "STALE", onAcquired)
		}
		return s.handleIndexFallbackProxy(w, r, m, prefetch, boost, onAcquired)
	}

	headers := platform.FilterRequestHeaders(r.Header)
	opt := downloader.Options{
		URL:           m.TargetURL,
		SourceURL:     m.TargetURL,
		CacheKey:      cacheKey,
		TTLSeconds:    cfg.Cache.IndexTTLSeconds,
		Platform:      m.Platform,
		Prefetch:      prefetch,
		Boost:         boost,
		TaskID:        taskID,
		Headers:       headers,
		ChunkTTLHours: cfg.Cache.ChunkTTLHours,
		Kind:          "metadata",
		Concurrency:   pypi.Download.Concurrency,
		ChunkSize:     pypi.Download.ChunkSize,
		RangeHeader:   r.Header.Get("Range"),
		OnAcquired:    onAcquired,
	}
	if prefetch {
		res, err := s.dl.GetOrDownload(r.Context(), opt)
		if err != nil {
			if stale, ok := s.cache.GetStale(cacheKey); ok {
				if onAcquired != nil {
					onAcquired()
				}
				return s.dl.ServeCachedEntry(w, stale, boost, r.Header.Get("Range"), "STALE")
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", err
		}
		return serveDownloadResult(w, res, boost)
	}
	label, err := s.dl.ServePackage(r.Context(), w, opt)
	if err != nil {
		if stale, ok := s.cache.GetStale(cacheKey); ok {
			if onAcquired != nil {
				onAcquired()
			}
			return s.dl.ServeCachedEntry(w, stale, boost, r.Header.Get("Range"), "STALE")
		}
	}
	return label, err
}

func serveMetadataHead(w http.ResponseWriter, entry *cache.Entry, boost bool, cacheLabel string, onAcquired func()) (string, error) {
	if onAcquired != nil {
		onAcquired()
	}
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if entry.ETag != "" {
		w.Header().Set("ETag", entry.ETag)
	}
	w.Header().Set("X-Cache", cacheLabel)
	w.Header().Set("X-Mirrorhub-Strategy", "cache")
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	w.WriteHeader(http.StatusOK)
	switch strings.ToUpper(cacheLabel) {
	case "STALE":
		return "stale", nil
	case "REVALIDATED":
		return "revalidated", nil
	default:
		return "hit", nil
	}
}

// handleIndexFallbackProxy 非 GET 的简单透传（仍占任务槽+限速）
func (s *Server) handleIndexFallbackProxy(w http.ResponseWriter, r *http.Request, m *router.Match, prefetch, boost bool, onAcquired func()) (string, error) {
	release, err := s.acquireProxyTask(r.Context(), m.Platform, boost, onAcquired)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return cacheLabelForErr(err), err
	}
	defer release()
	headers := platform.FilterRequestHeaders(r.Header)
	status, respHeader, body, err := s.dl.ProxyBytes(r.Context(), r.Method, m.TargetURL, headers, r.Body, m.Platform, prefetch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return "miss", err
	}
	ct := respHeader.Get("Content-Type")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writeProxyHeaders(w, respHeader, ct, status, "MISS", "proxy", boost)
	_, _ = w.Write(body)
	return "miss", nil
}
