package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nihaoyanzu/cistern/internal/cache"
	"github.com/nihaoyanzu/cistern/internal/ratelimit"
)

// downloadSerial 单连接整包下载（上游未提供 Content-Length 时使用）
func (e *Engine) downloadSerial(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, ct, strategy string) (*Result, error) {
	ctx, _ = WithHubFileMeta(ctx)
	tmp := e.cache.TempPath(opt.CacheKey + ".serial")
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if _, err := e.waitPrefetchGate(ctx, pl, opt, lease, nil); err != nil {
				_ = os.Remove(tmp)
				return nil, err
			}
		}
		res, err := e.downloadSerialOnce(ctx, pl, opt, lease, ct, strategy, tmp)
		if err == nil {
			return res, nil
		}
		last = err
		cl := ClassOf(err)
		host := hostKey(opt.URL)
		switch cl {
		case ClassCancel, ClassHard, ClassAuthPermanent, ClassContentChanged:
			_ = os.Remove(tmp)
			return nil, wrapClassified(err, false)
		case ClassAuthRetry:
			if refreshErr := e.tryAuthRefresh(ctx, &opt, AuthChallengeOf(err)); refreshErr != nil {
				_ = os.Remove(tmp)
				return nil, refreshErr
			}
		case ClassRateLimit:
			e.gov.ObserveRateLimit(host)
		case ClassTransient:
			if errors.Is(err, errUpstreamStall) {
				e.gov.ObserveStall(host)
			}
		default:
			if !errors.Is(err, errPrefetchReconnect) && !isTransientNetErr(err) {
				_ = os.Remove(tmp)
				return nil, wrapClassified(err, false)
			}
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(tmp)
			return nil, ctx.Err()
		case <-time.After(time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond):
		}
	}
	_ = os.Remove(tmp)
	return nil, wrapClassified(last, false)
}

func (e *Engine) downloadSerialOnce(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, ct, strategy, tmp string) (*Result, error) {
	if err := pl.AcquireConn(ctx, !opt.Prefetch); err != nil {
		return nil, err
	}
	connHeld := true
	releaseConn := func() {
		if connHeld {
			pl.ReleaseConn()
			connHeld = false
		}
	}
	defer releaseConn()

	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
		if have < 0 {
			have = 0
		}
	}

	headers := cloneHeader(opt.Headers)
	if have > 0 {
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := e.doGET(ctx, http.MethodGet, opt.URL, headers, 3)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 续传：要 206；若上游忽略 Range 回 200，则从头重写
	restart := false
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		// ok
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && have == 0:
		// ok full body
	case resp.StatusCode >= 200 && resp.StatusCode < 300 && have > 0:
		restart = true
		have = 0
	default:
		if resp.StatusCode >= 400 {
			return nil, statusError(resp.StatusCode, resp.Header, opt.OnAuthFail != nil)
		}
		return nil, statusError(resp.StatusCode, resp.Header, opt.OnAuthFail != nil)
	}
	if opt.IdentityETag != "" {
		if got := resp.Header.Get("ETag"); got != "" && got != opt.IdentityETag {
			return nil, &FailError{Code: CodeContentChanged, Msg: "etag changed", Class: ClassContentChanged, Cause: errContentChanged}
		}
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 && opt.HasKnownSize && opt.KnownSize > 0 && total != opt.KnownSize {
		return nil, &FailError{Code: CodeContentChanged, Msg: "content-range total mismatch", Class: ClassContentChanged, Cause: errContentChanged}
	}

	if ct == "" {
		ct = resp.Header.Get("Content-Type")
	}
	if (opt.Kind == "package" || opt.Kind == "") && isHTMLContentType(ct) {
		return nil, fmt.Errorf("upstream returned HTML, not a package file")
	}
	if opt.Platform == "docker" && looksLikeDockerNonBlobJSON(ct) {
		return nil, fmt.Errorf("upstream returned JSON/manifest for docker blob (not a layer)")
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	if restart {
		_ = os.Remove(tmp)
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if have > 0 {
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			return nil, err
		}
	} else if err := f.Truncate(0); err != nil {
		return nil, err
	}

	idle := 0.3
	if e.idleRatio != nil {
		idle = e.idleRatio()
	}

	reqCtx, reqCancel := context.WithCancel(ctx)
	defer reqCancel()
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
	lr := &limitReader{
		ctx:       ctx,
		r:         body,
		pl:        pl,
		prefetch:  opt.Prefetch,
		idle:      idle,
		traffic:   e.traffic,
		allow:     e.prefetchAllow(opt),
		lease:     lease,
		onBlocked: releaseConn,
	}

	total := resp.ContentLength
	if have > 0 && resp.StatusCode == http.StatusPartialContent {
		if total > 0 {
			total = have + total
		}
	}
	if cr := resp.Header.Get("Content-Range"); have > 0 && total <= 0 {
		if n := parseContentRangeTotal(cr); n > 0 {
			total = n
		}
	}
	wantSize := int64(-1)
	if opt.HasKnownSize && opt.KnownSize > 0 {
		wantSize = opt.KnownSize
	}
	if total > 0 {
		if wantSize > 0 && total != wantSize {
			return nil, fmt.Errorf("upstream size mismatch: known=%d get=%d", wantSize, total)
		}
		wantSize = total
	}
	if total > 0 {
		notifyProgress(opt, have, total)
	} else if have > 0 {
		notifyProgress(opt, have, 0)
	}

	done := have
	var doneMu sync.Mutex
	cw := &progressWriter{
		w: f,
		onWrite: func(n int64) {
			lastProgress.Store(time.Now().UnixNano())
			doneMu.Lock()
			done += n
			cur, tot := done, total
			doneMu.Unlock()
			if tot > 0 {
				notifyProgress(opt, cur, tot)
			} else {
				notifyProgress(opt, cur, 0)
			}
		},
	}
	n, err := io.Copy(cw, lr)
	reqCancel()
	<-watchDone
	if err != nil {
		cleanup = false
		return nil, err
	}
	if err := f.Close(); err != nil {
		cleanup = false
		return nil, err
	}
	final := have + n
	if wantSize > 0 && final != wantSize {
		cleanup = false
		return nil, fmt.Errorf("incomplete download: got %d want %d", final, wantSize)
	}
	notifyProgress(opt, final, final)
	opt.resolveExpectedDigest()
	if err := verifyAndRemember(tmp, opt); err != nil {
		return nil, err
	}
	repoCommit, _, _ := HubFileMetaFrom(ctx).snapshot()
	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL:     opt.metaSource(),
		Kind:          opt.metaKind(),
		Digest:        opt.ExpectedSHA256,
		DigestTrusted: strings.TrimSpace(opt.ExpectedSHA256) != "", // verifyAndRemember 已校验
		RepoCommit:    repoCommit,
	})
	if err != nil {
		return nil, err
	}
	cleanup = false
	_ = os.Remove(tmp)
	return &Result{Entry: entry, ContentType: ct, Size: final, FromCache: false, Strategy: strategy}, nil
}
