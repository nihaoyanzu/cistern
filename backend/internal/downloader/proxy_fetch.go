package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/platform"
)

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
		if entry, data, ok := e.cache.ReadAll(cacheKey, platName); ok {
			return data, entry.ContentType, nil
		}
	}
	status, respHeader, body, err := e.ProxyBytes(ctx, http.MethodGet, indexURL, nil, nil, platName, prefetch)
	if err != nil || status >= 400 {
		if ttlSeconds > 0 {
			if entry, data, ok := e.cache.ReadAllStale(cacheKey); ok {
				return data, entry.ContentType, nil
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
		if _, data, ok := e.cache.ReadAll(cacheKey, platName); ok {
			return data, nil
		}
	}
	status, respHeader, body, err := e.ProxyBytes(ctx, http.MethodGet, metaURL, nil, nil, platName, prefetch)
	if err != nil || status < 200 || status >= 300 {
		if ttlSeconds > 0 {
			if _, data, ok := e.cache.ReadAllStale(cacheKey); ok {
				return data, nil
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
