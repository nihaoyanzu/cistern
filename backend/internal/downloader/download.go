package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/ratelimit"
	"go.uber.org/zap"
)

func (e *Engine) download(ctx context.Context, opt Options) (*Result, error) {
	opt.resolveExpectedDigest()
	if opt.Prefetch {
		if err := e.cache.AllowPrefetchWrite(); err != nil {
			return nil, err
		}
	}
	if err := e.cache.HoldWrite(); err != nil {
		return nil, err
	}
	defer e.cache.ReleaseWrite()
	pl := e.limiters.Get(opt.Platform)
	if pl == nil {
		return nil, fmt.Errorf("no rate limiter for platform %s", opt.Platform)
	}
	if err := e.acquireTaskSlot(ctx, pl, opt); err != nil {
		return nil, err
	}
	lease := newPrefetchLease(e, pl, opt)
	defer lease.close()
	notifyAcquired(opt)

	contentRetries := 0
	for {
		res, err := e.downloadOnce(ctx, pl, opt, lease, contentRetries > 0)
		if err == nil {
			return res, nil
		}
		c := ClassOf(err)
		if c == ClassContentChanged && contentRetries < 1 {
			contentRetries++
			e.wipePartial(opt)
			if e.log != nil {
				e.log.Warn("content changed; full redownload",
					zap.String("url", opt.URL),
					zap.Error(err),
				)
			}
			continue
		}
		return res, wrapClassified(err, false)
	}
}

func (e *Engine) wipePartial(opt Options) {
	_ = e.cache.ClearChunks(opt.URL)
	_ = os.Remove(e.cache.TempPath(opt.CacheKey))
	_ = os.Remove(e.cache.TempPath(opt.CacheKey + ".serial"))
}

func (e *Engine) downloadOnce(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, afterWipe bool) (*Result, error) {
	size := int64(-1)
	ct := opt.ContentType
	if opt.HasKnownSize && !afterWipe {
		size = opt.KnownSize
	} else {
		hs, hct, finalURL, etag, err := e.headWithETag(ctx, opt)
		if err != nil {
			authCan := opt.OnAuthFail != nil
			cl := classifyUpstreamAuth(err, 0, authCan)
			if cl.Class == ClassAuthRetry {
				if refreshErr := e.tryAuthRefresh(ctx, &opt); refreshErr != nil {
					return nil, fail(CodeAuthFailed, refreshErr.Error(), ClassAuthPermanent, refreshErr)
				}
				hs, hct, finalURL, etag, err = e.headWithETag(ctx, opt)
			}
			if err != nil {
				return e.downloadSerial(ctx, pl, opt, lease, ct, "serial")
			}
			_ = etag
		}
		size = hs
		if hct != "" {
			ct = hct
		}
		if finalURL != "" {
			opt.URL = finalURL
		}
		if etag != "" {
			opt.IdentityETag = etag
		}
	}
	if size < 0 {
		return e.downloadSerial(ctx, pl, opt, lease, ct, "serial")
	}

	host := hostKey(opt.URL)
	ceiling := opt.Concurrency
	if ceiling <= 0 {
		ceiling = 8
	}

	tmp := e.cache.TempPath(opt.CacheKey)
	resume := false
	if !afterWipe {
		if st, err := os.Stat(tmp); err == nil && st.Size() == size {
			resume = true
		} else {
			_ = os.Remove(tmp)
		}
	} else {
		_ = os.Remove(tmp)
	}

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if !resume {
		if err := f.Truncate(size); err != nil {
			f.Close()
			return nil, err
		}
	}

	chunkSize := opt.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 5 * 1024 * 1024
	}

	type chunk struct{ start, end int64 }
	var chunks []chunk
	for start := int64(0); start < size; start += chunkSize {
		end := start + chunkSize - 1
		if end >= size {
			end = size - 1
		}
		chunks = append(chunks, chunk{start, end})
	}

	doneSet := map[[2]int64]bool{}
	var already int64
	if resume {
		for _, r := range e.cache.GetChunks(opt.URL, size, opt.ChunkTTLHours) {
			doneSet[r] = true
			already += r[1] - r[0] + 1
		}
	}
	var downloaded atomic.Int64
	downloaded.Store(already)
	notifyProgress(opt, already, size)
	onBytes := func(n int64) {
		if n <= 0 {
			return
		}
		notifyProgress(opt, downloaded.Add(n), size)
	}

	jobs := make(chan chunk, len(chunks))
	for _, ch := range chunks {
		if doneSet[[2]int64{ch.start, ch.end}] {
			continue
		}
		jobs <- ch
	}
	close(jobs)

	var (
		mu                sync.Mutex
		authUsed          bool
		fileErr           error
		active            int
		wake              = make(chan struct{}, 1)
		wg                sync.WaitGroup
		jobCtx, jobCancel = context.WithCancel(ctx)
	)
	defer jobCancel()

	signalWake := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if fileErr == nil {
			fileErr = err
			jobCancel()
		}
		mu.Unlock()
		signalWake()
	}

	tryAuth := func() error {
		mu.Lock()
		if authUsed || opt.OnAuthFail == nil {
			mu.Unlock()
			return fail(CodeAuthFailed, "auth refresh unavailable", ClassAuthPermanent, errUpstreamAuth)
		}
		authUsed = true
		mu.Unlock()
		return e.tryAuthRefresh(jobCtx, &opt)
	}

	runChunk := func(ch chunk) {
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
			signalWake()
		}()
		if _, err := e.waitPrefetchGate(jobCtx, pl, opt, lease, nil); err != nil {
			setErr(err)
			return
		}
		if err := pl.AcquireConn(jobCtx, !opt.Prefetch); err != nil {
			setErr(err)
			return
		}
		connHeld := true
		releaseConn := func() {
			if connHeld {
				pl.ReleaseConn()
				connHeld = false
			}
		}
		defer releaseConn()
		ensureConn := func() error {
			if connHeld {
				return nil
			}
			if err := pl.AcquireConn(jobCtx, !opt.Prefetch); err != nil {
				return err
			}
			connHeld = true
			return nil
		}

		cur := ch.start
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			if cur > ch.end {
				break
			}
			mu.Lock()
			fatal := fileErr
			mu.Unlock()
			if fatal != nil || jobCtx.Err() != nil {
				return
			}
			if attempt > 0 {
				if _, err := e.waitPrefetchGate(jobCtx, pl, opt, lease, releaseConn); err != nil {
					setErr(err)
					return
				}
				if err := ensureConn(); err != nil {
					setErr(err)
					return
				}
			}
			mu.Lock()
			canAuth := !authUsed && opt.OnAuthFail != nil
			mu.Unlock()
			var got int64
			err := e.fetchChunk(jobCtx, pl, opt, lease, f, cur, ch.end, size, opt.IdentityETag, canAuth, releaseConn, func(n int64) {
				got += n
				onBytes(n)
			})
			if err == nil {
				_ = e.cache.MarkChunks(opt.URL, size, [][2]int64{{ch.start, ch.end}})
				e.gov.ObserveSuccess(host)
				return
			}
			cur += got
			lastErr = err
			cl := ClassOf(err)
			switch cl {
			case ClassCancel:
				setErr(err)
				return
			case ClassHard, ClassAuthPermanent:
				setErr(wrapClassified(err, false))
				return
			case ClassContentChanged:
				setErr(wrapClassified(err, false))
				return
			case ClassAuthRetry:
				if refreshErr := tryAuth(); refreshErr != nil {
					setErr(refreshErr)
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			case ClassRateLimit:
				e.gov.ObserveRateLimit(host)
				signalWake()
				var fe *FailError
				wait := time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond
				if errors.As(err, &fe) && fe.Retry > 0 {
					wait = fe.Retry
				}
				time.Sleep(wait)
				continue
			case ClassTransient:
				if errors.Is(err, errUpstreamStall) {
					e.gov.ObserveStall(host)
					signalWake()
				}
				time.Sleep(time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond)
				continue
			default:
				if errors.Is(err, errPrefetchReconnect) || isTransientNetErr(err) {
					time.Sleep(time.Duration(1<<min(attempt, 3)) * 200 * time.Millisecond)
					continue
				}
				setErr(wrapClassified(err, false))
				return
			}
		}
		if lastErr != nil {
			setErr(wrapClassified(lastErr, false))
		}
	}

	// 调度循环：按 HostGovernor.effective 投放工人，从 1 爬升
	var pending []chunk
	for ch := range jobs {
		pending = append(pending, ch)
	}
	for len(pending) > 0 {
		mu.Lock()
		fatal := fileErr
		act := active
		mu.Unlock()
		if fatal != nil {
			break
		}
		if jobCtx.Err() != nil {
			break
		}
		limit := e.gov.Limit(host, ceiling)
		for act < limit && len(pending) > 0 {
			ch := pending[0]
			pending = pending[1:]
			mu.Lock()
			active++
			act = active
			mu.Unlock()
			wg.Add(1)
			go func(ch chunk) {
				defer wg.Done()
				runChunk(ch)
			}(ch)
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-jobCtx.Done():
			pending = nil
		case <-wake:
		case <-time.After(200 * time.Millisecond):
		}
	}
	wg.Wait()
	_ = f.Close()

	mu.Lock()
	err = fileErr
	mu.Unlock()
	if err != nil {
		// 并发已降到 1 仍失败：可恢复错误改串行兜底
		c := ClassOf(err)
		if e.gov.Limit(host, ceiling) <= 1 && (c == ClassTransient || c == ClassRateLimit) {
			if e.log != nil {
				e.log.Warn("parallel exhausted at concurrency 1; serial fallback",
					zap.String("url", opt.URL),
					zap.Error(err),
				)
			}
			return e.downloadSerial(ctx, pl, opt, lease, ct, "serial")
		}
		return nil, err
	}

	notifyProgress(opt, size, size)
	opt.resolveExpectedDigest()
	if err := verifyAndRemember(tmp, opt); err != nil {
		return nil, &FailError{Code: CodeDigestMismatch, Msg: err.Error(), Class: ClassContentChanged, Cause: errDigestMismatch}
	}

	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL:    opt.metaSource(),
		Kind:         opt.metaKind(),
		Digest:       opt.ExpectedSHA256,
		UpstreamETag: opt.IdentityETag,
	})
	if err != nil {
		return nil, err
	}
	_ = e.cache.ClearChunks(opt.URL)
	_ = os.Remove(tmp)
	return &Result{Entry: entry, ContentType: ct, Size: size, FromCache: false, Strategy: "parallel"}, nil
}

func (e *Engine) tryAuthRefresh(ctx context.Context, opt *Options) error {
	if opt.OnAuthFail == nil {
		return fail(CodeAuthFailed, "no auth refresh hook", ClassAuthPermanent, errUpstreamAuth)
	}
	if opt.Headers == nil {
		opt.Headers = http.Header{}
	}
	if err := opt.OnAuthFail(ctx, opt.Headers); err != nil {
		return fail(CodeAuthFailed, err.Error(), ClassAuthPermanent, err)
	}
	return nil
}
