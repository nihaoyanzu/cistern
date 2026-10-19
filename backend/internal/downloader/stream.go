package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/metrics"
	"github.com/nihaoyanzu/cistern/internal/ratelimit"
)

const (
	// 略低于 pip 默认 Read timeout（约 15s），卡顿即取消上游并用 Range 续传
	streamStallTimeout = 10 * time.Second
	streamFlushEvery   = 64 * 1024
	streamMaxResume    = 12
)

const interactiveStreamMaxWall = 2 * time.Hour
const interactiveStreamMaxWallHard = 48 * time.Hour

func interactiveStreamWall(opt Options) time.Duration {
	wall := interactiveStreamMaxWall
	// 大文件按约 1MiB/s 估算超时（再留 2 倍余量），避免 HF GGUF 等超过 2h 被掐断后永不入库
	if opt.HasKnownSize && opt.KnownSize > 1<<30 {
		sec := opt.KnownSize / (1 << 20) * 2
		if sec < 1 {
			sec = 1
		}
		need := time.Duration(sec) * time.Second
		if need > wall {
			wall = need
		}
	}
	if wall > interactiveStreamMaxWallHard {
		wall = interactiveStreamMaxWallHard
	}
	return wall
}

func (e *Engine) streamToClientAndCache(ctx context.Context, w http.ResponseWriter, opt Options) (*cache.Entry, string, int64, error) {
	// 交互传体绝对上限，防止 Timeout=0 僵尸连接
	if !opt.Prefetch {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, interactiveStreamWall(opt))
		defer cancel()
	}
	ctx, hubMeta := WithHubFileMeta(ctx)
	// 下载全程占写租约，Clear 会等到本请求结束且拒绝入库
	if err := e.cache.HoldWrite(); err != nil {
		return nil, "", 0, err
	}
	defer e.cache.ReleaseWrite()
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

	// docker 或带期望摘要：边下边 hash，结束不匹配则掐连接且不入库
	opt.resolveExpectedDigest()
	var streamHash hash.Hash
	if opt.Platform == "docker" || strings.TrimSpace(opt.ExpectedSHA256) != "" {
		streamHash = sha256.New()
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
			cl := ClassOf(err)
			if written > 0 && (cl == ClassTransient || isTransientNetErr(err)) {
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
			return nil, ct, written, wrapClassified(err, opt.OnAuthFail != nil)
		}

		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return nil, ct, written, statusError(resp.StatusCode, resp.Header, opt.OnAuthFail != nil)
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
					return nil, ct, written, &FailError{
						Code: CodeContentChanged, Msg: fmt.Sprintf("upstream size mismatch: head=%d get=%d", knownSize, getCL),
						Class: ClassContentChanged, Cause: errContentChanged,
					}
				}
				size = getCL
			} else if knownSize > 0 {
				size = knownSize
			}
			if et := resp.Header.Get("ETag"); et != "" && opt.IdentityETag == "" {
				opt.IdentityETag = et
			}
		} else if opt.IdentityETag != "" {
			if got := resp.Header.Get("ETag"); got != "" && got != opt.IdentityETag {
				_ = resp.Body.Close()
				return nil, ct, written, &FailError{Code: CodeContentChanged, Msg: "etag changed", Class: ClassContentChanged, Cause: errContentChanged}
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
			w.Header().Set("X-Cistern-Strategy", "stream")
			if opt.Boost {
				w.Header().Set("X-Cistern-Boost", "1")
			}
			// HF：把重定向链上捕获的 X-Repo-Commit 等写回客户端
			ApplyHubFileMetaHeaders(w, hubMeta, resp.Header)
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
		n, copyErr := e.copyStreamToClientAndFile(ctx, w, f, body, pl, opt.Prefetch, idle, &lastProgress, streamHash)
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
			if errors.Is(copyErr, errUpstreamStall) {
				e.gov.ObserveStall(hostKey(opt.URL))
			}
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
		return nil, ct, written, wrapClassified(copyErr, opt.OnAuthFail != nil)
	}

	if !completed {
		e.gov.ObserveStall(hostKey(opt.URL))
		err := fail(CodeStalled, fmt.Sprintf("incomplete download: got %d after %d resume attempts", written, streamMaxResume), ClassTransient, errUpstreamStall)
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
	digestTrusted := false
	if streamHash != nil && strings.TrimSpace(opt.ExpectedSHA256) != "" {
		sum := hex.EncodeToString(streamHash.Sum(nil))
		exp := strings.ToLower(strings.TrimSpace(opt.ExpectedSHA256))
		if len(exp) == 64 && !strings.EqualFold(sum, exp) {
			err := fmt.Errorf("digest mismatch: want %s got %s", exp, sum)
			if headerWritten {
				abortHTTPBody(w)
			}
			_ = os.Remove(tmp)
			cleanupTmp = true
			return nil, ct, written, err
		}
		if len(exp) == 64 {
			digest = strings.ToLower(sum)
			RememberDigest(opt.metaSource(), digest)
			digestTrusted = true // 边下边 hash 已校验，勿再整文件重读
		}
	}
	// 流式路径已用 streamHash 校验过时跳过 verifyAndRemember，避免大文件客户端下完后还卡数十分钟才 Put
	if !digestTrusted {
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
				if headerWritten {
					abortHTTPBody(w)
				}
				_ = os.Remove(tmp)
				cleanupTmp = true
				return nil, ct, written, err
			}
		} else if strings.TrimSpace(digest) != "" {
			digestTrusted = true
		}
	}

	repoCommit, _, _ := hubMeta.snapshot()
	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL:     opt.metaSource(),
		Kind:          opt.metaKind(),
		Digest:        digest,
		DigestTrusted: digestTrusted,
		RepoCommit:    repoCommit,
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
	hasher hash.Hash,
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
			if hasher != nil {
				_, _ = hasher.Write(buf[:n])
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
