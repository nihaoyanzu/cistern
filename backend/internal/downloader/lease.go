package downloader

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nihaoyanzu/cistern/internal/ratelimit"
)

func (e *Engine) prefetchAllow(opt Options) func() bool {
	if !opt.Prefetch || e.prefetchGate == nil || opt.TaskID == "" {
		return nil
	}
	taskID := opt.TaskID
	gate := e.prefetchGate
	return func() bool { return gate(taskID) }
}

// prefetchLease 预取任务槽租约：pause/让路等待时释放槽，恢复后再占回，避免饿死 P0。
type prefetchLease struct {
	e           *Engine
	pl          *ratelimit.PlatformLimiter
	opt         Options
	mu          sync.Mutex
	held        bool
	acquiring   bool
	acquireWait chan struct{}
}

func newPrefetchLease(e *Engine, pl *ratelimit.PlatformLimiter, opt Options) *prefetchLease {
	if !opt.Prefetch || pl == nil {
		return nil
	}
	return &prefetchLease{e: e, pl: pl, opt: opt, held: true}
}

func (l *prefetchLease) release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		l.pl.ReleaseTask(l.opt.Boost)
		l.held = false
	}
}

func (l *prefetchLease) close() {
	l.release()
}

func (l *prefetchLease) ensureHeld(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for {
		l.mu.Lock()
		if l.held {
			l.mu.Unlock()
			return nil
		}
		if l.acquiring {
			ch := l.acquireWait
			l.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ch:
				continue
			}
		}
		l.acquiring = true
		l.acquireWait = make(chan struct{})
		l.mu.Unlock()

		err := l.e.acquireTaskSlot(ctx, l.pl, l.opt)

		l.mu.Lock()
		l.acquiring = false
		if err == nil {
			l.held = true
		}
		close(l.acquireWait)
		l.mu.Unlock()
		return err
	}
}

// waitPrefetchGate：P0 交互 pause 时挡住预取；等待期间通过 lease 释放任务槽。
// onBlocked 在首次进入等待时调用（用于释放连接槽，避免预取攥 conn 饿死交互）。
// held=true 表示曾被门闩挡住，上游连接不宜继续读。
func (e *Engine) waitPrefetchGate(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options, lease *prefetchLease, onBlocked func()) (held bool, err error) {
	if !opt.Prefetch || pl == nil {
		return false, nil
	}
	allow := e.prefetchAllow(opt)
	blockedOnce := false
	for {
		if !pl.PrefetchPaused() && (allow == nil || allow()) {
			if held {
				if err := lease.ensureHeld(ctx); err != nil {
					return true, err
				}
			}
			return held, nil
		}
		held = true
		if !blockedOnce {
			blockedOnce = true
			if onBlocked != nil {
				onBlocked()
			}
		}
		lease.release()
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// acquireTaskSlot 获取任务槽；预取在 pause / 让路期间不占槽，避免暂停间隙继续开跑、冲破并发体感。
func (e *Engine) acquireTaskSlot(ctx context.Context, pl *ratelimit.PlatformLimiter, opt Options) error {
	if pl == nil {
		return fmt.Errorf("no rate limiter")
	}
	if !opt.Prefetch {
		return pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch)
	}
	allow := e.prefetchAllow(opt)
	for {
		for {
			if !pl.PrefetchPaused() && (allow == nil || allow()) {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if err := pl.AcquireTask(ctx, opt.Boost, !opt.Prefetch); err != nil {
			return err
		}
		if !pl.PrefetchPaused() && (allow == nil || allow()) {
			return nil
		}
		pl.ReleaseTask(opt.Boost)
	}
}
