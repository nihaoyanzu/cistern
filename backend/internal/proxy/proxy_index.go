package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/config"
	"github.com/livehl/mirrorhub/internal/platform"
	"github.com/livehl/mirrorhub/internal/router"
)

// handleIndex 缓存改写前正文，每次响应按客户端访问 Host 改写
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request, m *router.Match, plat platform.Platform, cfg config.Config, prefetch, boost bool, onAcquired func()) (string, error) {
	cfg = applyRequestPublicHost(cfg, r)
	release, err := s.acquireProxyTask(r.Context(), m.Platform, boost, onAcquired)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return cacheLabelForErr(err), err
	}
	defer release()

	cacheKey := m.Platform + ":index:" + cache.KeyFromURL(m.TargetURL)
	clientAccept := r.Header.Get("Accept")
	if keyer, ok := plat.(platform.IndexKeyer); ok {
		if v := keyer.IndexCacheVariant(clientAccept); v != "" {
			cacheKey += ":" + v
		}
	}

	// ---------- 1. 缓存命中（未过期）→ 直接返回 ----------
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if entry, data, ok := s.cache.ReadAll(cacheKey, m.Platform); ok {
			return s.serveCachedIndex(w, r, entry, data, plat, cfg, boost, "HIT")
		}
	}

	// ---------- 2. 缓存 miss：尝试条件请求续期（per-key 去重防惊群）----------
	headers := platform.FilterRequestHeaders(r.Header)
	// 客户端条件请求针对我们改写后的 ETag，不能原样转给上游
	headers.Del("If-None-Match")
	headers.Del("If-Modified-Since")
	if accepter, ok := plat.(platform.IndexAccepter); ok {
		accepter.PrepareIndexHeaders(clientAccept, headers)
	}
	if auther, ok := plat.(platform.UpstreamAuther); ok {
		if err := auther.InjectUpstreamAuth(r.Context(), headers, cfg, r.URL.Path); err != nil {
			if stale, data, ok := s.cache.ReadAllStale(cacheKey); ok {
				return s.serveCachedIndex(w, r, stale, data, plat, cfg, boost, "STALE")
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			// 无缓存可回退时记 miss（如 Docker 换票失败），访问日志展示「未命中」而非「-」
			return "miss", err
		}
	}

	var staleEntry *cache.Entry
	var staleData []byte
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if stale, data, ok := s.cache.ReadAllStale(cacheKey); ok {
			staleEntry = stale
			staleData = data
			if r.Method == http.MethodGet {
				result := s.doRevalidation(cacheKey, m.TargetURL, m.Platform, stale, data, headers, cfg.Cache.IndexTTLSeconds)
				<-result.done
				if result.entry != nil && len(result.body) > 0 {
					// 续期成功 → 用旧内容响应
					return s.serveRevalidated(w, r, result, plat, cfg, boost)
				}
				// 续期拿到了新正文：写入缓存后按 MISS 响应，避免再打一次上游
				if len(result.body) > 0 && result.contentType != "" {
					body := result.body
					body, _ = platform.MaybeGunzip(body)
					ct := indexContentType(plat, result.contentType, clientAccept, body)
					if _, err := s.cache.PutBytes(cacheKey, body, ct, cfg.Cache.IndexTTLSeconds, cache.Meta{
						SourceURL:    m.TargetURL,
						Kind:         "index",
						UpstreamETag: result.upstreamETag,
					}); err != nil && s.log != nil {
						s.log.Warn("index cache put failed", zap.String("key", cacheKey), zap.Error(err))
					}
					rememberIndexDigests(body, m.TargetURL)
					tr := plat.TransformIndex(body, ct, cfg, clientAccept, m.TargetURL)
					w.Header().Set("Content-Length", strconv.Itoa(len(tr.Body)))
					w.Header().Set("ETag", tr.ETag)
					decorateIndexHeaders(w, plat, tr.Body)
					writeProxyHeaders(w, http.Header{}, tr.ContentType, http.StatusOK, "MISS", "index", boost)
					_, _ = w.Write(tr.Body)
					return "miss", nil
				}
			}
			// 续期失败 / HEAD → 直接回退过期缓存（不依赖后续全量 GET）
			return s.serveCachedIndex(w, r, stale, data, plat, cfg, boost, "STALE")
		}
	}

	// ---------- 3. 全量 GET（无本地条目时）----------
	status, respHeader, body, getErr := s.dl.ProxyBytes(r.Context(), r.Method, m.TargetURL, headers, r.Body, m.Platform, prefetch)
	if getErr != nil {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if staleEntry == nil {
				if stale, data, ok := s.cache.ReadAllStale(cacheKey); ok {
					staleEntry = stale
					staleData = data
				}
			}
			if staleEntry != nil {
				return s.serveCachedIndex(w, r, staleEntry, staleData, plat, cfg, boost, "STALE")
			}
		}
		if statusSuggestsAuth(getErr) {
			if inv, ok := plat.(platform.UpstreamAuthInvalidator); ok {
				inv.InvalidateUpstreamAuth(cfg, r.URL.Path)
			}
		}
		http.Error(w, getErr.Error(), http.StatusBadGateway)
		return "miss", getErr
	}

	body, _ = platform.MaybeGunzip(body)
	ct := indexContentType(plat, respHeader.Get("Content-Type"), clientAccept, body)
	if r.Method == http.MethodGet && status >= 200 && status < 300 {
		if _, err := s.cache.PutBytes(cacheKey, body, ct, cfg.Cache.IndexTTLSeconds, cache.Meta{
			SourceURL:    m.TargetURL,
			Kind:         "index",
			UpstreamETag: respHeader.Get("ETag"),
		}); err != nil && s.log != nil {
			s.log.Warn("index cache put failed", zap.String("key", cacheKey), zap.Error(err))
		}
	}
	rememberIndexDigests(body, m.TargetURL)
	result := plat.TransformIndex(body, ct, cfg, clientAccept, m.TargetURL)
	if matchETag(r.Header.Get("If-None-Match"), result.ETag) {
		w.Header().Set("ETag", result.ETag)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(http.StatusNotModified)
		return "miss", nil
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(result.Body)))
	w.Header().Set("ETag", result.ETag)
	stripXetFromHeader(respHeader)
	writeProxyHeaders(w, respHeader, result.ContentType, status, "MISS", "index", boost)
	decorateIndexHeaders(w, plat, result.Body)
	if r.Method != http.MethodHead {
		_, _ = w.Write(result.Body)
	}
	return "miss", nil
}

// doRevalidation 执行 per-key 条件请求续期，防止惊群效应。
// 优先 If-None-Match；镜像无 ETag/不支持 304 时回退全文比对。
func (s *Server) doRevalidation(cacheKey, targetURL, platName string, stale *cache.Entry, oldData []byte, headers http.Header, ttlSeconds int) *revalResult {
	val, loaded := s.revalLocks.LoadOrStore(cacheKey, &revalResult{done: make(chan struct{})})
	result := val.(*revalResult)
	if loaded {
		return result
	}
	go func() {
		defer func() {
			s.revalLocks.Delete(cacheKey)
			close(result.done)
		}()

		if len(oldData) == 0 {
			if s.log != nil {
				s.log.Warn("revalidation missing stale body", zap.String("key", cacheKey))
			}
			return
		}

		renewOK := func() bool {
			if err := s.cache.RenewTTL(cacheKey, ttlSeconds); err != nil {
				if s.log != nil {
					s.log.Warn("RenewTTL failed", zap.String("key", cacheKey), zap.Error(err))
				}
				return false
			}
			result.entry = stale
			result.body = oldData
			result.contentType = stale.ContentType
			return true
		}

		if strings.TrimSpace(stale.UpstreamETag) != "" {
			condHeaders := headers.Clone()
			condHeaders.Set("If-None-Match", stale.UpstreamETag)
			condStatus, condErr := s.dl.ProxyHEAD(context.Background(), targetURL, condHeaders)
			if condErr == nil && condStatus == http.StatusNotModified {
				_ = renewOK()
				return
			}
			if condErr != nil && s.log != nil {
				s.log.Warn("revalidation HEAD failed, will fallback to GET",
					zap.String("url", targetURL), zap.Error(condErr))
			} else if condStatus >= 400 && s.log != nil {
				s.log.Warn("revalidation upstream error",
					zap.String("url", targetURL), zap.Int("status", condStatus))
			}
		}

		// 无 ETag 或不支持 304：全量 GET 后比对正文
		getHeaders := headers.Clone()
		getHeaders.Del("If-None-Match")
		getHeaders.Del("If-Modified-Since")
		status, respHeader, body, getErr := s.dl.ProxyBytes(context.Background(), http.MethodGet, targetURL, getHeaders, nil, platName, false)
		if getErr != nil {
			if s.log != nil {
				s.log.Warn("revalidation GET failed", zap.String("url", targetURL), zap.Error(getErr))
			}
			return
		}
		if status < 200 || status >= 300 {
			return
		}
		body, _ = platform.MaybeGunzip(body)
		oldNorm, _ := platform.MaybeGunzip(oldData)
		if bytes.Equal(body, oldNorm) {
			_ = renewOK()
			return
		}
		// 内容已变：交给调用方写入新缓存，避免再 GET 一次
		result.body = body
		result.contentType = respHeader.Get("Content-Type")
		result.upstreamETag = respHeader.Get("ETag")
	}()
	return result
}

// serveRevalidated 从续期结果响应客户端
func (s *Server) serveRevalidated(w http.ResponseWriter, r *http.Request, result *revalResult, plat platform.Platform, cfg config.Config, boost bool) (string, error) {
	data, _ := platform.MaybeGunzip(result.body)
	accept := r.Header.Get("Accept")
	ct := indexContentType(plat, result.contentType, accept, data)
	rememberIndexDigests(data, result.entry.SourceURL)
	tr := plat.TransformIndex(data, ct, cfg, accept, result.entry.SourceURL)
	if matchETag(r.Header.Get("If-None-Match"), tr.ETag) {
		w.Header().Set("ETag", tr.ETag)
		w.Header().Set("X-Cache", "REVALIDATED")
		w.WriteHeader(http.StatusNotModified)
		return "revalidated", nil
	}
	w.Header().Set("Content-Type", tr.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(tr.Body)))
	w.Header().Set("ETag", tr.ETag)
	w.Header().Set("X-Cache", "REVALIDATED")
	w.Header().Set("X-Mirrorhub-Strategy", "index")
	decorateIndexHeaders(w, plat, tr.Body)
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(tr.Body)
	}
	return "revalidated", nil
}

// serveCachedIndex 从已持锁读出的缓存正文响应索引（ETag/304、URL 改写）。
// cacheLabel 一般为 HIT 或 STALE（过期回退）。
func (s *Server) serveCachedIndex(w http.ResponseWriter, r *http.Request, entry *cache.Entry, data []byte, plat platform.Platform, cfg config.Config, boost bool, cacheLabel string) (string, error) {
	if cacheLabel == "" {
		cacheLabel = "HIT"
	}
	if entry == nil || len(data) == 0 {
		err := fmt.Errorf("empty cached index")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return "miss", err
	}
	data, _ = platform.MaybeGunzip(data)
	accept := r.Header.Get("Accept")
	ct := indexContentType(plat, entry.ContentType, accept, data)
	rememberIndexDigests(data, entry.SourceURL)
	tr := plat.TransformIndex(data, ct, cfg, accept, entry.SourceURL)
	label := strings.ToLower(cacheLabel)
	if label == "" {
		label = "hit"
	}
	if matchETag(r.Header.Get("If-None-Match"), tr.ETag) {
		w.Header().Set("ETag", tr.ETag)
		w.Header().Set("X-Cache", cacheLabel)
		w.WriteHeader(http.StatusNotModified)
		return label, nil
	}
	w.Header().Set("Content-Type", tr.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(tr.Body)))
	w.Header().Set("ETag", tr.ETag)
	w.Header().Set("X-Cache", cacheLabel)
	w.Header().Set("X-Mirrorhub-Strategy", "index")
	decorateIndexHeaders(w, plat, tr.Body)
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(tr.Body)
	}
	return label, nil
}

func indexContentType(plat platform.Platform, upstreamCT, accept string, body []byte) string {
	if det, ok := plat.(platform.ContentTypeDetector); ok {
		return det.DetectIndexContentType(upstreamCT, accept, body)
	}
	return platform.DetectContentType(upstreamCT, body)
}

func decorateIndexHeaders(w http.ResponseWriter, plat platform.Platform, body []byte) {
	if d, ok := plat.(platform.IndexHeaderDecorator); ok {
		d.DecorateIndexHeaders(w, body)
	}
}

func statusSuggestsAuth(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized")
}
