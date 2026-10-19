package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/config"
	"github.com/nihaoyanzu/cistern/internal/downloader"
	pypihandler "github.com/nihaoyanzu/cistern/internal/handlers/pypi"
	"github.com/nihaoyanzu/cistern/internal/platform"
	"github.com/nihaoyanzu/cistern/internal/router"
)

// looksLikeNonArtifactBlobType 粗判 Docker blob 不应出现的类型（错误 JSON / manifest）。
func looksLikeNonArtifactBlobType(ct string) bool {
	low := strings.ToLower(strings.TrimSpace(ct))
	if low == "" {
		return false
	}
	if strings.Contains(low, "text/html") {
		return true
	}
	if strings.Contains(low, "json") &&
		(strings.Contains(low, "manifest") || strings.Contains(low, "error") ||
			strings.Contains(low, "vnd.docker.distribution.manifest") ||
			strings.Contains(low, "vnd.oci.image")) {
		return true
	}
	return false
}

func (s *Server) handlePackage(w http.ResponseWriter, r *http.Request, m *router.Match, plat platform.Platform, cfg config.Config, pypi config.PlatformConfig, prefetch, boost bool, onAcquired, onHit, onWaiting func(), onProgress func(done, total int64), taskID string) (string, string, error) {
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

	// 鉴权+HEAD+占槽准备阶段墙钟上限 60s
	prepareCtx, prepareCancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer prepareCancel()

	injectAuthCtx := func(ctx context.Context) error {
		auther, ok := plat.(platform.UpstreamAuther)
		if !ok {
			return nil
		}
		return auther.InjectUpstreamAuth(ctx, headers, cfg, r.URL.Path)
	}
	injectAuth := func() error { return injectAuthCtx(prepareCtx) }
	invalidateAuth := func(challenge http.Header) {
		if inv, ok := plat.(platform.UpstreamAuthInvalidator); ok {
			inv.InvalidateUpstreamAuth(cfg, r.URL.Path, challenge)
		}
	}

	if err := injectAuth(); err != nil {
		if entry, f, ok := s.cache.Open(cacheKey, m.Platform); ok {
			if m.Platform == "huggingface" && strings.TrimSpace(entry.RepoCommit) == "" {
				s.backfillHFRepoCommit(prepareCtx, entry, origURL, headers)
			}
			return s.servePackageHit(w, r, entry, f, boost, onHit, onProgress)
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return "miss", "proxy", err
	}

	if entry, f, ok := s.cache.Open(cacheKey, m.Platform); ok {
		if entry.Size <= 0 {
			f.Close()
			_ = s.cache.Delete(cacheKey)
		} else if m.Platform == "huggingface" && strings.TrimSpace(entry.RepoCommit) == "" {
			// 旧缓存无 Hub commit：先 HEAD 回填；仍失败则关 fd 走上游（文件保留），避免 hf 拒收无头 HIT
			s.backfillHFRepoCommit(prepareCtx, entry, origURL, headers)
			if strings.TrimSpace(entry.RepoCommit) != "" {
				return s.servePackageHit(w, r, entry, f, boost, onHit, onProgress)
			}
			f.Close()
		} else {
			return s.servePackageHit(w, r, entry, f, boost, onHit, onProgress)
		}
	}

	headProbe := func() (int64, string, string, *downloader.HubFileMeta, error) {
		headCtx, headCancel := context.WithTimeout(prepareCtx, 15*time.Second)
		defer headCancel()
		headCtx, meta := downloader.WithHubFileMeta(headCtx)
		size, ct, u, err := s.dl.Head(headCtx, origURL, headers)
		return size, ct, u, meta, err
	}
	// 上游已拿到 commit 时顺带补写本地缺头条目，下次 HIT 即可带 X-Repo-Commit
	persistHFRepoCommit := func(hubMeta *downloader.HubFileMeta) {
		if m.Platform != "huggingface" || hubMeta == nil {
			return
		}
		commit := hubMeta.GetRepoCommit()
		if commit == "" {
			return
		}
		_ = s.cache.SetRepoCommit(cacheKey, commit)
	}
	isAuthFail := func(err error) bool {
		if err == nil {
			return false
		}
		msg := strings.ToLower(err.Error())
		return strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
			strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden")
	}

	// HEAD 请求直接探测上游文件大小，绕过 task semaphore 避免被暂停的 prefetch 任务阻塞。
	if r.Method == http.MethodHead {
		size, ct, _, hubMeta, headErr := headProbe()
		if headErr != nil && isAuthFail(headErr) {
			invalidateAuth(downloader.AuthChallengeOf(headErr))
			if err := injectAuth(); err == nil {
				size, ct, _, hubMeta, headErr = headProbe()
			}
		}
		if headErr != nil {
			http.Error(w, headErr.Error(), http.StatusBadGateway)
			return "miss", "proxy", headErr
		}
		if size <= 0 {
			err := fmt.Errorf("upstream content size of zero")
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "proxy", err
		}
		if pypihandler.IsHTMLContentType(ct) {
			err := fmt.Errorf("upstream returned HTML for package path (not an artifact)")
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "proxy", err
		}
		if m.Platform == "docker" && looksLikeNonArtifactBlobType(ct) {
			err := fmt.Errorf("upstream returned non-artifact content-type for docker blob: %s", ct)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "proxy", err
		}
		if ct == "" {
			ct = "application/octet-stream"
		}
		persistHFRepoCommit(hubMeta)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Cistern-Strategy", "proxy")
		downloader.ApplyHubFileMetaHeaders(w, hubMeta, nil)
		if d, ok := plat.(platform.PackageHeaderDecorator); ok {
			d.DecoratePackageHeaders(w)
		}
		if boost {
			w.Header().Set("X-Cistern-Boost", "1")
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
	var headHubMeta *downloader.HubFileMeta
	// SkipUpstreamHead：交互冷路径跳过上游 HEAD；Docker blob / HF 等需 CL 校验的不设此标志
	skipHead := m.SkipUpstreamHead && !prefetch
	// 真正占槽下载时由 OnAcquired→MarkRunning
	if !skipHead {
		hs, headCT, finalURL, hubMeta, headErr := headProbe()
		if headErr != nil && isAuthFail(headErr) {
			invalidateAuth(downloader.AuthChallengeOf(headErr))
			if err := injectAuth(); err == nil {
				hs, headCT, finalURL, hubMeta, headErr = headProbe()
			}
		}
		if headErr != nil {
			if m.Platform == "docker" {
				http.Error(w, headErr.Error(), http.StatusBadGateway)
				return "miss", "proxy", headErr
			}
		} else if hs > 0 {
			if pypihandler.IsHTMLContentType(headCT) {
				err := fmt.Errorf("upstream returned HTML for package path (not an artifact)")
				http.Error(w, err.Error(), http.StatusBadGateway)
				return "miss", "proxy", err
			}
			if m.Platform == "docker" && looksLikeNonArtifactBlobType(headCT) {
				err := fmt.Errorf("upstream returned non-artifact content-type for docker blob: %s", headCT)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return "miss", "proxy", err
			}
			size = hs
			hasSize = true
			headHubMeta = hubMeta
			persistHFRepoCommit(hubMeta)
			if finalURL != "" {
				fetchURL = finalURL
			}
		} else if m.Platform == "docker" {
			err := fmt.Errorf("upstream content size of zero")
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "proxy", err
		}
	}
	// HEAD 已跟随到 CDN 时，stream/GET 不会再经过 Hub；把捕获的 X-Repo-Commit 注入下载 ctx
	serveCtx := r.Context()
	if headHubMeta != nil {
		var seeded *downloader.HubFileMeta
		serveCtx, seeded = downloader.WithHubFileMeta(serveCtx)
		seeded.CopyFrom(headHubMeta)
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
		OnHit:          onHit,
		OnWaiting:      onWaiting,
		OnProgress:     onProgress,
		OnAuthFail: func(actx context.Context, h http.Header, challenge http.Header) error {
			invalidateAuth(challenge)
			headers.Del("Authorization")
			if err := injectAuthCtx(actx); err != nil {
				return err
			}
			// 把最新 Authorization 拷回调用方 header map
			for k, vals := range headers {
				h[k] = append([]string(nil), vals...)
			}
			return nil
		},
	}

	writeUpstreamErr := func(err error, wrote bool) {
		if wrote {
			return
		}
		code := http.StatusBadGateway
		switch downloader.ErrorCode(err) {
		case downloader.CodeAuthRequired, downloader.CodeAuthFailed:
			code = http.StatusUnauthorized
		case downloader.CodeForbidden:
			code = http.StatusForbidden
		case downloader.CodeNotFound:
			code = http.StatusNotFound
		}
		if isAuthFail(err) && code == http.StatusBadGateway {
			if strings.Contains(strings.ToLower(err.Error()), "403") || strings.Contains(strings.ToLower(err.Error()), "forbidden") {
				code = http.StatusForbidden
			} else {
				code = http.StatusUnauthorized
			}
		}
		http.Error(w, err.Error(), code)
	}

	if !prefetch {
		tw := &writeTrack{ResponseWriter: w}
		label, err := s.dl.ServePackage(serveCtx, tw, opt)
		// 写头前 401/403：换票后重试一次 ServePackage（OnAuthFail 也会在引擎内触发）
		if err != nil && !tw.wrote && isAuthFail(err) {
			invalidateAuth(downloader.AuthChallengeOf(err))
			headers.Del("Authorization")
			if injErr := injectAuth(); injErr == nil {
				opt.Headers = headers
				tw2 := &writeTrack{ResponseWriter: w}
				label, err = s.dl.ServePackage(serveCtx, tw2, opt)
				tw = tw2
			}
		}
		strat := "stream"
		if label == "hit" {
			strat = "cache"
		}
		if err != nil {
			writeUpstreamErr(err, tw.wrote)
		}
		return label, strat, err
	}

	if err := s.cache.AllowPrefetchWrite(); err != nil {
		http.Error(w, err.Error(), http.StatusInsufficientStorage)
		return "miss", "parallel", err
	}

	res, err := s.dl.GetOrDownload(serveCtx, opt)
	if err != nil {
		writeUpstreamErr(err, false)
		return "miss", "parallel", err
	}
	label, _, err := s.serveDownloadResultFull(w, res, fileBoost)
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

// backfillHFRepoCommit 对缺 X-Repo-Commit 的本地条目向上游 HEAD 补头并落盘。
func (s *Server) backfillHFRepoCommit(ctx context.Context, entry *cache.Entry, rawURL string, headers http.Header) {
	if entry == nil || strings.TrimSpace(entry.RepoCommit) != "" {
		return
	}
	if strings.TrimSpace(rawURL) == "" || s.dl == nil {
		return
	}
	headCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	headCtx, meta := downloader.WithHubFileMeta(headCtx)
	// CDN 超时等错误仍可能已在 Hub 302 上捕获到 X-Repo-Commit
	_, _, _, _ = s.dl.Head(headCtx, rawURL, headers)
	if meta == nil {
		return
	}
	commit := meta.GetRepoCommit()
	if commit == "" {
		return
	}
	if err := s.cache.SetRepoCommit(entry.Key, commit); err == nil {
		entry.RepoCommit = commit
	}
}

// servePackageHit 回本地 HIT。f 须来自 cache.Open，本函数负责 Close。
func (s *Server) servePackageHit(w http.ResponseWriter, r *http.Request, entry *cache.Entry, f *os.File, boost bool, onHit func(), onProgress func(done, total int64)) (string, string, error) {
	if f != nil {
		defer f.Close()
	}
	if entry != nil && entry.Size <= 0 {
		_ = s.cache.Delete(entry.Key)
		err := fmt.Errorf("cached entry has zero size")
		http.Error(w, err.Error(), http.StatusBadGateway)
		return "miss", "cache", err
	}
	if onHit != nil {
		onHit()
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
			if entry.RepoCommit != "" {
				w.Header().Set("X-Repo-Commit", entry.RepoCommit)
			}
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
		if entry.RepoCommit != "" {
			w.Header().Set("X-Repo-Commit", entry.RepoCommit)
		}
		w.WriteHeader(http.StatusOK)
		return "hit", "cache", nil
	}
	// GET 命中：直接回已 Open 的本地 fd，禁止再打上游 HEAD
	if r.Method == http.MethodGet {
		if f == nil {
			err := fmt.Errorf("cache hit missing file handle")
			http.Error(w, err.Error(), http.StatusBadGateway)
			return "miss", "cache", err
		}
		tw := &writeTrack{ResponseWriter: w}
		label, err := s.dl.ServeCachedFile(tw, entry, f, boost, r.Header.Get("Range"), "HIT")
		if err != nil && !tw.wrote {
			_ = s.cache.Delete(entry.Key)
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return label, "cache", err
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return "miss", "cache", fmt.Errorf("method not allowed")
}
