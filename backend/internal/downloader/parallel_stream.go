package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/livehl/mirrorhub/internal/cache"
	"github.com/livehl/mirrorhub/internal/ratelimit"
)

// streamParallelToClient 并行 Range 灌盘，同时按连续前缀边下边吐给客户端。
func (e *Engine) streamParallelToClient(ctx context.Context, w http.ResponseWriter, opt Options) (*cache.Entry, string, int64, error) {
	pl := e.limiters.Get(opt.Platform)
	if pl == nil {
		return nil, "", 0, fmt.Errorf("no rate limiter for platform %s", opt.Platform)
	}
	if err := pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch); err != nil {
		return nil, "", 0, err
	}
	defer pl.ReleaseTask(opt.Boost)
	notifyAcquired(opt)

	size := opt.KnownSize
	if size <= 0 {
		return nil, "", 0, fmt.Errorf("parallel stream requires known size")
	}
	ct := opt.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}

	concurrency := opt.Concurrency
	if concurrency <= 0 {
		concurrency = 8
	}
	chunkSize := opt.ChunkSize
	if chunkSize <= 0 {
		chunkSize = 5 * 1024 * 1024
	}

	tmp := e.cache.TempPath(opt.CacheKey + ".pstream")
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
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
	if err := f.Truncate(size); err != nil {
		return nil, ct, 0, err
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

	st := &fillState{
		completed: make(map[[2]int64]struct{}, len(chunks)),
	}
	st.cond = sync.NewCond(&st.mu)

	fillCtx, fillCancel := context.WithCancel(ctx)
	defer fillCancel()

	var downloaded atomic.Int64
	notifyProgress(opt, 0, size)

	g, gctx := errgroup.WithContext(fillCtx)
	g.SetLimit(concurrency)
	for _, ch := range chunks {
		ch := ch
		g.Go(func() error {
			if err := pl.AcquireConn(gctx, !opt.Prefetch); err != nil {
				return err
			}
			defer pl.ReleaseConn()
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				var got int64
				err := e.fetchChunk(gctx, pl, opt, nil, f, ch.start, ch.end, func(n int64) {
					got += n
					notifyProgress(opt, downloaded.Add(n), size)
				})
				if err != nil {
					if got > 0 {
						notifyProgress(opt, downloaded.Add(-got), size)
					}
					lastErr = err
					select {
					case <-gctx.Done():
						return gctx.Err()
					case <-time.After(time.Duration(1<<attempt) * 200 * time.Millisecond):
					}
					continue
				}
				st.mark(ch.start, ch.end)
				return nil
			}
			return lastErr
		})
	}

	fillErrCh := make(chan error, 1)
	go func() {
		err := g.Wait()
		st.finish(err)
		fillErrCh <- err
	}()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Mirrorhub-Strategy", "parallel-stream")
	if opt.Boost {
		w.Header().Set("X-Mirrorhub-Boost", "1")
	}
	if et := etagFor(opt, size); et != "" {
		w.Header().Set("ETag", et)
	}
	w.WriteHeader(http.StatusOK)
	flushWriter(w)

	written, streamErr := e.drainContiguous(ctx, w, f, st, size, pl, opt)
	fillCancel()
	fillErr := <-fillErrCh
	if streamErr != nil {
		return nil, ct, written, streamErr
	}
	if fillErr != nil {
		return nil, ct, written, fillErr
	}
	if written != size {
		return nil, ct, written, fmt.Errorf("incomplete parallel stream: got %d want %d", written, size)
	}
	if err := f.Sync(); err != nil {
		return nil, ct, written, err
	}
	_ = f.Close()

	if err := verifyAndRemember(tmp, opt); err != nil {
		return nil, ct, written, err
	}

	entry, err := e.cache.Put(opt.CacheKey, tmp, ct, opt.TTLSeconds, cache.Meta{
		SourceURL: opt.metaSource(),
		Kind:      opt.metaKind(),
		Digest:    opt.ExpectedSHA256,
	})
	if err != nil {
		return nil, ct, written, err
	}
	cleanupTmp = false
	_ = os.Remove(tmp)
	return entry, ct, written, nil
}

type fillState struct {
	mu         sync.Mutex
	cond       *sync.Cond
	completed  map[[2]int64]struct{}
	contiguous int64
	err        error
	done       bool
}

func (s *fillState) mark(start, end int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed[[2]int64{start, end}] = struct{}{}
	for {
		advanced := false
		for r := range s.completed {
			if r[0] == s.contiguous {
				s.contiguous = r[1] + 1
				delete(s.completed, r)
				advanced = true
				break
			}
		}
		if !advanced {
			break
		}
	}
	s.cond.Broadcast()
}

func (s *fillState) finish(err error) {
	s.mu.Lock()
	s.err = err
	s.done = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *fillState) waitContiguous(ctx context.Context, want int64) (contiguous int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.cond.Broadcast()
		case <-stop:
		}
	}()
	for s.contiguous < want && s.err == nil && !s.done {
		s.cond.Wait()
		if ctx.Err() != nil {
			return s.contiguous, ctx.Err()
		}
	}
	if s.contiguous < want {
		if s.err != nil {
			return s.contiguous, s.err
		}
		if s.done {
			return s.contiguous, fmt.Errorf("fill finished before contiguous prefix ready")
		}
	}
	return s.contiguous, nil
}

func (e *Engine) drainContiguous(ctx context.Context, w http.ResponseWriter, f *os.File, st *fillState, size int64, pl *ratelimit.PlatformLimiter, opt Options) (int64, error) {
	// stall watchdog：检测并行分片是否长时间没有推进连续前缀；
	// 若卡顿超过 streamStallTimeout 则取消 fill，返回错误让上层快速失败，
	// 避免 pip ReadTimeout（15s）导致连接断开。
	fillCtx, fillCancel := context.WithCancel(ctx)
	defer fillCancel()
	lastProgress := time.Now()
	stallDone := make(chan struct{})
	defer close(stallDone)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-stallDone:
				return
			case <-fillCtx.Done():
				return
			case <-t.C:
				if time.Since(lastProgress) >= streamStallTimeout {
					if e.log != nil {
						e.log.Warn("parallel-stream stall: contiguous prefix not advancing",
							zap.Int64("written", st.contiguous),
							zap.Int64("size", size),
						)
					}
					fillCancel()
					return
				}
			}
		}
	}()

	buf := make([]byte, 256*1024)
	var written int64
	var sinceFlush int
	flusher, canFlush := w.(http.Flusher)
	for written < size {
		cont, err := st.waitContiguous(fillCtx, written+1)
		if err != nil && cont <= written {
			return written, err
		}
		if cont <= written {
			if st.done {
				if st.err != nil {
					return written, st.err
				}
				break
			}
			continue
		}
		lastProgress = time.Now()
		toRead := cont - written
		for toRead > 0 {
			n := int(toRead)
			if n > len(buf) {
				n = len(buf)
			}
			nr, rerr := f.ReadAt(buf[:n], written)
			if nr > 0 {
				if _, err := w.Write(buf[:nr]); err != nil {
					return written, err
				}
				written += int64(nr)
				toRead -= int64(nr)
				sinceFlush += nr
				if canFlush && sinceFlush >= streamFlushEvery {
					flusher.Flush()
					sinceFlush = 0
				}
				if err := pl.WaitNClass(ctx, nr, ratelimit.ClassInteractive); err != nil {
					return written, err
				}
			}
			if rerr != nil && rerr != io.EOF {
				return written, rerr
			}
			if nr == 0 {
				break
			}
		}
	}
	flushWriter(w)
	return written, nil
}

func (opt Options) parallelThreshold() int64 {
	if opt.MinSize > 0 {
		return opt.MinSize
	}
	return 100 * 1024
}

func etagFor(opt Options, size int64) string {
	if opt.ExpectedSHA256 != "" {
		return `"` + opt.ExpectedSHA256 + `"`
	}
	if size >= 0 {
		return fmt.Sprintf(`W/"%d"`, size)
	}
	return ""
}
