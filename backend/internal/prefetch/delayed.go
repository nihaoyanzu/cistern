package prefetch

import (
	"context"
	"time"
)

type delayedItem struct {
	url      string
	platform string
	attempt  int
	after    time.Time
}

// waitingItem 因 Host 文件槽满而排队；不消耗 delayedBackoff attempt。
type waitingItem struct {
	url      string
	platform string
	attempt  int
}

var delayedBackoff = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second, 90 * time.Second, 180 * time.Second}

func (s *Service) enqueueDelayed(rawURL, platform string, attempt int) {
	if attempt >= len(delayedBackoff) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 延迟重试优先：从等待队列剔除同 URL
	keptW := s.waiting[:0]
	for _, w := range s.waiting {
		if w.url != rawURL {
			keptW = append(keptW, w)
		}
	}
	s.waiting = keptW
	for _, d := range s.delayed {
		if d.url == rawURL {
			return
		}
	}
	s.delayed = append(s.delayed, delayedItem{
		url:      rawURL,
		platform: platform,
		attempt:  attempt,
		after:    time.Now().Add(delayedBackoff[attempt]),
	})
}

func (s *Service) enqueueWaiting(rawURL, platform string, attempt int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.delayed {
		if d.url == rawURL {
			return
		}
	}
	for _, w := range s.waiting {
		if w.url == rawURL {
			return
		}
	}
	s.waiting = append(s.waiting, waitingItem{
		url:      rawURL,
		platform: platform,
		attempt:  attempt,
	})
}

func (s *Service) drainDelayed(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	var ready []delayedItem
	kept := s.delayed[:0]
	for _, d := range s.delayed {
		if !d.after.After(now) {
			ready = append(ready, d)
		} else {
			kept = append(kept, d)
		}
	}
	s.delayed = kept
	s.mu.Unlock()
	for _, d := range ready {
		s.startOneAttempt(ctx, d.url, d.platform, d.attempt)
	}
}

func (s *Service) drainWaiting(ctx context.Context) {
	s.mu.Lock()
	items := append([]waitingItem{}, s.waiting...)
	s.waiting = nil
	s.mu.Unlock()
	for _, w := range items {
		s.startOneAttempt(ctx, w.url, w.platform, w.attempt)
	}
}

// ClearDeferredMatching 清理延迟重试与文件槽等待队列（空 platform=全部）。
func (s *Service) ClearDeferredMatching(platform string) {
	platform = normalizePrefetchPlatform(platform)
	s.clearDeferredMatching(platform)
}

func (s *Service) clearDeferredMatching(platform string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearDelayedLocked(platform)
	s.clearWaitingLocked(platform)
}

func (s *Service) clearDelayedLocked(platform string) {
	if platform == "" {
		s.delayed = nil
		return
	}
	kept := s.delayed[:0]
	for _, d := range s.delayed {
		name := d.platform
		if name == "" {
			_, _, name = resolvePlatform(s.cfg.Get(), d.url, "")
		}
		if name != platform {
			kept = append(kept, d)
		}
	}
	s.delayed = kept
}

func (s *Service) clearWaitingLocked(platform string) {
	if platform == "" {
		s.waiting = nil
		return
	}
	kept := s.waiting[:0]
	for _, w := range s.waiting {
		name := w.platform
		if name == "" {
			_, _, name = resolvePlatform(s.cfg.Get(), w.url, "")
		}
		if name != platform {
			kept = append(kept, w)
		}
	}
	s.waiting = kept
}
