package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/nihaoyanzu/cistern/internal/ratelimit"
)

func (e *Engine) fetchChunk(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, f *os.File, start, end, knownSize int64, identityETag string, authCanRetry bool, onBlocked func(), onBytes func(int64)) error {
	want := end - start + 1
	if want <= 0 {
		return fmt.Errorf("invalid chunk range %d-%d", start, end)
	}
	headers := cloneHeader(opt.Headers)
	headers.Del("Accept-Encoding")
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := e.doGET(ctx, http.MethodGet, opt.URL, headers, 3)
	if err != nil {
		return wrapClassified(err, authCanRetry)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return statusError(resp.StatusCode, resp.Header, authCanRetry)
	}
	if identityETag != "" {
		if got := resp.Header.Get("ETag"); got != "" && got != identityETag {
			return &FailError{Code: CodeContentChanged, Msg: "etag changed", Class: ClassContentChanged, Cause: errContentChanged}
		}
	}
	if resp.StatusCode == http.StatusOK && start != 0 {
		// 期望 206；回 200 且总长变化 → 内容变更
		if knownSize > 0 && resp.ContentLength > 0 && resp.ContentLength != knownSize {
			return &FailError{Code: CodeContentChanged, Msg: "size changed on resume", Class: ClassContentChanged, Cause: errContentChanged}
		}
		return fmt.Errorf("chunk ignored Range (got 200 for start=%d)", start)
	}
	if resp.StatusCode != http.StatusPartialContent && !(resp.StatusCode == http.StatusOK && start == 0) {
		return statusError(resp.StatusCode, resp.Header, authCanRetry)
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 && knownSize > 0 && total != knownSize {
		return &FailError{Code: CodeContentChanged, Msg: fmt.Sprintf("content-range total %d != known %d", total, knownSize), Class: ClassContentChanged, Cause: errContentChanged}
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
	buf := make([]byte, 32*1024)
	offset := start
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			lastProgress.Store(time.Now().UnixNano())
			idle := 0.3
			if e.idleRatio != nil {
				idle = e.idleRatio()
			}
			if opt.Prefetch {
				held, gateErr := e.waitPrefetchGate(ctx, pl, opt, lease, onBlocked)
				if gateErr != nil {
					return gateErr
				}
				if held {
					return errPrefetchReconnect
				}
				if err := pl.WaitPrefetch(ctx, n, idle); err != nil {
					return err
				}
			} else if err := pl.WaitNClass(ctx, n, ratelimit.ClassInteractive); err != nil {
				return err
			}
			if e.traffic != nil {
				e.traffic.AddDownload(int64(n))
			}
			if _, err := f.WriteAt(buf[:n], offset); err != nil {
				return err
			}
			offset += int64(n)
			if onBytes != nil {
				onBytes(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, errUpstreamStall) {
				return errUpstreamStall
			}
			return wrapClassified(readErr, authCanRetry)
		}
	}
	got := offset - start
	if got != want {
		return fmt.Errorf("chunk short read: got %d want %d (range %d-%d)", got, want, start, end)
	}
	return nil
}
