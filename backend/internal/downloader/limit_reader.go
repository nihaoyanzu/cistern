package downloader

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/livehl/mirrorhub/internal/ratelimit"
	"github.com/livehl/mirrorhub/internal/traffic"
)

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
	ctx       context.Context
	r         io.Reader
	pl        *ratelimit.PlatformLimiter
	prefetch  bool
	idle      float64
	traffic   *traffic.Recorder
	allow     func() bool
	lease     *prefetchLease
	onBlocked func() // pause 首次进入时释 conn 等
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.prefetch && l.pl != nil {
		held := false
		blockedOnce := false
		for {
			if !l.pl.PrefetchPaused() && (l.allow == nil || l.allow()) {
				break
			}
			held = true
			if !blockedOnce {
				blockedOnce = true
				if l.onBlocked != nil {
					l.onBlocked()
				}
			}
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
