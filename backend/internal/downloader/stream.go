package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/metrics"
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/traffic"
)

const (
	// 略低于 pip 默认 Read timeout（约 15s），卡顿即取消上游并用 Range 续传
	streamStallTimeout = 10 * time.Second
	streamFlushEvery   = 64 * 1024
	streamMaxResume    = 12
)

var errUpstreamStall = errors.New("upstream read stalled")

func isHTMLContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "text/html")
}

// looksLikeDockerNonBlobJSON 粗判 registry 错误页或 manifest（不应作为 layer blob）。
func looksLikeDockerNonBlobJSON(ct string) bool {
	low := strings.ToLower(strings.TrimSpace(ct))
	if low == "" || !strings.Contains(low, "json") {
		return false
	}
	return strings.Contains(low, "manifest") ||
		strings.Contains(low, "vnd.docker.distribution.manifest") ||
		strings.Contains(low, "vnd.oci.image") ||
		strings.Contains(low, "error")
}

// parseLinkedSize 解析 HuggingFace X-Linked-Size 头；无效返回 -1。
func parseLinkedSize(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// ServePackage 交互下载：命中缓存直接回文件；未命中则流式/并行流式并写入缓存。
func (e *Engine) ServePackage(ctx context.Context, w http.ResponseWriter, opt Options) (cacheLabel string, err error) {
	opt.resolveExpectedDigest()
		if entry, ok := e.cache.Get(opt.CacheKey, opt.Platform); ok {
			notifyAcquired(opt)
			return serveCached(w, entry, opt.Boost, opt.RangeHeader, "HIT")
		}

		for try := 0; try < 3; try++ {
			label, done, err := e.servePackageOnce(ctx, w, opt)
			if done {
				return label, err
			}
		}
		return "miss", fmt.Errorf("unable to acquire download slot")
	}

	// ServeCachedEntry 直接回已查到的缓存条目（避免二次 Get）。cacheLabel 空则 HIT。
	func (e *Engine) ServeCachedEntry(w http.ResponseWriter, entry *cache.Entry, boost bool, rangeHeader, cacheLabel string) (string, error) {
		return serveCached(w, entry, boost, rangeHeader, cacheLabel)
	}

	func (e *Engine) servePackageOnce(ctx context.Context, w http.ResponseWriter, opt Options) (label string, done bool, err error) {
		wait := &inflightWait{done: make(chan struct{}), mode: inflightModeStream}
		actual, loaded := e.inflight.LoadOrStore(opt.CacheKey, wait)
		if loaded {
			sw := actual.(*inflightWait)
			if sw.mode == inflightModeFetch && sw.cancel != nil {
				sw.cancel()
			}
			if opt.OnWaiting != nil {
				opt.OnWaiting()
			}
			select {
			case <-ctx.Done():
				return "na", true, ctx.Err()
			case <-sw.done:
			}
			if entry, ok := e.cache.Get(opt.CacheKey, opt.Platform); ok {
				notifyAcquired(opt)
				l, e2 := serveCached(w, entry, opt.Boost, opt.RangeHeader, "HIT")
				return l, true, e2
			}
			if sw.err == nil && sw.entry != nil {
				notifyAcquired(opt)
				l, e2 := serveCached(w, sw.entry, opt.Boost, opt.RangeHeader, "HIT")
				return l, true, e2
			}
			return "", false, nil // 重试占坑
		}

	defer func() {
		e.inflight.Delete(opt.CacheKey)
		close(wait.done)
	}()

	var entry *cache.Entry
	// 交互式下载始终使用串行流式（streamToClientAndCache）：
	// - 有卡顿检测（streamStallTimeout=10s）和自动 Range 续传
	// - 单连接下载，不受并发分片带宽分配影响
	// - 对慢上游镜像更可靠，避免 pip ReadTimeout
	// 预取下载使用 GetOrDownload → download（并行分片），不经过此路径。
	entry, _, _, err = e.streamToClientAndCache(ctx, w, opt)
	wait.err = err
	wait.entry = entry
	if entry != nil {
		wait.ct = entry.ContentType
		wait.size = entry.Size
	}
	if err != nil {
		return "miss", true, err
	}
	return "miss", true, nil
}

func serveCached(w http.ResponseWriter, entry *cache.Entry, boost bool, rangeHeader, cacheLabel string) (string, error) {
	if cacheLabel == "" {
		cacheLabel = "HIT"
	}
	accessLabel := strings.ToLower(cacheLabel)
	if accessLabel == "" {
		accessLabel = "hit"
	}
	f, err := os.Open(entry.FilePath)
	if err != nil {
		return "miss", err
	}
	defer f.Close()
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	etag := entry.ETag
	if etag == "" && entry.Digest != "" {
		etag = `"` + entry.Digest + `"`
	}
	if etag == "" {
		etag = fmt.Sprintf(`W/"%d-%d"`, entry.Size, entry.CreatedAt.Unix())
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Cache", cacheLabel)
	w.Header().Set("X-Mirrorhub-Strategy", "cache")
	if boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}

	start, end, ok, err := parseSingleRange(rangeHeader, entry.Size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		// 已向客户端完成 416，不记为任务失败
		return accessLabel, nil
	}
	if ok {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return accessLabel, err
		}
		length := end - start + 1
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, entry.Size))
		w.WriteHeader(http.StatusPartialContent)
		_, err = copyFlushN(w, f, length)
		return accessLabel, err
	}

	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	_, err = copyFlush(w, f)
	return accessLabel, err
}

// parseSingleRange 解析单个 bytes=start-end；多区间返回 error→416；无 Range 返回 ok=false。
func parseSingleRange(header string, size int64) (start, end int64, ok bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" || size <= 0 {
		return 0, 0, false, nil
	}
	if !strings.HasPrefix(strings.ToLower(header), "bytes=") {
		return 0, 0, false, fmt.Errorf("unsupported range unit")
	}
	spec := strings.TrimSpace(header[len("bytes="):])
	if strings.Contains(spec, ",") {
		return 0, 0, false, fmt.Errorf("multiple ranges not supported")
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		// suffix: bytes=-N
		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false, fmt.Errorf("invalid suffix range")
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	start, e1 := strconv.ParseInt(parts[0], 10, 64)
	if e1 != nil || start < 0 {
		return 0, 0, false, fmt.Errorf("invalid range start")
	}
	if parts[1] == "" {
		end = size - 1
	} else {
		end, e1 = strconv.ParseInt(parts[1], 10, 64)
		if e1 != nil || end < start {
			return 0, 0, false, fmt.Errorf("invalid range end")
		}
	}
	if start >= size {
		return 0, 0, false, fmt.Errorf("range not satisfiable")
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true, nil
}

func copyFlushN(w http.ResponseWriter, src io.Reader, n int64) (int64, error) {
	return copyFlush(w, io.LimitReader(src, n))
}

func copyFlush(w http.ResponseWriter, src io.Reader) (int64, error) {
	buf := make([]byte, 256*1024)
	var written int64
	var since int
	flusher, canFlush := w.(http.Flusher)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			since += n
			if canFlush && since >= streamFlushEvery {
				flusher.Flush()
				since = 0
			}
		}
		if readErr == io.EOF {
			flushWriter(w)
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

const interactiveStreamMaxWall = 2 * time.Hour

func (e *Engine) streamToClientAndCache(ctx context.Context, w http.ResponseWriter, opt Options) (*cache.Entry, string, int64, error) {
	// 交互传体绝对上限，防止 Timeout=0 僵尸连接
	if !opt.Prefetch {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, interactiveStreamMaxWall)
		defer cancel()
	}
	pl := e.limiters.Get(opt.Platform)
	if pl == nil {
		return nil, "", 0, fmt.Errorf("no rate limiter for platform %s", opt.Platform)
	}
	if err := pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch); err != nil {
		return nil, "", 0, err
	}
	defer pl.ReleaseTask(opt.Boost)
	notifyAcquired(opt)
	if err := pl.AcquireConn(ctx, !opt.Prefetch); err != nil {
		return nil, "", 0, err
	}
	defer pl.ReleaseConn()

	tmp := e.cache.TempPath(opt.CacheKey + ".stream")
	_ = os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		return nil, "", 0, err
	}
	cleanupTmp := true
	defer func() {
		_ = f.Close()
		if cleanupTmp {
			_ = os.Remove(tmp)
		}
	}()

	knownSize := int64(-1)
	if opt.HasKnownSize && opt.KnownSize > 0 {
		knownSize = opt.KnownSize
	}
	size := knownSize
	ct := opt.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}

	headerWritten := false
	var written int64

	idle := 0.3
	if e.idleRatio != nil {
		idle = e.idleRatio()
	}

	completed := false
	for attempt := 0; attempt < streamMaxResume; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, ct, written, err
		}

		headers := cloneHeader(opt.Headers)
		if written > 0 {
			headers.Set("Range", fmt.Sprintf("bytes=%d-", written))
		}

		// 建连/等响应头走父 ctx（由 Transport.ResponseHeaderTimeout 约束），
		// stall 看门狗只盯 body 读取，避免把慢握手误判成可续传取消。
		resp, err := e.doGET(ctx, http.MethodGet, opt.URL, headers, 3)
		if err != nil {
			if err := ctx.Err(); err != nil {
				return nil, ct, written, err
			}
			if written > 0 && isTransientNetErr(err) {
				if e.log != nil {
					e.log.Warn("stream resume after request error",
						zap.Int64("offset", written),
						zap.Int("attempt", attempt+1),
						zap.Error(err),
					)
				}
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
				metrics.DownloadResumesTotal.Inc()
				continue
			}
			return nil, ct, written, err
		}

		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return nil, ct, written, fmt.Errorf("upstream %s", resp.Status)
		}

		if rh := resp.Header.Get("Content-Type"); rh != "" {
			ct = rh
		}
		if (opt.Kind == "package" || opt.Kind == "") && isHTMLContentType(ct) {
			_ = resp.Body.Close()
			return nil, ct, written, fmt.Errorf("upstream returned HTML, not a package file")
		}
		// Docker blob 不应是 JSON manifest/错误页
		if opt.Platform == "docker" && written == 0 && looksLikeDockerNonBlobJSON(ct) {
			_ = resp.Body.Close()
			return nil, ct, written, fmt.Errorf("upstream returned JSON/manifest for docker blob (not a layer)")
		}
		if written == 0 && !headerWritten {
			getCL := resp.ContentLength
			if getCL <= 0 {
				if linked := parseLinkedSize(resp.Header.Get("X-Linked-Size")); linked > 0 {
					getCL = linked
				}
			}
			if getCL > 0 {
				if knownSize > 0 && getCL != knownSize {
					_ = resp.Body.Close()
					return nil, ct, written, fmt.Errorf("upstream size mismatch: head=%d get=%d", knownSize, getCL)
				}
				size = getCL
			} else if knownSize > 0 {
				size = knownSize
			}
		}
		// 禁止向客户端声明 CL=0（containerd size of zero）
		if !headerWritten && size == 0 {
			_ = resp.Body.Close()
			return nil, ct, written, fmt.Errorf("upstream content size of zero")
		}
		if written > 0 && resp.StatusCode == http.StatusOK {
			if _, skipErr := io.CopyN(io.Discard, resp.Body, written); skipErr != nil {
				_ = resp.Body.Close()
				if err := ctx.Err(); err != nil {
					return nil, ct, written, err
				}
				if isTransientNetErr(skipErr) || errors.Is(skipErr, io.EOF) {
					time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
					metrics.DownloadResumesTotal.Inc()
					continue
				}
				return nil, ct, written, skipErr
			}
		} else if written > 0 && resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, ct, written, fmt.Errorf("unexpected resume status %s", resp.Status)
		}

		if !headerWritten {
			w.Header().Set("Content-Type", ct)
			if size > 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			}
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Mirrorhub-Strategy", "stream")
			if opt.Boost {
				w.Header().Set("X-Mirrorhub-Boost", "1")
			}
			w.WriteHeader(http.StatusOK)
			flushWriter(w)
			headerWritten = true
		}

		reqCtx, reqCancel := context.WithCancel(ctx)
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
		n, copyErr := e.copyStreamToClientAndFile(ctx, w, f, body, pl, opt.Prefetch, idle, &lastProgress)
		_ = resp.Body.Close()
		reqCancel()
		<-watchDone
		written += n

		if copyErr == nil {
			completed = true
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, ct, written, err
		}
		if errors.Is(copyErr, errUpstreamStall) || isTransientNetErr(copyErr) {
			if e.log != nil {
				e.log.Warn("stream resume after stall",
					zap.Int64("offset", written),
					zap.Int("attempt", attempt+1),
					zap.Error(copyErr),
				)
			}
			time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
			metrics.DownloadResumesTotal.Inc()
			continue
		}
		return nil, ct, written, copyErr
	}

	if !completed {
		err := fmt.Errorf("incomplete download: got %d after %d resume attempts", written, streamMaxResume)
		if headerWritten {
			abortHTTPBody(w)
		}
		return nil, ct, written, err
	}
	if size > 0 && written != size {
		err := fmt.Errorf("incomplete download: got %d want %d", written, size)
		if headerWritten {
			abortHTTPBody(w)
		}
		return nil, ct, written, err
	}
	if err := f.Close(); err != nil {
		return nil, ct, written, err
	}
	cleanupTmp = false // 交由 Put 接管路径
	opt.resolveExpectedDigest()
	digest := opt.ExpectedSHA256
	if err := verifyAndRemember(tmp, opt); err != nil {
		// 已向客户端写出正文：非 docker 可降级无摘要入库；docker blob 禁止无 digest HIT
		if headerWritten && opt.Platform != "docker" {
			if e.log != nil {
				e.log.Warn("stream digest verify failed after client write; caching without digest",
					zap.String("url", opt.metaSource()),
					zap.Error(err),
				)
			}
			digest = ""
		} else {
			_ = os.Remove(tmp)
			cleanupTmp = true
			return nil, ct, written, err
		}
	}

	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL: opt.metaSource(),
		Kind:      opt.metaKind(),
		Digest:    digest,
	})
	if err != nil {
		_ = os.Remove(tmp)
		return nil, ct, written, err
	}
	return entry, ct, written, nil
}

// stallAwareBody 在 stall 看门狗取消时关闭上游 body，并把读错误归一成 errUpstreamStall。
type stallAwareBody struct {
	ctx  context.Context
	body io.ReadCloser
}

func (s *stallAwareBody) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, errUpstreamStall
	}
	n, err := s.body.Read(p)
	if err != nil {
		if s.ctx.Err() != nil {
			return n, errUpstreamStall
		}
		return n, err
	}
	return n, nil
}

func (e *Engine) copyStreamToClientAndFile(
	ctx context.Context,
	w http.ResponseWriter,
	f *os.File,
	src io.Reader,
	pl *ratelimit.PlatformLimiter,
	prefetch bool,
	idle float64,
	lastProgress *atomic.Int64,
) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	var sinceFlush int
	flusher, canFlush := w.(http.Flusher)

	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if lastProgress != nil {
				lastProgress.Store(time.Now().UnixNano())
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return written, err
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return written, err
			}
			written += int64(n)
			sinceFlush += n
			if canFlush && sinceFlush >= streamFlushEvery {
				flusher.Flush()
				sinceFlush = 0
			}
			if e.traffic != nil {
				e.traffic.AddDownload(int64(n))
			}
			if prefetch {
				for {
					if !pl.PrefetchPaused() {
						break
					}
					select {
					case <-ctx.Done():
						return written, ctx.Err()
					case <-time.After(200 * time.Millisecond):
					}
				}
				if err := pl.WaitPrefetch(ctx, n, idle); err != nil {
					return written, err
				}
			} else if err := pl.WaitNClass(ctx, n, ratelimit.ClassInteractive); err != nil {
				return written, err
			}
		}
		if readErr == io.EOF {
			flushWriter(w)
			return written, nil
		}
		if readErr != nil {
			// 请求 ctx 被 stall watchdog 取消时，表现为 context.Canceled
			if errors.Is(readErr, context.Canceled) && ctx.Err() == nil {
				return written, errUpstreamStall
			}
			if ctx.Err() == nil && (errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded)) {
				return written, errUpstreamStall
			}
			return written, readErr
		}
	}
}

func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// abortHTTPBody 已写 CL 后无法再发 502：尽量掐断连接，避免客户端无限等剩余字节。
func abortHTTPBody(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil && conn != nil {
			_ = conn.Close()
		}
	}
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return make(http.Header)
	}
	out := make(http.Header, len(h))
	for k, vals := range h {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

type limitReader struct {
	ctx      context.Context
	r        io.Reader
	pl       *ratelimit.PlatformLimiter
	prefetch bool
	idle     float64
	traffic  *traffic.Recorder
	allow    func() bool
	lease    *prefetchLease
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.prefetch && l.pl != nil {
		held := false
		for {
			if !l.pl.PrefetchPaused() && (l.allow == nil || l.allow()) {
				break
			}
			held = true
			if l.lease != nil {
				l.lease.release()
			}
			select {
			case <-l.ctx.Done():
				return 0, l.ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if held {
			if l.lease != nil {
				if err := l.lease.ensureHeld(l.ctx); err != nil {
					return 0, err
				}
			}
			// pause 解除后旧 body 可能已僵死，交给上层换连接重拉
			return 0, errPrefetchReconnect
		}
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if l.prefetch {
			if waitErr := l.pl.WaitPrefetch(l.ctx, n, l.idle); waitErr != nil {
				return n, waitErr
			}
		} else if waitErr := l.pl.WaitNClass(l.ctx, n, ratelimit.ClassInteractive); waitErr != nil {
			return n, waitErr
		}
		if l.traffic != nil {
			l.traffic.AddDownload(int64(n))
		}
	}
	return n, err
}
